package hnsw

import (
	"math"
	"math/rand"
	"path/filepath"
	"testing"
)

// randomUnitVectors generates normalized vectors, matching what Engrex stores — cosine
// distance is only well behaved on unit vectors.
func randomUnitVectors(count, dimensions int, seed int64) [][]float32 {
	random := rand.New(rand.NewSource(seed))
	vectors := make([][]float32, count)
	for index := range vectors {
		vector := make([]float32, dimensions)
		var sumOfSquares float64
		for position := range vector {
			value := random.NormFloat64()
			vector[position] = float32(value)
			sumOfSquares += value * value
		}
		magnitude := float32(math.Sqrt(sumOfSquares))
		for position := range vector {
			vector[position] /= magnitude
		}
		vectors[index] = vector
	}
	return vectors
}

// exactNearest is the ground truth the approximate index is measured against.
func exactNearest(vectors [][]float32, query []float32, k int) []int64 {
	type scored struct {
		id       int64
		distance float64
	}
	all := make([]scored, len(vectors))
	for index, vector := range vectors {
		all[index] = scored{id: int64(index), distance: distance(query, vector)}
	}
	for outer := 1; outer < len(all); outer++ {
		item := all[outer]
		inner := outer - 1
		for inner >= 0 && all[inner].distance > item.distance {
			all[inner+1] = all[inner]
			inner--
		}
		all[inner+1] = item
	}
	if len(all) > k {
		all = all[:k]
	}
	ids := make([]int64, len(all))
	for index, item := range all {
		ids[index] = item.id
	}
	return ids
}

func buildIndex(t *testing.T, vectors [][]float32, params Params) *Index {
	t.Helper()
	index := New(params)
	for id, vector := range vectors {
		if err := index.Add(int64(id), vector); err != nil {
			t.Fatalf("Add(%d): %v", id, err)
		}
	}
	return index
}

// recallAt reports the fraction of the true k nearest that the index actually returned.
func recallAt(t *testing.T, index *Index, vectors [][]float32, queries [][]float32, k int) float64 {
	t.Helper()
	total := 0.0
	for _, query := range queries {
		want := exactNearest(vectors, query, k)
		got, _, err := index.Search(query, k)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		found := make(map[int64]bool, len(got))
		for _, id := range got {
			found[id] = true
		}
		hits := 0
		for _, id := range want {
			if found[id] {
				hits++
			}
		}
		total += float64(hits) / float64(len(want))
	}
	return total / float64(len(queries))
}

// The core guarantee: HNSW is approximate, but at sane parameters it should recover
// nearly all of the true nearest neighbors. A collapse here means the graph is
// disconnected or the search is terminating early.
func TestRecallAgainstExactSearch(t *testing.T) {
	vectors := randomUnitVectors(2000, 64, 1)
	queries := randomUnitVectors(50, 64, 2)

	index := buildIndex(t, vectors, Params{M: 16, EfConstruction: 200, EfSearch: 64, Seed: 42})

	recall := recallAt(t, index, vectors, queries, 10)
	if recall < 0.90 {
		t.Errorf("recall@10 = %.3f, want >= 0.90 — the graph is losing neighbors", recall)
	}
	t.Logf("recall@10 = %.3f", recall)
}

// efSearch is the recall/latency dial, so recall must actually respond to it. If this
// fails, the parameter is not reaching the search.
func TestRecallImprovesWithEfSearch(t *testing.T) {
	vectors := randomUnitVectors(2000, 64, 3)
	queries := randomUnitVectors(50, 64, 4)

	index := buildIndex(t, vectors, Params{M: 8, EfConstruction: 100, EfSearch: 10, Seed: 42})
	low := recallAt(t, index, vectors, queries, 10)

	index.SetEfSearch(200)
	high := recallAt(t, index, vectors, queries, 10)

	t.Logf("recall@10: efSearch=10 -> %.3f, efSearch=200 -> %.3f", low, high)
	if high < low {
		t.Errorf("raising efSearch reduced recall (%.3f -> %.3f)", low, high)
	}
	if high < 0.90 {
		t.Errorf("recall at efSearch=200 is only %.3f", high)
	}
}

// A query that is itself an indexed vector must return that vector first — the easiest
// possible case, and a fast signal that the graph is navigable at all.
func TestSearchFindsExactMatch(t *testing.T) {
	vectors := randomUnitVectors(500, 32, 5)
	index := buildIndex(t, vectors, DefaultParams())

	for _, id := range []int{0, 100, 499} {
		got, distances, err := index.Search(vectors[id], 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d results, want 1", len(got))
		}
		if got[0] != int64(id) {
			t.Errorf("query for vector %d returned %d", id, got[0])
		}
		if distances[0] > 1e-6 {
			t.Errorf("distance to self = %v, want ~0", distances[0])
		}
	}
}

func TestSearchOnEmptyIndex(t *testing.T) {
	index := New(DefaultParams())
	ids, distances, err := index.Search(make([]float32, 8), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 || len(distances) != 0 {
		t.Errorf("empty index returned %d results", len(ids))
	}
}

func TestDimensionMismatchIsRejected(t *testing.T) {
	index := New(DefaultParams())
	if err := index.Add(1, make([]float32, 16)); err != nil {
		t.Fatal(err)
	}
	if err := index.Add(2, make([]float32, 8)); err != ErrDimensionMismatch {
		t.Errorf("Add with wrong dimensions: %v, want ErrDimensionMismatch", err)
	}
	if _, _, err := index.Search(make([]float32, 8), 1); err != ErrDimensionMismatch {
		t.Errorf("Search with wrong dimensions: %v, want ErrDimensionMismatch", err)
	}
}

// Re-adding an id must replace the vector, not leave neighbors pointing at the old one.
func TestAddReplacesExistingID(t *testing.T) {
	vectors := randomUnitVectors(200, 16, 6)
	index := buildIndex(t, vectors, DefaultParams())
	before := index.Len()

	replacement := randomUnitVectors(1, 16, 99)[0]
	if err := index.Add(50, replacement); err != nil {
		t.Fatal(err)
	}
	if index.Len() != before {
		t.Errorf("Len changed from %d to %d on replace", before, index.Len())
	}

	got, distances, err := index.Search(replacement, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != 50 || distances[0] > 1e-6 {
		t.Errorf("replaced vector not found: id=%d distance=%v", got[0], distances[0])
	}
}

// Every node must be reachable from the entry point at layer 0, or searches starting
// from that entry point can never find the unreachable ones regardless of efSearch.
func TestGraphIsConnectedAtBottomLayer(t *testing.T) {
	vectors := randomUnitVectors(1000, 32, 7)
	index := buildIndex(t, vectors, DefaultParams())

	seen := map[int64]bool{index.entryPoint: true}
	queue := []int64{index.entryPoint}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		node := index.nodes[current]
		if len(node.neighbors) == 0 {
			continue
		}
		for _, neighborID := range node.neighbors[0] {
			if !seen[neighborID] {
				seen[neighborID] = true
				queue = append(queue, neighborID)
			}
		}
	}

	if len(seen) != len(vectors) {
		t.Errorf("only %d of %d nodes reachable at layer 0 — graph is fragmented",
			len(seen), len(vectors))
	}
}

// Degree budgets must hold after pruning, or memory grows without bound.
func TestNeighborDegreeStaysBounded(t *testing.T) {
	params := Params{M: 8, EfConstruction: 100, EfSearch: 50, Seed: 42}
	vectors := randomUnitVectors(1000, 32, 8)
	index := buildIndex(t, vectors, params)

	for id, node := range index.nodes {
		for layer, neighbors := range node.neighbors {
			budget := params.M
			if layer == 0 {
				budget = params.M * 2
			}
			if len(neighbors) > budget {
				t.Errorf("node %d layer %d has %d neighbors, budget %d",
					id, layer, len(neighbors), budget)
			}
		}
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	vectors := randomUnitVectors(500, 32, 9)
	queries := randomUnitVectors(20, 32, 10)
	index := buildIndex(t, vectors, DefaultParams())

	path := filepath.Join(t.TempDir(), "index.hnsw")
	if err := index.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Len() != index.Len() {
		t.Errorf("loaded %d nodes, saved %d", loaded.Len(), index.Len())
	}

	// A reloaded index must answer identically, not merely load without error.
	for _, query := range queries {
		before, _, err := index.Search(query, 10)
		if err != nil {
			t.Fatal(err)
		}
		after, _, err := loaded.Search(query, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(before) != len(after) {
			t.Fatalf("result count changed across save/load: %d -> %d", len(before), len(after))
		}
		for position := range before {
			if before[position] != after[position] {
				t.Errorf("result %d changed across save/load: %d -> %d",
					position, before[position], after[position])
			}
		}
	}
}

func TestDefaultsFillInZeroParams(t *testing.T) {
	index := New(Params{})
	params := index.Params()
	if params.M == 0 || params.EfConstruction == 0 || params.EfSearch == 0 {
		t.Errorf("zero params were not defaulted: %+v", params)
	}
}
