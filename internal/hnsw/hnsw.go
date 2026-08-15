// Package hnsw implements Hierarchical Navigable Small World graphs for approximate
// nearest-neighbor search, following Malkov & Yashunin (2016).
//
// The structure is a stack of proximity graphs. Layer 0 holds every vector; each layer
// above holds an exponentially thinning sample, so the top layers act as an express
// network that carries a search most of the way to its target in a few hops, and the
// dense bottom layer refines it. A search descends greedily from the top entry point,
// then explores layer 0 with a bounded candidate list.
//
// Read this alongside docs/hnsw.md, which reports where it actually beats brute force
// on this corpus — which, at Engrex's scale, it mostly does not.
package hnsw

import (
	"container/heap"
	"errors"
	"math"
	"math/rand"
	"sync"
)

// Params control the index's speed/recall/memory trade-off.
type Params struct {
	// M is the number of bidirectional links kept per node per layer above 0. Layer 0
	// keeps 2*M. Higher M means better recall and more memory. 16 is the paper's
	// general-purpose default.
	M int

	// EfConstruction is the candidate-list size used while inserting. Higher builds a
	// better graph and takes longer; it does not affect query cost.
	EfConstruction int

	// EfSearch is the candidate-list size used while querying, and the main
	// recall/latency dial at query time. It must be >= the k being requested.
	EfSearch int

	// Seed makes level assignment deterministic, which is what lets the benchmark
	// compare two configurations without the graph shape changing underneath it.
	Seed int64
}

// DefaultParams are the paper's general-purpose settings.
func DefaultParams() Params {
	return Params{M: 16, EfConstruction: 200, EfSearch: 64, Seed: 42}
}

// node is one vector and its neighbor lists, one per layer it appears in.
type node struct {
	id     int64
	vector []float32

	// neighbors[layer] holds the ids this node links to at that layer. Index 0 is the
	// bottom, densest layer.
	neighbors [][]int64
}

// Index is an HNSW graph. Safe for concurrent reads; writes take an exclusive lock.
type Index struct {
	mutex sync.RWMutex

	params Params
	random *rand.Rand

	nodes map[int64]*node

	// entryPoint is the id of the node at the highest layer — every search starts here.
	entryPoint int64
	maxLayer   int

	// levelMultiplier is 1/ln(M), the normalization that makes layer occupancy decay
	// exponentially so the expected number of layers stays logarithmic in the corpus.
	levelMultiplier float64

	dimensions int
}

var ErrDimensionMismatch = errors.New("hnsw: vector dimension does not match the index")

// New returns an empty index. Zero or negative parameters fall back to the defaults, so
// a partially filled Params is still usable.
func New(params Params) *Index {
	defaults := DefaultParams()
	if params.M <= 0 {
		params.M = defaults.M
	}
	if params.EfConstruction <= 0 {
		params.EfConstruction = defaults.EfConstruction
	}
	if params.EfSearch <= 0 {
		params.EfSearch = defaults.EfSearch
	}
	if params.Seed == 0 {
		params.Seed = defaults.Seed
	}

	return &Index{
		params:          params,
		random:          rand.New(rand.NewSource(params.Seed)),
		nodes:           make(map[int64]*node),
		entryPoint:      -1,
		maxLayer:        -1,
		levelMultiplier: 1 / math.Log(float64(params.M)),
	}
}

// Len returns how many vectors are indexed.
func (index *Index) Len() int {
	index.mutex.RLock()
	defer index.mutex.RUnlock()
	return len(index.nodes)
}

// Params returns the index's configuration.
func (index *Index) Params() Params {
	index.mutex.RLock()
	defer index.mutex.RUnlock()
	return index.params
}

// SetEfSearch adjusts the query-time candidate list. Exposed separately from the rest
// of Params because sweeping it against recall is the whole point of the benchmark.
func (index *Index) SetEfSearch(efSearch int) {
	index.mutex.Lock()
	defer index.mutex.Unlock()
	if efSearch > 0 {
		index.params.EfSearch = efSearch
	}
}

// distance returns cosine distance. Engrex normalizes every embedding to unit length,
// so the dot product is the cosine and 1-dot is the distance — matching what
// sqlite-vec's vec0 reports, which is what makes recall comparisons meaningful.
func distance(a, b []float32) float64 {
	var dot float64
	for index := range a {
		dot += float64(a[index]) * float64(b[index])
	}
	return 1 - dot
}

// randomLevel draws a node's top layer from an exponentially decaying distribution, so
// roughly 1/M of the nodes at each layer are promoted to the next one up.
func (index *Index) randomLevel() int {
	return int(-math.Log(index.random.Float64()) * index.levelMultiplier)
}

// Add inserts a vector.
//
// The insert walks down from the top: greedily descending the layers above the new
// node's own level to find a good entry point, then at each layer from its level down
// to 0 running a full candidate search, choosing neighbors with the selection
// heuristic, and linking bidirectionally — pruning any neighbor that exceeded its
// degree budget as a result.
func (index *Index) Add(id int64, vector []float32) error {
	index.mutex.Lock()
	defer index.mutex.Unlock()

	if index.dimensions == 0 {
		index.dimensions = len(vector)
	} else if len(vector) != index.dimensions {
		return ErrDimensionMismatch
	}

	if existing, found := index.nodes[id]; found {
		// Re-adding an id replaces it. Unlinking first keeps the graph consistent
		// rather than leaving neighbors pointing at the stale vector.
		index.unlink(existing)
		delete(index.nodes, id)
	}

	level := index.randomLevel()
	fresh := &node{
		id:        id,
		vector:    vector,
		neighbors: make([][]int64, level+1),
	}
	index.nodes[id] = fresh

	// First node ever: it becomes the entry point and there is nothing to link to.
	if index.entryPoint == -1 {
		index.entryPoint = id
		index.maxLayer = level
		return nil
	}

	current := index.entryPoint

	// Layers above the new node: pure greedy descent, one candidate at a time. The new
	// node isn't present up here, so there is nothing to link — this only refines the
	// entry point for the layers where it does exist.
	for layer := index.maxLayer; layer > level; layer-- {
		current = index.greedyClosest(vector, current, layer)
	}

	// Layers the new node belongs to: search properly, then link.
	for layer := min(level, index.maxLayer); layer >= 0; layer-- {
		candidates := index.searchLayer(vector, []int64{current}, index.params.EfConstruction, layer)

		degree := index.params.M
		if layer == 0 {
			degree = index.params.M * 2 // the bottom layer carries the most traffic
		}

		selected := index.selectNeighbors(vector, candidates, degree)
		fresh.neighbors[layer] = selected

		for _, neighborID := range selected {
			neighbor := index.nodes[neighborID]
			neighbor.neighbors[layer] = append(neighbor.neighbors[layer], id)

			// Linking back can push a neighbor over its budget, so re-select its links
			// rather than letting degree grow without bound.
			if len(neighbor.neighbors[layer]) > degree {
				pruned := index.selectNeighbors(
					neighbor.vector,
					index.toCandidates(neighbor.vector, neighbor.neighbors[layer]),
					degree)
				neighbor.neighbors[layer] = pruned
			}
		}

		if len(candidates) > 0 {
			current = candidates[0].id
		}
	}

	if level > index.maxLayer {
		index.maxLayer = level
		index.entryPoint = id
	}
	return nil
}

// unlink removes every inbound reference to a node, used when replacing an id.
func (index *Index) unlink(target *node) {
	for layer := range target.neighbors {
		for _, neighborID := range target.neighbors[layer] {
			neighbor, found := index.nodes[neighborID]
			if !found || layer >= len(neighbor.neighbors) {
				continue
			}
			filtered := neighbor.neighbors[layer][:0]
			for _, id := range neighbor.neighbors[layer] {
				if id != target.id {
					filtered = append(filtered, id)
				}
			}
			neighbor.neighbors[layer] = filtered
		}
	}
}

// candidate is a node paired with its distance to the query.
type candidate struct {
	id       int64
	distance float64
}

// toCandidates measures a set of ids against a vector.
func (index *Index) toCandidates(vector []float32, ids []int64) []candidate {
	candidates := make([]candidate, 0, len(ids))
	for _, id := range ids {
		if neighbor, found := index.nodes[id]; found {
			candidates = append(candidates, candidate{id: id, distance: distance(vector, neighbor.vector)})
		}
	}
	return candidates
}

// greedyClosest walks one layer, always stepping to the closest neighbor, until no
// neighbor improves on the current node. This is the cheap descent used in the upper
// layers where a single best candidate is all that's needed.
func (index *Index) greedyClosest(query []float32, start int64, layer int) int64 {
	current := start
	currentDistance := distance(query, index.nodes[current].vector)

	for {
		improved := false
		node := index.nodes[current]
		if layer >= len(node.neighbors) {
			return current
		}
		for _, neighborID := range node.neighbors[layer] {
			neighbor, found := index.nodes[neighborID]
			if !found {
				continue
			}
			if candidateDistance := distance(query, neighbor.vector); candidateDistance < currentDistance {
				current, currentDistance, improved = neighborID, candidateDistance, true
			}
		}
		if !improved {
			return current
		}
	}
}

// searchLayer explores one layer and returns the ef closest nodes found, nearest first.
//
// It keeps two heaps: a min-heap of candidates still worth expanding, and a max-heap of
// the best results so far. Expansion stops as soon as the nearest unexpanded candidate
// is further than the worst kept result, since nothing reachable through it can improve
// the set — that early exit is what makes the search sublinear.
func (index *Index) searchLayer(query []float32, entryPoints []int64, ef int, layer int) []candidate {
	visited := make(map[int64]bool, ef*2)

	candidates := &minHeap{}
	results := &maxHeap{}
	heap.Init(candidates)
	heap.Init(results)

	for _, id := range entryPoints {
		entry, found := index.nodes[id]
		if !found {
			continue
		}
		entryDistance := distance(query, entry.vector)
		visited[id] = true
		heap.Push(candidates, candidate{id: id, distance: entryDistance})
		heap.Push(results, candidate{id: id, distance: entryDistance})
	}

	for candidates.Len() > 0 {
		nearest := heap.Pop(candidates).(candidate)
		if results.Len() >= ef && nearest.distance > (*results)[0].distance {
			break
		}

		node, found := index.nodes[nearest.id]
		if !found || layer >= len(node.neighbors) {
			continue
		}

		for _, neighborID := range node.neighbors[layer] {
			if visited[neighborID] {
				continue
			}
			visited[neighborID] = true

			neighbor, found := index.nodes[neighborID]
			if !found {
				continue
			}
			neighborDistance := distance(query, neighbor.vector)

			if results.Len() < ef {
				heap.Push(candidates, candidate{id: neighborID, distance: neighborDistance})
				heap.Push(results, candidate{id: neighborID, distance: neighborDistance})
			} else if neighborDistance < (*results)[0].distance {
				heap.Push(candidates, candidate{id: neighborID, distance: neighborDistance})
				heap.Push(results, candidate{id: neighborID, distance: neighborDistance})
				heap.Pop(results)
			}
		}
	}

	ordered := make([]candidate, results.Len())
	for index := len(ordered) - 1; index >= 0; index-- {
		ordered[index] = heap.Pop(results).(candidate)
	}
	return ordered
}

// selectNeighbors picks which candidates a node should link to, using the paper's
// heuristic rather than simply taking the closest.
//
// A candidate is kept only if it is closer to the query than to any already-kept
// neighbor. Taking the nearest M would fill a node's links with one tight cluster,
// leaving whole regions unreachable and stranding greedy search in local minima; this
// keeps links pointing in diverse directions, which is what preserves connectivity.
func (index *Index) selectNeighbors(query []float32, candidates []candidate, count int) []int64 {
	if len(candidates) <= count {
		selected := make([]int64, len(candidates))
		for position, item := range candidates {
			selected[position] = item.id
		}
		return selected
	}

	sorted := make([]candidate, len(candidates))
	copy(sorted, candidates)
	sortCandidates(sorted)

	selected := make([]int64, 0, count)
	for _, item := range sorted {
		if len(selected) >= count {
			break
		}

		keep := true
		for _, chosenID := range selected {
			chosen := index.nodes[chosenID]
			if distance(index.nodes[item.id].vector, chosen.vector) < item.distance {
				keep = false
				break
			}
		}
		if keep {
			selected = append(selected, item.id)
		}
	}

	// The heuristic can be strict enough to under-fill the budget. Backfill with the
	// nearest unused candidates rather than leaving a node under-connected.
	if len(selected) < count {
		chosen := make(map[int64]bool, len(selected))
		for _, id := range selected {
			chosen[id] = true
		}
		for _, item := range sorted {
			if len(selected) >= count {
				break
			}
			if !chosen[item.id] {
				selected = append(selected, item.id)
				chosen[item.id] = true
			}
		}
	}
	return selected
}

// Search returns the k nearest ids to the query, nearest first.
func (index *Index) Search(query []float32, k int) ([]int64, []float64, error) {
	index.mutex.RLock()
	defer index.mutex.RUnlock()

	if len(index.nodes) == 0 {
		return nil, nil, nil
	}
	if index.dimensions != 0 && len(query) != index.dimensions {
		return nil, nil, ErrDimensionMismatch
	}
	if k <= 0 {
		return nil, nil, nil
	}

	// ef below k would cap results before k could be filled.
	ef := max(index.params.EfSearch, k)

	current := index.entryPoint
	for layer := index.maxLayer; layer > 0; layer-- {
		current = index.greedyClosest(query, current, layer)
	}

	found := index.searchLayer(query, []int64{current}, ef, 0)
	if len(found) > k {
		found = found[:k]
	}

	ids := make([]int64, len(found))
	distances := make([]float64, len(found))
	for position, item := range found {
		ids[position] = item.id
		distances[position] = item.distance
	}
	return ids, distances, nil
}

// sortCandidates orders by ascending distance. Insertion sort because these slices are
// bounded by efConstruction (a few hundred at most) and nearly sorted already.
func sortCandidates(candidates []candidate) {
	for outer := 1; outer < len(candidates); outer++ {
		item := candidates[outer]
		inner := outer - 1
		for inner >= 0 && candidates[inner].distance > item.distance {
			candidates[inner+1] = candidates[inner]
			inner--
		}
		candidates[inner+1] = item
	}
}
