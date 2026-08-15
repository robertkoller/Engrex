package hnsw

import (
	"fmt"
	"io"
	"math"
	"math/rand"
	"sort"
	"strings"
	"time"
)

// This file answers the only question that makes a hand-written index worth having:
// at what corpus size does it actually beat brute force, and what recall does it cost?
//
// Reporting the crossover honestly is the point. On a personal corpus the answer is
// usually "brute force wins", and a benchmark that says so is more useful — and more
// credible — than one tuned to make the fancy path look good.

// BenchmarkRow is one corpus size's measurements.
type BenchmarkRow struct {
	CorpusSize int

	BuildTime time.Duration

	// ExactLatency and ApproxLatency are median per-query times.
	ExactLatency  time.Duration
	ApproxLatency time.Duration

	// RecallAtK is the fraction of true nearest neighbors the index returned.
	RecallAtK float64

	// MemoryBytes is the approximate resident size of the graph: vectors plus links.
	MemoryBytes int64

	EfSearch int
}

// Speedup is how many times faster approximate search was than exact. Below 1.0 means
// brute force won.
func (row BenchmarkRow) Speedup() float64 {
	if row.ApproxLatency == 0 {
		return 0
	}
	return float64(row.ExactLatency) / float64(row.ApproxLatency)
}

// BenchmarkConfig controls the sweep.
type BenchmarkConfig struct {
	Sizes      []int
	Dimensions int
	Queries    int
	K          int
	Params     Params

	// EfSweep, when set, re-measures the largest corpus at each efSearch value to show
	// the recall/latency trade-off directly.
	EfSweep []int
}

// DefaultBenchmarkConfig sweeps the range that brackets a personal corpus and extends
// past it, at Engrex's real embedding dimensionality.
func DefaultBenchmarkConfig() BenchmarkConfig {
	return BenchmarkConfig{
		Sizes:      []int{1000, 5000, 10000, 50000, 100000},
		Dimensions: 768,
		Queries:    50,
		K:          10,
		Params:     DefaultParams(),
		EfSweep:    []int{10, 25, 50, 100, 200},
	}
}

// Run executes the sweep, streaming progress to out because the large sizes take a
// while to build and a silent multi-minute benchmark looks like a hang.
func Run(config BenchmarkConfig, out io.Writer) ([]BenchmarkRow, error) {
	var rows []BenchmarkRow

	for _, size := range config.Sizes {
		fmt.Fprintf(out, "building %d vectors (dim %d)... ", size, config.Dimensions)

		vectors := randomUnitVectorsBench(size, config.Dimensions, int64(size))
		queries := randomUnitVectorsBench(config.Queries, config.Dimensions, int64(size)+1)

		index := New(config.Params)
		buildStart := time.Now()
		for id, vector := range vectors {
			if err := index.Add(int64(id), vector); err != nil {
				return nil, err
			}
		}
		buildTime := time.Since(buildStart)
		fmt.Fprintf(out, "%s\n", buildTime.Round(time.Millisecond))

		row := measure(index, vectors, queries, config.K)
		row.CorpusSize = size
		row.BuildTime = buildTime
		row.MemoryBytes = index.MemoryBytes()
		row.EfSearch = config.Params.EfSearch
		rows = append(rows, row)
	}

	return rows, nil
}

// measure times both search paths and scores the approximate one against the exact.
func measure(index *Index, vectors, queries [][]float32, k int) BenchmarkRow {
	exactTimings := make([]time.Duration, 0, len(queries))
	approxTimings := make([]time.Duration, 0, len(queries))
	totalRecall := 0.0

	for _, query := range queries {
		start := time.Now()
		want := exactNearestBench(vectors, query, k)
		exactTimings = append(exactTimings, time.Since(start))

		start = time.Now()
		got, _, _ := index.Search(query, k)
		approxTimings = append(approxTimings, time.Since(start))

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
		totalRecall += float64(hits) / float64(len(want))
	}

	return BenchmarkRow{
		ExactLatency:  median(exactTimings),
		ApproxLatency: median(approxTimings),
		RecallAtK:     totalRecall / float64(len(queries)),
	}
}

// RunEfSweep measures how recall and latency move together as efSearch changes, on a
// single fixed corpus. This is the trade-off curve — the thing efSearch actually buys.
func RunEfSweep(config BenchmarkConfig, size int, out io.Writer) ([]BenchmarkRow, error) {
	fmt.Fprintf(out, "building %d vectors for the efSearch sweep... ", size)
	vectors := randomUnitVectorsBench(size, config.Dimensions, int64(size))
	queries := randomUnitVectorsBench(config.Queries, config.Dimensions, int64(size)+1)

	index := New(config.Params)
	buildStart := time.Now()
	for id, vector := range vectors {
		if err := index.Add(int64(id), vector); err != nil {
			return nil, err
		}
	}
	fmt.Fprintf(out, "%s\n", time.Since(buildStart).Round(time.Millisecond))

	var rows []BenchmarkRow
	for _, efSearch := range config.EfSweep {
		index.SetEfSearch(efSearch)
		row := measure(index, vectors, queries, config.K)
		row.CorpusSize = size
		row.EfSearch = efSearch
		row.MemoryBytes = index.MemoryBytes()
		rows = append(rows, row)
	}
	return rows, nil
}

// MemoryBytes approximates the graph's resident size: the vectors themselves plus the
// neighbor lists. Vectors dominate at 768 dimensions, which is worth seeing next to
// the speedup — the index is not free.
func (index *Index) MemoryBytes() int64 {
	index.mutex.RLock()
	defer index.mutex.RUnlock()

	var total int64
	for _, item := range index.nodes {
		total += int64(len(item.vector) * 4) // float32
		for _, layer := range item.neighbors {
			total += int64(len(layer) * 8) // int64 ids
		}
	}
	return total
}

// WriteReport renders the sweep, ending with the crossover statement.
func WriteReport(rows []BenchmarkRow, out io.Writer) {
	fmt.Fprintf(out, "\n%-10s %10s %12s %12s %9s %9s %10s\n",
		"CORPUS", "BUILD", "EXACT", "HNSW", "SPEEDUP", "RECALL", "MEMORY")
	fmt.Fprintln(out, strings.Repeat("-", 80))

	for _, row := range rows {
		fmt.Fprintf(out, "%-10d %10s %12s %12s %8.2fx %9.3f %9.1fMB\n",
			row.CorpusSize,
			row.BuildTime.Round(time.Millisecond),
			formatLatency(row.ExactLatency),
			formatLatency(row.ApproxLatency),
			row.Speedup(),
			row.RecallAtK,
			float64(row.MemoryBytes)/(1024*1024))
	}

	writeCrossover(rows, out)
}

// WriteEfReport renders the efSearch trade-off curve.
func WriteEfReport(rows []BenchmarkRow, out io.Writer) {
	fmt.Fprintf(out, "\n%-10s %12s %9s   %s\n", "EF_SEARCH", "LATENCY", "RECALL", "")
	fmt.Fprintln(out, strings.Repeat("-", 50))
	for _, row := range rows {
		fmt.Fprintf(out, "%-10d %12s %9.3f   %s\n",
			row.EfSearch,
			formatLatency(row.ApproxLatency),
			row.RecallAtK,
			bar(row.RecallAtK))
	}
}

// writeCrossover states, in words, the corpus size where approximate search starts
// winning — or that it never did within the range measured.
func writeCrossover(rows []BenchmarkRow, out io.Writer) {
	fmt.Fprintln(out)

	crossover := -1
	for _, row := range rows {
		if row.Speedup() > 1.0 {
			crossover = row.CorpusSize
			break
		}
	}

	if crossover == -1 {
		fmt.Fprintln(out, "CROSSOVER: none within the range measured — brute force wins at every")
		fmt.Fprintln(out, "size tested, so exact search stays the default. The index is worth")
		fmt.Fprintln(out, "keeping only if the corpus is expected to grow well past these sizes.")
		return
	}

	fmt.Fprintf(out, "CROSSOVER: HNSW overtakes brute force at ~%d vectors.\n", crossover)
	fmt.Fprintln(out, "Below that, exact search is both faster and lossless, so it stays the default.")
}

func formatLatency(duration time.Duration) string {
	if duration < time.Microsecond {
		return fmt.Sprintf("%dns", duration.Nanoseconds())
	}
	if duration < time.Millisecond {
		return fmt.Sprintf("%.1fµs", float64(duration.Nanoseconds())/1000)
	}
	return fmt.Sprintf("%.2fms", float64(duration.Nanoseconds())/1e6)
}

func bar(fraction float64) string {
	filled := int(fraction * 20)
	if filled < 0 {
		filled = 0
	}
	if filled > 20 {
		filled = 20
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", 20-filled)
}

func median(durations []time.Duration) time.Duration {
	if len(durations) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), durations...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

// clusterCount is how many topic centroids the synthetic corpus is built around, and
// spread is how far a vector drifts from its centroid — expressed as the magnitude of
// the whole offset vector, not per-dimension.
//
// The distinction is the difference between a realistic corpus and noise. Drawing each
// dimension from N(0, spread) makes the offset's magnitude spread*sqrt(dimensions),
// which at 768 dimensions is ~27x the unit-length centroid for spread=0.35: the
// centroid contributes nothing and the "clusters" are uniform random. Dividing by
// sqrt(dimensions) fixes the offset's total magnitude at spread regardless of
// dimensionality, so 0.35 means what it reads as — a point 35% of the way out from its
// cluster centre.
const (
	clusterCount = 60
	spread       = 0.35
)

// randomUnitVectorsBench generates a synthetic corpus that behaves like real embeddings.
//
// The obvious approach — uniform random directions on the unit sphere — is actively
// misleading at 768 dimensions. Concentration of measure makes independent random
// vectors almost exactly equidistant from each other, so "the true 10 nearest
// neighbors" is a near-arbitrary pick among thousands of ties, and ANY approximate
// index scores terrible recall against a ground truth that is itself noise. Measured
// here, that produced recall falling 0.94 -> 0.56 -> 0.24 as the corpus grew, which
// says nothing about the index.
//
// Real embedding corpora are nothing like that: documents cluster by topic and occupy
// a low-dimensional manifold inside the ambient 768. So the corpus is generated as
// Gaussian clusters around random centroids, which reproduces the neighborhood
// structure HNSW is designed to exploit — and which a personal knowledge base actually
// has.
func randomUnitVectorsBench(count, dimensions int, seed int64) [][]float32 {
	random := rand.New(rand.NewSource(seed))

	centroids := make([][]float64, clusterCount)
	for index := range centroids {
		centroids[index] = randomDirection(random, dimensions)
	}

	// Normalizing the per-dimension noise by sqrt(dimensions) keeps the offset's total
	// magnitude at spread instead of letting it grow with the ambient dimension.
	noiseScale := spread / math.Sqrt(float64(dimensions))

	vectors := make([][]float32, count)
	for index := range vectors {
		centroid := centroids[random.Intn(clusterCount)]

		vector := make([]float32, dimensions)
		var sumOfSquares float64
		for position := range vector {
			value := centroid[position] + random.NormFloat64()*noiseScale
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

// randomDirection draws a uniformly random unit vector, used for cluster centroids.
func randomDirection(random *rand.Rand, dimensions int) []float64 {
	direction := make([]float64, dimensions)
	var sumOfSquares float64
	for index := range direction {
		value := random.NormFloat64()
		direction[index] = value
		sumOfSquares += value * value
	}
	magnitude := math.Sqrt(sumOfSquares)
	for index := range direction {
		direction[index] /= magnitude
	}
	return direction
}

// exactNearestBench is the brute-force baseline: a full linear scan, which is exactly
// what sqlite-vec's vec0 does today.
func exactNearestBench(vectors [][]float32, query []float32, k int) []int64 {
	type scored struct {
		id       int64
		distance float64
	}
	all := make([]scored, len(vectors))
	for index, vector := range vectors {
		all[index] = scored{id: int64(index), distance: distance(query, vector)}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].distance < all[j].distance })
	if len(all) > k {
		all = all[:k]
	}
	ids := make([]int64, len(all))
	for index, item := range all {
		ids[index] = item.id
	}
	return ids
}
