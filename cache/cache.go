package cache

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

// Embedder is what the cache needs from an embedding model.
//
// EmbedQuery on both sides rather than EmbedDocument on one: this is prompt-to-prompt
// similarity, which is symmetric, and nomic-embed-text is trained with a different task
// prefix for each side. Mixing them is silently wrong rather than an error.
type Embedder interface {
	EmbedQuery(text string) ([]float32, error)
}

// Status is what a lookup decided.
type Status string

const (
	StatusHit    Status = "hit"
	StatusMiss   Status = "miss"
	StatusBypass Status = "bypass"
)

// Tier names how a hit was found, which is worth reporting separately: an exact tier hit
// is free, and a context-tolerant one is the only kind that can be wrong.
type Tier string

const (
	TierExact     Tier = "exact"
	TierNamespace Tier = "namespace"
	TierTolerant  Tier = "context-tolerant"
)

// Result is the outcome of a lookup, and everything Store needs afterwards so a miss
// does not have to redo the split or the embedding.
type Result struct {
	Status Status
	Tier   Tier
	Entry  *Entry

	// Similarity is the best score seen, whether or not it cleared the threshold. Below
	// the threshold it is the near-miss score the tuner works from.
	Similarity float64
	Threshold  float64

	Class     Class
	Namespace string
	Parts     Parts
	Embedding Vector
	Request   Request
}

// Recorder is told about every lookup and every store. The metrics and the near-miss log
// are both implemented on top of it, and a nil Recorder is fine.
type Recorder interface {
	RecordLookup(Result)
	RecordStore(*Entry)
	RecordEviction(count int)
}

// Options configure a Cache.
type Options struct {
	Embedder Embedder
	Journal  *Journal
	Splitter Splitter
	Policy   Policy
	Recorder Recorder

	// ContextTolerant turns on the second lookup tier: matching across namespaces when
	// the retrieved passages are similar enough. Off by default, matching how every
	// other optional stage in this project ships.
	ContextTolerant bool

	// MinContextOverlap is how much of the context two prompts must share for a
	// context-tolerant hit. Ignored unless ContextTolerant is set.
	MinContextOverlap float64

	// InvalidateAnswersOnIngest drops cached answers whenever Engrex embeds new content,
	// because an answer is only as good as the notes behind it. Defaults to on; see
	// noteCorpusChange. Turning it off trades freshness for hit rate.
	InvalidateAnswersOnIngest *bool

	// Now is injectable so tests can age entries without sleeping.
	Now func() time.Time
}

// Cache is the semantic cache: it decides whether a request has already been answered,
// and remembers the answers to the ones that had not.
type Cache struct {
	index    *Index
	journal  *Journal
	embedder Embedder
	splitter Splitter
	policy   Policy
	recorder Recorder

	contextTolerant    bool
	minContextOverlap  float64
	invalidateOnIngest bool
	now                func() time.Time

	// mutex guards byExactKey and usage, and serializes every change to the index
	// together with its journal record. The index has its own lock; this one exists so a
	// store or a removal is atomic across the index, the key map and the journal, which
	// is what keeps a put from landing in the journal after the delete that undid it.
	mutex      sync.Mutex
	byExactKey map[string]int64

	// usage is when each entry was last served and how often. Kept here rather than on
	// the Entry because entries are read without this lock by the index and the
	// journal, so an Entry is never written again once it is stored
	usage map[int64]*entryUsage

	// lastSweep is when expired entries were last looked for. Sweeping walks every
	// entry, so it runs on an interval rather than after every store
	lastSweep time.Time
}

type entryUsage struct {
	lastUsed time.Time
	hits     int
}

// sweepInterval is how often expired entries are cleared out. Expiry is still checked
// on every lookup, so an expired entry is never served in the meantime; this only
// bounds how long it takes up memory
const sweepInterval = time.Minute

// evictionHeadroom is the fraction of the cap cleared below it when the cap is hit. One
// sort then buys room for many stores, instead of sorting every entry on every store
// once the cache is full
const evictionHeadroom = 0.05

// compactSlack is how many dead journal records are tolerated beyond the live ones
// before the journal is rewritten, so a small cache is not compacted constantly
const compactSlack = 1000

// defaultMinContextOverlap is how much of the retrieved context has to be shared.
//
// Set from what the Jaccard actually works out to, not by feel. Swap one passage out of
// five and the two sets share four of six distinct passages, so the overlap is about
// 0.67 — measured at 0.72 on a real prompt, where the shared instruction preamble lifts
// it. Swap two and it falls to three of seven, about 0.43. So this sits between them: a
// single changed passage still hits, two do not.
const defaultMinContextOverlap = 0.65

// New builds a Cache and replays the journal into it.
func New(options Options) (*Cache, error) {
	if options.Embedder == nil {
		return nil, fmt.Errorf("cache: an embedder is required")
	}
	if options.Splitter == nil {
		options.Splitter = DefaultSplitter()
	}
	if options.Policy.Thresholds == nil {
		options.Policy = DefaultPolicy()
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.MinContextOverlap <= 0 {
		options.MinContextOverlap = defaultMinContextOverlap
	}
	if options.InvalidateAnswersOnIngest == nil {
		enabled := true
		options.InvalidateAnswersOnIngest = &enabled
	}

	cache := &Cache{
		index:              newIndex(options.ContextTolerant),
		journal:            options.Journal,
		embedder:           options.Embedder,
		splitter:           options.Splitter,
		policy:             options.Policy,
		recorder:           options.Recorder,
		contextTolerant:    options.ContextTolerant,
		minContextOverlap:  options.MinContextOverlap,
		invalidateOnIngest: *options.InvalidateAnswersOnIngest,
		now:                options.Now,
		byExactKey:         make(map[string]int64),
		usage:              make(map[int64]*entryUsage),
	}
	cache.lastSweep = cache.now()

	if options.Journal != nil {
		entries, records, err := replayJournal(options.Journal.path)
		if err != nil {
			return nil, fmt.Errorf("cache: replaying the journal: %w", err)
		}
		options.Journal.setRecords(records)
		for _, entry := range entries {
			if entry.Expired(cache.now()) {
				continue
			}
			if err := cache.index.Add(entry); err != nil {
				return nil, fmt.Errorf("cache: rebuilding the index: %w", err)
			}
			cache.byExactKey[entry.ExactKey] = entry.ID
			cache.usage[entry.ID] = &entryUsage{lastUsed: entry.LastUsedAt, hits: entry.HitCount}
		}

		// Expired entries and old history are dropped from the file here as well as from
		// memory, or the journal would only ever grow across restarts
		cache.mutex.Lock()
		err = cache.compactIfDueLocked()
		cache.mutex.Unlock()
		if err != nil {
			return nil, fmt.Errorf("cache: compacting the journal: %w", err)
		}
	}
	return cache, nil
}

// Len is how many entries the cache holds.
func (cache *Cache) Len() int { return cache.index.Len() }

// Policy returns the policy in force, for the dashboard and the tuner.
func (cache *Cache) Policy() Policy { return cache.policy }

// Lookup decides whether this request has already been answered.
//
// It tries three things in order, cheapest first: the same prompt seen before, a
// close-enough question asked over identical context, and — only when enabled — a
// close-enough question asked over similar context.
func (cache *Cache) Lookup(request Request) (Result, error) {
	var parts Parts
	if request.Endpoint == embedEndpoint {
		// An embedding request has no question inside it to isolate; the input is the
		// whole of what was asked for.
		parts = Parts{Semantic: request.Text, Class: ClassEmbed}
	} else {
		var recognized bool
		parts, recognized = cache.splitter.Split(request.Text)
		if !recognized {
			parts = Parts{Semantic: request.Text, Class: ClassOther}
		}
	}

	result := Result{
		Status:    StatusMiss,
		Class:     parts.Class,
		Parts:     parts,
		Request:   request,
		Threshold: cache.policy.Threshold(parts.Class),
	}

	if !cache.policy.Cacheable(parts.Class, parts.Semantic) {
		result.Status = StatusBypass
		cache.record(result)
		return result, nil
	}

	if entry, found := cache.exactMatch(ExactKey(request)); found {
		result.Status, result.Tier, result.Entry, result.Similarity = StatusHit, TierExact, entry, 1
		cache.record(result)
		return result, nil
	}

	if !cache.policy.ApproximateAllowed(parts.Class) {
		cache.record(result)
		return result, nil
	}

	vector, err := cache.embedder.EmbedQuery(parts.Semantic)
	if err != nil {
		return result, fmt.Errorf("cache: embedding the request: %w", err)
	}
	result.Embedding = vector
	result.Namespace = Namespace(request, parts.Context)

	if match, found := cache.index.SearchNamespace(result.Namespace, vector, cache.now()); found {
		result.Similarity = match.Similarity
		if match.Similarity >= result.Threshold {
			cache.touch(match.Entry)
			result.Status, result.Tier, result.Entry = StatusHit, TierNamespace, match.Entry
			cache.record(result)
			return result, nil
		}
	}

	if cache.contextTolerant {
		if match, found, err := cache.searchTolerant(parts, vector, result.Threshold); err != nil {
			return result, err
		} else if found {
			cache.touch(match.Entry)
			result.Status, result.Tier, result.Entry = StatusHit, TierTolerant, match.Entry
			result.Similarity = match.Similarity
			cache.record(result)
			return result, nil
		}
	}

	cache.record(result)
	return result, nil
}

// exactMatch finds a live entry stored under this exact key and marks it used
func (cache *Cache) exactMatch(exactKey string) (*Entry, bool) {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()

	id, seen := cache.byExactKey[exactKey]
	if !seen {
		return nil, false
	}
	entry, live := cache.index.Get(id)
	if !live || entry.Expired(cache.now()) {
		return nil, false
	}
	cache.touchLocked(entry.ID)
	return entry, true
}

// searchTolerant looks for the same question asked over similar, not identical, context.
//
// The similarity threshold alone is not enough here: it only compares the questions, and
// two identical questions over genuinely different documents would sail through it. The
// overlap guard is what makes this safe — the stored answer has to have been generated
// from mostly the same passages.
func (cache *Cache) searchTolerant(parts Parts, vector []float32, threshold float64) (Match, bool, error) {
	matches, err := cache.index.SearchGlobal(vector, parts.Class, tolerantCandidates, cache.now())
	if err != nil {
		return Match{}, false, err
	}

	sketch := Sketch(parts.Context)
	for _, match := range matches {
		if match.Similarity < threshold {
			continue
		}
		if Overlap(sketch, match.Entry.ContextSketch) < cache.minContextOverlap {
			continue
		}
		return match, true, nil
	}
	return Match{}, false, nil
}

// tolerantCandidates is how many neighbours to consider before the overlap guard. Small,
// because a question that is not among the nearest few is not the same question.
const tolerantCandidates = 5

// embedEndpoint is the one path whose body is an input rather than a prompt.
const embedEndpoint = "/api/embed"

// Store remembers what the provider returned for a request that missed.
func (cache *Cache) Store(result Result, payload Payload, generationTime time.Duration) (*Entry, error) {
	if result.Status == StatusBypass {
		return nil, nil
	}
	if !cache.policy.Cacheable(result.Class, result.Parts.Semantic) {
		return nil, nil
	}

	// A miss on a class that is never matched approximately never embedded anything,
	// since only the exact key is ever used to find it again.
	embedding := result.Embedding
	if embedding == nil && cache.policy.ApproximateAllowed(result.Class) {
		vector, err := cache.embedder.EmbedQuery(result.Parts.Semantic)
		if err != nil {
			return nil, fmt.Errorf("cache: embedding for storage: %w", err)
		}
		embedding = vector
	}

	namespace := result.Namespace
	if namespace == "" {
		namespace = Namespace(result.Request, result.Parts.Context)
	}

	semantic := result.Parts.Semantic
	if result.Class == ClassEmbed {
		// For an embedding the semantic part is the whole input, which Prompt already
		// holds. Storing it twice would double the text in memory and in the journal
		semantic = ""
	}

	now := cache.now()
	entry := &Entry{
		Namespace:      namespace,
		Class:          result.Class,
		Model:          result.Request.Model,
		Endpoint:       result.Request.Endpoint,
		Prompt:         result.Request.Text,
		Semantic:       semantic,
		Embedding:      embedding,
		Payload:        payload,
		GenerationTime: generationTime,
		CreatedAt:      now,
		LastUsedAt:     now,
		ExpiresAt:      now.Add(cache.policy.TTL(result.Class, result.Parts.Semantic)),
		ExactKey:       ExactKey(result.Request),
	}
	if cache.contextTolerant {
		entry.ContextSketch = Sketch(result.Parts.Context)
	}

	cache.mutex.Lock()
	removed := 0

	// Two identical misses can both finish and store. Keep the newer and retire the
	// older, rather than leaving it in the index where nothing can reach it by key
	if previous, exists := cache.byExactKey[entry.ExactKey]; exists {
		if stale, live := cache.index.Get(previous); live {
			cache.removeLocked([]*Entry{stale})
			removed++
		}
	}

	if err := cache.index.Add(entry); err != nil {
		cache.mutex.Unlock()
		return nil, err
	}
	cache.byExactKey[entry.ExactKey] = entry.ID
	cache.usage[entry.ID] = &entryUsage{lastUsed: now}

	var journalErr error
	if cache.journal != nil {
		journalErr = cache.journal.Put(entry)
	}

	removed += cache.noteCorpusChangeLocked(entry)
	removed += cache.evictLocked(now)
	if err := cache.compactIfDueLocked(); err != nil {
		log.Printf("cache: compacting the journal failed, will retry: %v", err)
	}
	cache.mutex.Unlock()

	if cache.recorder != nil {
		cache.recorder.RecordStore(entry)
		if removed > 0 {
			cache.recorder.RecordEviction(removed)
		}
	}
	if journalErr != nil {
		return entry, fmt.Errorf("cache: the entry is served from memory but was not journaled: %w", journalErr)
	}
	return entry, nil
}

// touchLocked records that an entry was served, which is what drives LRU eviction. The
// journal is deliberately not written here: a hit is the hot path, and a restart only
// loses recency since the last compaction
func (cache *Cache) touchLocked(id int64) {
	if usage, tracked := cache.usage[id]; tracked {
		usage.hits++
		usage.lastUsed = cache.now()
	}
}

func (cache *Cache) touch(entry *Entry) {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	cache.touchLocked(entry.ID)
}

// removeLocked drops entries from the index, the key map and usage, and journals each
// removal under the same lock as the change itself
func (cache *Cache) removeLocked(doomed []*Entry) {
	for _, entry := range doomed {
		if !cache.index.Remove(entry.ID) {
			continue
		}
		// Only clear the key if it still points here. A replaced entry's key already
		// belongs to its successor
		if cache.byExactKey[entry.ExactKey] == entry.ID {
			delete(cache.byExactKey, entry.ExactKey)
		}
		delete(cache.usage, entry.ID)
		if cache.journal != nil {
			cache.journal.Delete(entry.ID) //nolint:errcheck — eviction must not fail a request
		}
	}
}

// evictLocked drops expired entries, then the least recently used ones if the cache is
// still over its cap, and returns how many went.
//
// Walking every entry is only done when the cap is exceeded or a sweep is due. At the cap
// it clears a little headroom below it, so the sort is paid once per many stores rather
// than on each one
func (cache *Cache) evictLocked(now time.Time) int {
	overCap := cache.index.Len() > cache.policy.MaxEntries
	if !overCap && now.Sub(cache.lastSweep) < sweepInterval {
		return 0
	}
	cache.lastSweep = now

	var doomed []*Entry
	var live []*Entry
	for _, entry := range cache.index.Entries() {
		if entry.Expired(now) {
			doomed = append(doomed, entry)
		} else {
			live = append(live, entry)
		}
	}

	if surplus := len(live) - cache.policy.MaxEntries; surplus > 0 {
		surplus += int(float64(cache.policy.MaxEntries) * evictionHeadroom)
		surplus = min(surplus, len(live))
		sort.Slice(live, func(first, second int) bool {
			return cache.lastUsedLocked(live[first]).Before(cache.lastUsedLocked(live[second]))
		})
		doomed = append(doomed, live[:surplus]...)
	}
	cache.removeLocked(doomed)
	return len(doomed)
}

func (cache *Cache) lastUsedLocked(entry *Entry) time.Time {
	if usage, tracked := cache.usage[entry.ID]; tracked {
		return usage.lastUsed
	}
	return entry.LastUsedAt
}

// compactIfDueLocked rewrites the journal once dead records outnumber live ones by a
// margin. The rewrite carries each entry's current recency and hit count, so LRU order
// survives a restart as of the last compaction
func (cache *Cache) compactIfDueLocked() error {
	if cache.journal == nil {
		return nil
	}
	live := cache.index.Entries()
	if cache.journal.Records() <= 2*len(live)+compactSlack {
		return nil
	}

	sort.Slice(live, func(first, second int) bool { return live[first].ID < live[second].ID })
	snapshot := make([]*Entry, len(live))
	for position, entry := range live {
		copied := *entry
		if usage, tracked := cache.usage[entry.ID]; tracked {
			copied.LastUsedAt = usage.lastUsed
			copied.HitCount = usage.hits
		}
		snapshot[position] = &copied
	}
	return cache.journal.Compact(snapshot)
}

func (cache *Cache) record(result Result) {
	if cache.recorder != nil {
		cache.recorder.RecordLookup(result)
	}
}

// InvalidateCriteria selects entries to drop. An empty criteria matches nothing, so a
// malformed request cannot empty the cache by accident.
type InvalidateCriteria struct {
	// All drops everything. Has to be asked for explicitly.
	All bool `json:"all,omitempty"`

	// Model drops every entry generated by one model. This is the case that matters
	// most: upgrade the generation model and every stored answer came from the old one.
	Model string `json:"model,omitempty"`

	// Class drops one kind of call — what to use after editing a prompt builder, since
	// only that class's stored responses are stale.
	Class Class `json:"class,omitempty"`

	Tag string `json:"tag,omitempty"`
}

func (criteria InvalidateCriteria) matches(entry *Entry) bool {
	if criteria.All {
		return true
	}
	if criteria.Model != "" && entry.Model == criteria.Model {
		return true
	}
	if criteria.Class != "" && entry.Class == criteria.Class {
		return true
	}
	if criteria.Tag != "" && entry.HasTag(criteria.Tag) {
		return true
	}
	return false
}

// Invalidate drops every entry matching the criteria and returns how many went.
func (cache *Cache) Invalidate(criteria InvalidateCriteria) int {
	cache.mutex.Lock()
	removed := cache.invalidateLocked(criteria)
	cache.mutex.Unlock()

	if cache.recorder != nil && removed > 0 {
		cache.recorder.RecordEviction(removed)
	}
	return removed
}

func (cache *Cache) invalidateLocked(criteria InvalidateCriteria) int {
	var doomed []*Entry
	for _, entry := range cache.index.Entries() {
		if criteria.matches(entry) {
			doomed = append(doomed, entry)
		}
	}
	cache.removeLocked(doomed)
	return len(doomed)
}

// documentPrefix is the task prefix internal/embedder puts on text it is storing, as
// opposed to a question it is searching with. See internal/embedder/ollama.go:19 — this
// has to match, and it is duplicated rather than exported because the proxy is meant to
// work in front of Ollama generally, not only for Engrex.
const documentPrefix = "search_document: "

// noteCorpusChangeLocked drops cached answers when Engrex stores something new, and
// returns how many went.
//
// A cached answer is only as good as the notes it was built from, and this corpus moves:
// the watcher ingests whatever lands in ~/Engrex. The signal is free. Ingestion embeds
// each chunk with the document prefix, and that call comes through this proxy. A miss on
// one means the chunk's text was not seen before, so new or edited content. A hit means
// identical text, so nothing changed and nothing needs dropping.
//
// This only matters for the context-tolerant tier. The exact and namespace tiers key on
// the retrieved passages byte for byte, and Engrex retrieves before it generates, so once
// a new note changes what comes back the prompt changes and those tiers miss on their
// own. Dropping their answers too would only throw away hits that were still correct. A
// tolerant hit is different: it is allowed to reuse an answer whose passages differ by
// one, and that one could be exactly the new note.
//
// Only answers go: reranking, rewriting and verification are functions of the inputs they
// were handed, and a new note does not change what they computed.
func (cache *Cache) noteCorpusChangeLocked(entry *Entry) int {
	if !cache.invalidateOnIngest || !cache.contextTolerant || entry.Class != ClassEmbed {
		return 0
	}
	if !strings.HasPrefix(entry.Prompt, documentPrefix) {
		return 0
	}
	return cache.invalidateLocked(InvalidateCriteria{Class: ClassAnswer})
}
