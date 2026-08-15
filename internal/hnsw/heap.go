package hnsw

// Two heaps over the same candidate type, differing only in comparison direction.
//
// searchLayer needs both at once: a min-heap to always expand the nearest unexplored
// candidate, and a max-heap whose root is the worst result currently kept, so the
// weakest can be evicted in O(log n) and the stopping condition is a single comparison.

type minHeap []candidate

func (h minHeap) Len() int           { return len(h) }
func (h minHeap) Less(i, j int) bool { return h[i].distance < h[j].distance }
func (h minHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *minHeap) Push(value any)    { *h = append(*h, value.(candidate)) }
func (h *minHeap) Pop() any {
	old := *h
	last := len(old) - 1
	item := old[last]
	*h = old[:last]
	return item
}

type maxHeap []candidate

func (h maxHeap) Len() int           { return len(h) }
func (h maxHeap) Less(i, j int) bool { return h[i].distance > h[j].distance }
func (h maxHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *maxHeap) Push(value any)    { *h = append(*h, value.(candidate)) }
func (h *maxHeap) Pop() any {
	old := *h
	last := len(old) - 1
	item := old[last]
	*h = old[:last]
	return item
}
