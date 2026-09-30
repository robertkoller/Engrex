package cache

import (
	"fmt"
	"hash/fnv"
	"math"
	"path/filepath"
	"testing"
	"time"
)

// fakeEmbedder returns a registered vector when it has one and a deterministic
// near-orthogonal vector otherwise. Registering lets a test say "these two questions are
// 98% similar" without needing a real model.
type fakeEmbedder struct {
	vectors map[string][]float32
}

func newFakeEmbedder() *fakeEmbedder {
	return &fakeEmbedder{vectors: make(map[string][]float32)}
}

func (embedder *fakeEmbedder) EmbedQuery(text string) ([]float32, error) {
	if vector, registered := embedder.vectors[text]; registered {
		return vector, nil
	}
	hash := fnv.New64a()
	hash.Write([]byte(text)) //nolint:errcheck
	return unitVector(int64(hash.Sum64()%1_000_000), 768), nil
}

// relate registers second as a vector sitting at the requested cosine similarity to
// first's, which is how a test creates a paraphrase of a known closeness.
func (embedder *fakeEmbedder) relate(first, second string, similarity float64) {
	base, _ := embedder.EmbedQuery(first)
	embedder.vectors[first] = base

	off := unitVector(9999, len(base))
	blended := make([]float32, len(base))
	weight := math.Sqrt(1 - similarity*similarity)
	for position := range base {
		blended[position] = float32(similarity*float64(base[position]) + weight*float64(off[position]))
	}

	var sumOfSquares float64
	for _, component := range blended {
		sumOfSquares += float64(component) * float64(component)
	}
	magnitude := math.Sqrt(sumOfSquares)
	for position := range blended {
		blended[position] /= float32(magnitude)
	}
	embedder.vectors[second] = blended
}

func newTestCache(t *testing.T, embedder Embedder, adjust func(*Options)) *Cache {
	t.Helper()
	options := Options{Embedder: embedder, Policy: DefaultPolicy()}
	if adjust != nil {
		adjust(&options)
	}
	cache, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return cache
}

func answerRequest(question string, passages ...string) Request {
	return Request{
		Endpoint: "/api/generate",
		Model:    "llama3.2",
		Text:     answerPrompt(question, passages...),
		Options:  map[string]any{"temperature": 0, "num_predict": 400},
	}
}

func storeAnswer(t *testing.T, cache *Cache, request Request, text string) {
	t.Helper()
	result, err := cache.Lookup(request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status == StatusHit {
		t.Fatal("expected a miss before anything was stored")
	}
	if _, err := cache.Store(result, Payload{Text: text}, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestExactRepeatIsAHit(t *testing.T) {
	cache := newTestCache(t, newFakeEmbedder(), nil)
	request := answerRequest("what is the dedup threshold?", "Deduplication uses 0.451.")
	storeAnswer(t, cache, request, "0.451")

	result, err := cache.Lookup(request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusHit {
		t.Fatalf("status = %q, want a hit", result.Status)
	}
	if result.Tier != TierExact {
		t.Errorf("tier = %q, want %q", result.Tier, TierExact)
	}
	if result.Entry.Payload.Text != "0.451" {
		t.Errorf("served %q", result.Entry.Payload.Text)
	}
}

// The property the whole design exists for. An answer generated from one set of passages
// must never be served for a prompt built from a different set, however similar the
// question is — here it is not merely similar, it is the identical question.
func TestAnswerIsNeverServedForDifferentContext(t *testing.T) {
	cache := newTestCache(t, newFakeEmbedder(), nil)
	question := "what threshold is used?"

	storeAnswer(t, cache, answerRequest(question, "Deduplication uses 0.451."), "0.451")

	result, err := cache.Lookup(answerRequest(question, "Edge building uses 0.95."))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status == StatusHit {
		t.Fatalf("served an answer built from different passages: %q", result.Entry.Payload.Text)
	}
}

// The other half: over identical passages, a reworded question should hit, and an
// unrelated one should not.
func TestParaphraseHitsWithinANamespace(t *testing.T) {
	passage := "Deduplication uses a cosine distance of 0.451."
	original := "what distance threshold does engrex use for deduplication?"
	paraphrase := "what dedup distance threshold does engrex use?"
	unrelated := "how does the file watcher debounce events?"

	embedder := newFakeEmbedder()
	embedder.relate(original, paraphrase, 0.99)
	embedder.relate(original, unrelated, 0.40)

	cache := newTestCache(t, embedder, nil)
	storeAnswer(t, cache, answerRequest(original, passage), "0.451")

	result, err := cache.Lookup(answerRequest(paraphrase, passage))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusHit {
		t.Fatalf("the paraphrase missed at similarity %.4f, threshold %.2f", result.Similarity, result.Threshold)
	}
	if result.Tier != TierNamespace {
		t.Errorf("tier = %q, want %q", result.Tier, TierNamespace)
	}

	result, err = cache.Lookup(answerRequest(unrelated, passage))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status == StatusHit {
		t.Errorf("an unrelated question hit at similarity %.4f", result.Similarity)
	}
}

// Serving a near neighbour's vector would put a subtly wrong direction into the index
// and quietly degrade every distance threshold calibrated against real model output. It
// has to be refused even when the inputs are all but identical.
func TestEmbeddingsAreNeverMatchedApproximately(t *testing.T) {
	first := "search_document: the daemon debounces for 500ms"
	second := "search_document: the daemon debounces for 500 ms"

	embedder := newFakeEmbedder()
	embedder.relate(first, second, 0.9999)

	cache := newTestCache(t, embedder, nil)
	request := Request{Endpoint: "/api/embed", Model: "nomic-embed-text", Text: first}
	result, err := cache.Lookup(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Store(result, Payload{Embedding: unitVector(1, 768)}, time.Millisecond); err != nil {
		t.Fatal(err)
	}

	// The same input again is free and correct.
	if again, _ := cache.Lookup(request); again.Status != StatusHit || again.Tier != TierExact {
		t.Errorf("an identical embedding request should hit exactly, got %q/%q", again.Status, again.Tier)
	}

	nearly := request
	nearly.Text = second
	result, err = cache.Lookup(nearly)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status == StatusHit {
		t.Fatal("an embedding was served for a different input")
	}
}

func TestVolatileQuestionsAreNotCached(t *testing.T) {
	cache := newTestCache(t, newFakeEmbedder(), nil)
	request := answerRequest("what did I save today?", "A note from this morning.")

	result, err := cache.Lookup(request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusBypass {
		t.Fatalf("status = %q, want %q", result.Status, StatusBypass)
	}

	entry, err := cache.Store(result, Payload{Text: "nothing"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if entry != nil {
		t.Error("a bypassed request was stored anyway")
	}
}

// A note containing the word "latest" must not make every question about that document
// uncacheable, which is what would happen if volatility were judged on the whole prompt.
func TestVolatilityIsJudgedOnTheQuestionNotThePassages(t *testing.T) {
	cache := newTestCache(t, newFakeEmbedder(), nil)
	request := answerRequest("what does the paper conclude?", "This is the latest revision, updated today.")

	result, err := cache.Lookup(request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status == StatusBypass {
		t.Error("a passage's wording made a stable question uncacheable")
	}
}

func TestExpiredEntriesAreEvicted(t *testing.T) {
	clock := time.Now()
	embedder := newFakeEmbedder()
	cache := newTestCache(t, embedder, func(options *Options) {
		options.Now = func() time.Time { return clock }
	})

	storeAnswer(t, cache, answerRequest("a question", "a passage"), "an answer")
	if cache.Len() != 1 {
		t.Fatalf("Len = %d, want 1", cache.Len())
	}

	clock = clock.Add(25 * time.Hour)
	result, err := cache.Lookup(answerRequest("a question", "a passage"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status == StatusHit {
		t.Error("served an entry past its TTL")
	}
}

func TestLeastRecentlyUsedEntriesAreEvictedAtCapacity(t *testing.T) {
	policy := DefaultPolicy()
	policy.MaxEntries = 5
	cache := newTestCache(t, newFakeEmbedder(), func(options *Options) {
		options.Policy = policy
	})

	for number := 0; number < 12; number++ {
		question := "question number " + string(rune('a'+number))
		storeAnswer(t, cache, answerRequest(question, "shared passage"), "answer")
	}
	if cache.Len() > policy.MaxEntries {
		t.Errorf("Len = %d, want at most %d", cache.Len(), policy.MaxEntries)
	}
}

func passageSet(swap int) []string {
	passages := []string{
		"Retrieval fuses vector and keyword hits using reciprocal rank fusion with k=60.",
		"The vector search returns twenty candidates before fusion.",
		"BM25 keyword search runs over the fts_chunks table.",
		"Reranking widens retrieval from twenty candidates to forty.",
		"The fused ranking is truncated to the final topK before the prompt is built.",
	}
	if swap > 0 {
		passages[len(passages)-swap] = "An entirely unrelated note about the Swift menu bar app and its hotkeys."
	}
	return passages
}

// Rewording a question usually changes which passages come back, often by one out of
// five. That breaks the exact-context match, which is the whole reason the tolerant tier
// exists — and the overlap guard is what keeps it from becoming unsafe.
func TestContextTolerantHitSurvivesOneSwappedPassage(t *testing.T) {
	original := "how does rank fusion work?"
	paraphrase := "how are the two rankings combined?"

	embedder := newFakeEmbedder()
	embedder.relate(original, paraphrase, 0.99)

	cache := newTestCache(t, embedder, func(options *Options) {
		options.ContextTolerant = true
	})
	storeAnswer(t, cache, answerRequest(original, passageSet(0)...), "RRF with k=60")

	result, err := cache.Lookup(answerRequest(paraphrase, passageSet(1)...))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusHit {
		t.Fatalf("a one-passage change missed at similarity %.4f", result.Similarity)
	}
	if result.Tier != TierTolerant {
		t.Errorf("tier = %q, want %q", result.Tier, TierTolerant)
	}
}

// The guard is the safety property: the same question over genuinely different material
// must still miss, or the tolerant tier is just a slower way of being wrong.
func TestContextTolerantHitRefusesUnrelatedPassages(t *testing.T) {
	original := "how does rank fusion work?"
	paraphrase := "how are the two rankings combined?"

	embedder := newFakeEmbedder()
	embedder.relate(original, paraphrase, 0.99)

	cache := newTestCache(t, embedder, func(options *Options) {
		options.ContextTolerant = true
	})
	storeAnswer(t, cache, answerRequest(original, passageSet(0)...), "RRF with k=60")

	unrelated := []string{
		"The Swift menu bar app registers a global hotkey through the Carbon API.",
		"Themes are stored in UserDefaults and applied at launch.",
		"File uploads are handled by a drop target on the query window.",
		"The graph view is a WKWebView pointed at localhost:7778.",
		"Accessibility permissions are tied to the bundle path.",
	}
	result, err := cache.Lookup(answerRequest(paraphrase, unrelated...))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status == StatusHit {
		t.Fatalf("served an answer grounded in unrelated passages (overlap guard failed)")
	}
}

// Off by default, like every other optional stage in this project.
func TestContextTolerantIsOffByDefault(t *testing.T) {
	original := "how does rank fusion work?"
	paraphrase := "how are the two rankings combined?"

	embedder := newFakeEmbedder()
	embedder.relate(original, paraphrase, 0.99)

	cache := newTestCache(t, embedder, nil)
	storeAnswer(t, cache, answerRequest(original, passageSet(0)...), "RRF with k=60")

	result, err := cache.Lookup(answerRequest(paraphrase, passageSet(1)...))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status == StatusHit {
		t.Error("the tolerant tier fired without being enabled")
	}
}

// An embedding request is exact-match only, so nothing ever embeds its semantic part and
// the entry carries no vector. It still has to be storable: for a long time it was handed
// to the vector index anyway, which seeded the graph with a zero-length vector and then
// panicked on the next real insert that tried to measure a distance against it.
//
// This is the ordinary Engrex sequence — a query embeds, then generates — so it took down
// the handler on essentially any real use.
func TestVectorlessEntryDoesNotBreakTheIndex(t *testing.T) {
	cache := newTestCache(t, newFakeEmbedder(), nil)

	embedRequest := Request{Endpoint: "/api/embed", Model: "nomic-embed-text", Text: "search_document: a chunk of text"}
	embedResult, err := cache.Lookup(embedRequest)
	if err != nil {
		t.Fatal(err)
	}
	if embedResult.Embedding != nil {
		t.Fatal("an embed lookup should never embed its own input")
	}
	if _, err := cache.Store(embedResult, Payload{Embedding: unitVector(3, 768)}, time.Millisecond); err != nil {
		t.Fatalf("storing a vectorless entry: %v", err)
	}

	// The insert that used to panic.
	storeAnswer(t, cache, answerRequest("what does the chunk say?", "a chunk of text"), "it says something")

	// Both remain retrievable by their own route.
	if again, _ := cache.Lookup(embedRequest); again.Status != StatusHit {
		t.Error("the stored embedding was not served for an identical request")
	}
	if again, _ := cache.Lookup(answerRequest("what does the chunk say?", "a chunk of text")); again.Status != StatusHit {
		t.Error("the stored answer was not served for an identical request")
	}
}

func ingestChunk(t *testing.T, cache *Cache, text string) {
	t.Helper()
	request := Request{Endpoint: embedEndpoint, Model: "nomic-embed-text", Text: "search_document: " + text}
	result, err := cache.Lookup(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Store(result, Payload{Embedding: unitVector(3, 768)}, time.Millisecond); err != nil {
		t.Fatal(err)
	}
}

// The strict tiers key on the passages byte for byte, so a new note changes the prompt
// before it could make a strict answer stale. Only the tolerant tier can reuse an answer
// across different passages, so only it needs answers dropped on ingest
func TestIngestDropsAnswersOnlyWhenTolerantMatchingIsOn(t *testing.T) {
	for _, tolerant := range []bool{false, true} {
		cache := newTestCache(t, newFakeEmbedder(), func(options *Options) {
			options.ContextTolerant = tolerant
		})
		storeAnswer(t, cache, answerRequest("what is hnsw?", "passage one"), "a graph index")
		ingestChunk(t, cache, "a brand new note")

		result, err := cache.Lookup(answerRequest("what is hnsw?", "passage one"))
		if err != nil {
			t.Fatal(err)
		}
		wantHit := !tolerant
		if (result.Status == StatusHit) != wantHit {
			t.Errorf("tolerant=%v: after ingest the answer lookup was %q", tolerant, result.Status)
		}
	}
}

// Two identical misses can both finish and store. The older copy should go, not sit in
// the index unreachable until LRU finds it
func TestStoringTheSameRequestTwiceKeepsOneEntry(t *testing.T) {
	cache := newTestCache(t, newFakeEmbedder(), nil)
	request := answerRequest("what is hnsw?", "passage one")

	first, _ := cache.Lookup(request)
	second, _ := cache.Lookup(request)
	cache.Store(first, Payload{Text: "first"}, time.Second)   //nolint:errcheck
	cache.Store(second, Payload{Text: "second"}, time.Second) //nolint:errcheck

	if cache.Len() != 1 {
		t.Errorf("holding %d entries for one request, want 1", cache.Len())
	}
	result, _ := cache.Lookup(request)
	if result.Status != StatusHit || result.Entry.Payload.Text != "second" {
		t.Errorf("served %q, want the newer answer", result.Entry.Payload.Text)
	}
}

// The journal is append-only, so without compaction it grows with every store and every
// eviction forever. It should be rewritten once dead history dominates, and a restart
// should come back with exactly the live entries
func TestJournalIsCompactedAndSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "entries.jsonl")
	journal, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}

	policy := DefaultPolicy()
	policy.MaxEntries = 50
	cache := newTestCache(t, newFakeEmbedder(), func(options *Options) {
		options.Journal = journal
		options.Policy = policy
	})
	for question := range 3000 {
		storeAnswer(t, cache, answerRequest(fmt.Sprintf("question %d", question), "passage"), "answer")
	}
	cache.Invalidate(InvalidateCriteria{Class: ClassAnswer})
	storeAnswer(t, cache, answerRequest("the survivor", "passage"), "kept")

	if records := journal.Records(); records > 2*policy.MaxEntries+compactSlack+10 {
		t.Errorf("journal holds %d records for at most %d live entries, so it was never compacted", records, policy.MaxEntries)
	}
	journal.Close() //nolint:errcheck

	reopened, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close() //nolint:errcheck
	restarted := newTestCache(t, newFakeEmbedder(), func(options *Options) {
		options.Journal = reopened
		options.Policy = policy
	})
	if restarted.Len() != 1 {
		t.Fatalf("restart loaded %d entries, want only the survivor; invalidated entries came back", restarted.Len())
	}
	result, _ := restarted.Lookup(answerRequest("the survivor", "passage"))
	if result.Status != StatusHit {
		t.Error("the surviving entry was not served after a restart")
	}
}
