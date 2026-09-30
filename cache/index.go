package cache

import (
	"sync"
	"time"

	"github.com/robertkoller/engrex/internal/hnsw"
)

// Match is one candidate the index returned, with how close it was.
type Match struct {
	Entry    *Entry
	Distance float64

	// Similarity is 1-Distance, which is the cosine itself. Thresholds are written in
	// these terms because that is how the literature and the guide state them.
	Similarity float64
}

// Index holds the cached entries and finds the nearest one to a request.
//
// It searches two different ways, because the two lookups have genuinely different
// shapes. Within a namespace it scans directly: a namespace is one exact context, and in
// practice that holds a handful of entries — twenty claims checked against one answer's
// passages, say — so a scan is both exact and faster than an approximate index. Across
// namespaces, which is what the context-tolerant lookup needs, it uses HNSW, because
// that search really is over every entry the cache holds.
type Index struct {
	mutex sync.RWMutex

	// graph is nil when nothing searches across namespaces. Only the context-tolerant
	// tier does, so a strict cache skips the HNSW insert on every store
	graph   *hnsw.Index
	entries map[int64]*Entry

	// byNamespace is the exact-bucket lookup. Ids only, so an entry lives in one place.
	byNamespace map[string][]int64

	// tombstones counts ids still in the graph that are no longer live. internal/hnsw
	// has no delete — its unlink path exists only to replace an id, and using it that
	// way leaves dangling edges — so eviction marks instead, and the graph is rebuilt
	// once enough of it is dead.
	tombstones int

	nextID int64
}

// rebuildFraction is how much of the graph may be dead before it gets rebuilt. A quarter
// keeps wasted search work bounded without rebuilding on every eviction.
const rebuildFraction = 0.25

func NewIndex() *Index {
	return newIndex(true)
}

func newIndex(withGraph bool) *Index {
	index := &Index{
		entries:     make(map[int64]*Entry),
		byNamespace: make(map[string][]int64),
		nextID:      1,
	}
	if withGraph {
		index.graph = hnsw.New(hnsw.DefaultParams())
	}
	return index
}

// Len is how many live entries the index holds.
func (index *Index) Len() int {
	index.mutex.RLock()
	defer index.mutex.RUnlock()
	return len(index.entries)
}

// Add stores an entry, assigning it an id if it does not have one.
//
// Ids start at 1 because internal/hnsw uses -1 as its "index is empty" sentinel, and 0
// is too easy to hand it by accident from a zero-valued struct.
//
// An entry with no embedding is held but kept out of the graph. Embedding requests are
// matched exactly and never by similarity, so nothing ever embeds their semantic part and
// there is no vector to index. Putting one in anyway seeds the graph with a zero-length
// vector, and the next real insert panics measuring a distance against it — which is the
// ordinary query sequence, an embed followed by a generate.
func (index *Index) Add(entry *Entry) error {
	index.mutex.Lock()
	defer index.mutex.Unlock()

	if entry.ID == 0 {
		entry.ID = index.nextID
	}
	if entry.ID >= index.nextID {
		index.nextID = entry.ID + 1
	}

	if index.graph != nil && len(entry.Embedding) > 0 {
		// hnsw.Add keeps the slice it is given rather than copying it, so a caller
		// reusing its embedding buffer would silently rewrite what is already indexed.
		vector := make([]float32, len(entry.Embedding))
		copy(vector, entry.Embedding)
		if err := index.graph.Add(entry.ID, vector); err != nil {
			return err
		}
	}

	index.entries[entry.ID] = entry

	// Only entries with a vector can be matched within a namespace. Every embedding of
	// one model shares a single namespace, so listing them would make each removal scan
	// all of them for nothing
	if len(entry.Embedding) > 0 {
		index.byNamespace[entry.Namespace] = append(index.byNamespace[entry.Namespace], entry.ID)
	}
	return nil
}

// Get returns a live entry by id.
func (index *Index) Get(id int64) (*Entry, bool) {
	index.mutex.RLock()
	defer index.mutex.RUnlock()
	entry, found := index.entries[id]
	return entry, found
}

// Remove drops an entry. The graph keeps its node until the next rebuild.
func (index *Index) Remove(id int64) bool {
	index.mutex.Lock()
	defer index.mutex.Unlock()
	return index.removeLocked(id)
}

func (index *Index) removeLocked(id int64) bool {
	entry, found := index.entries[id]
	if !found {
		return false
	}

	delete(index.entries, id)
	if len(entry.Embedding) == 0 {
		// Never in the graph or a namespace list, so there is nothing else to clean up.
		// Counting these as tombstones would trigger rebuilds over embedding entries,
		// which are most of the cache during ingestion and none of the graph
		return true
	}
	index.dropFromNamespaceLocked(entry)
	if index.graph == nil {
		return true
	}

	index.tombstones++
	if float64(index.tombstones) > rebuildFraction*float64(index.graph.Len()) {
		index.rebuildLocked()
	}
	return true
}

func (index *Index) dropFromNamespaceLocked(entry *Entry) {
	id := entry.ID
	remaining := index.byNamespace[entry.Namespace][:0]
	for _, candidate := range index.byNamespace[entry.Namespace] {
		if candidate != id {
			remaining = append(remaining, candidate)
		}
	}
	if len(remaining) == 0 {
		delete(index.byNamespace, entry.Namespace)
	} else {
		index.byNamespace[entry.Namespace] = remaining
	}
}

// SearchNamespace returns the closest live entry sharing the request's exact context.
// This is the lookup that runs on every request, and it is exact — no recall to lose.
func (index *Index) SearchNamespace(namespace string, vector []float32, now time.Time) (Match, bool) {
	index.mutex.RLock()
	defer index.mutex.RUnlock()

	best := Match{Distance: 2}
	found := false
	for _, id := range index.byNamespace[namespace] {
		entry := index.entries[id]
		if entry == nil || entry.Expired(now) || len(entry.Embedding) == 0 {
			continue
		}
		distance := cosineDistance(vector, entry.Embedding)
		if distance < best.Distance {
			best = Match{Entry: entry, Distance: distance, Similarity: 1 - distance}
			found = true
		}
	}
	return best, found
}

// SearchGlobal returns the nearest live entries of a class across every namespace. Only
// the context-tolerant lookup uses this, and its caller still has to check that the two
// prompts retrieved similar enough passages before serving anything it returns.
func (index *Index) SearchGlobal(vector []float32, class Class, limit int, now time.Time) ([]Match, error) {
	index.mutex.RLock()
	defer index.mutex.RUnlock()
	if index.graph == nil {
		return nil, nil
	}

	// Over-fetch: the graph knows nothing about class, expiry, or tombstones, so some of
	// what it returns will be filtered out here.
	ids, distances, err := index.graph.Search(vector, limit*searchOverfetch)
	if err != nil {
		return nil, err
	}

	matches := make([]Match, 0, limit)
	for position, id := range ids {
		entry, live := index.entries[id]
		if !live || entry.Class != class || entry.Expired(now) {
			continue
		}
		matches = append(matches, Match{
			Entry:      entry,
			Distance:   distances[position],
			Similarity: 1 - distances[position],
		})
		if len(matches) == limit {
			break
		}
	}
	return matches, nil
}

// searchOverfetch is how many extra candidates to pull so that filtering does not empty
// the result.
const searchOverfetch = 8

// Rebuild discards the graph and reinserts every live entry, clearing the tombstones.
func (index *Index) Rebuild() {
	index.mutex.Lock()
	defer index.mutex.Unlock()
	index.rebuildLocked()
}

func (index *Index) rebuildLocked() {
	if index.graph == nil {
		return
	}
	graph := hnsw.New(hnsw.DefaultParams())
	for id, entry := range index.entries {
		if len(entry.Embedding) == 0 {
			continue
		}
		vector := make([]float32, len(entry.Embedding))
		copy(vector, entry.Embedding)
		graph.Add(id, vector) //nolint:errcheck — every vector already passed Add once
	}
	index.graph = graph
	index.tombstones = 0
}

// Entries returns every live entry. The slice is fresh but the entries are shared, so
// callers may read them and must not mutate them.
func (index *Index) Entries() []*Entry {
	index.mutex.RLock()
	defer index.mutex.RUnlock()

	entries := make([]*Entry, 0, len(index.entries))
	for _, entry := range index.entries {
		entries = append(entries, entry)
	}
	return entries
}

// cosineDistance matches what internal/hnsw reports, so a namespace scan and a graph
// search return comparable numbers. Both rely on the embedder having normalized its
// output, which embedder.Normalize does on every vector it returns.
func cosineDistance(first, second []float32) float64 {
	if len(first) != len(second) {
		return 2
	}
	var dot float64
	for position := range first {
		dot += float64(first[position]) * float64(second[position])
	}
	return 1 - dot
}
