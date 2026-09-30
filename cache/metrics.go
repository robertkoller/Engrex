package cache

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"time"
)

// Metrics counts what the cache did, in a form both the dashboard and Prometheus can
// read.
//
// Written by hand rather than with a client library. The exposition format is a few
// lines of text, this project has a habit of building its own index rather than adding a
// dependency, and go.mod stays as short as it is.
type Metrics struct {
	mutex sync.Mutex

	hits     int64
	misses   int64
	bypasses int64
	errors   int64
	stores   int64

	hitsByTier    map[Tier]int64
	hitsByClass   map[Class]int64
	missesByClass map[Class]int64
	hitsByModel   map[string]int64
	missesByModel map[string]int64

	hitLatency  *samples
	missLatency *samples

	// nearMisses records the best similarity of every lookup that did not hit. This is
	// the distribution the threshold tuner works from — a miss at 0.96 against a 0.97
	// threshold is a hit that a slightly looser setting would have caught.
	nearMisses *samples

	// timeSaved sums, for every hit, how long that response took the one time it was
	// really generated. It is the headline number and the honest version of the guide's
	// cost saving: nothing here is billed, so what is saved is wall-clock, not dollars.
	timeSaved time.Duration

	evictions int64
	startedAt time.Time
}

func NewMetrics() *Metrics {
	return &Metrics{
		hitsByTier:    make(map[Tier]int64),
		hitsByClass:   make(map[Class]int64),
		missesByClass: make(map[Class]int64),
		hitsByModel:   make(map[string]int64),
		missesByModel: make(map[string]int64),
		hitLatency:    newSamples(),
		missLatency:   newSamples(),
		nearMisses:    newSamples(),
		startedAt:     time.Now(),
	}
}

func (metrics *Metrics) ObserveHit(result Result, elapsed time.Duration) {
	if metrics == nil {
		return
	}
	metrics.mutex.Lock()
	defer metrics.mutex.Unlock()

	metrics.hits++
	metrics.hitsByTier[result.Tier]++
	metrics.hitsByClass[result.Class]++
	metrics.hitsByModel[result.Request.Model]++
	metrics.hitLatency.add(float64(elapsed.Microseconds()))
	if result.Entry != nil {
		metrics.timeSaved += result.Entry.GenerationTime
	}
}

func (metrics *Metrics) ObserveMiss(result Result, elapsed time.Duration) {
	if metrics == nil {
		return
	}
	metrics.mutex.Lock()
	defer metrics.mutex.Unlock()

	if result.Status == StatusBypass {
		metrics.bypasses++
		return
	}
	metrics.misses++
	metrics.missesByClass[result.Class]++
	metrics.missesByModel[result.Request.Model]++
	metrics.missLatency.add(float64(elapsed.Microseconds()))
	if result.Similarity > 0 {
		metrics.nearMisses.add(result.Similarity)
	}
}

func (metrics *Metrics) ObserveError() {
	if metrics == nil {
		return
	}
	metrics.mutex.Lock()
	defer metrics.mutex.Unlock()
	metrics.errors++
}

// RecordLookup, RecordStore and RecordEviction implement Recorder, so the cache can
// report evictions and stores that never pass through the proxy handler.
func (metrics *Metrics) RecordLookup(Result) {}

func (metrics *Metrics) RecordStore(*Entry) {
	if metrics == nil {
		return
	}
	metrics.mutex.Lock()
	defer metrics.mutex.Unlock()
	metrics.stores++
}

func (metrics *Metrics) RecordEviction(count int) {
	if metrics == nil {
		return
	}
	metrics.mutex.Lock()
	defer metrics.mutex.Unlock()
	metrics.evictions += int64(count)
}

// Snapshot is the dashboard's view, and what /cache/stats returns.
type Snapshot struct {
	Hits     int64 `json:"hits"`
	Misses   int64 `json:"misses"`
	Bypasses int64 `json:"bypasses"`
	Errors   int64 `json:"errors"`
	Stores   int64 `json:"stores"`

	HitRate float64 `json:"hit_rate"`
	Entries int     `json:"entries"`

	HitsByTier    map[Tier]int64   `json:"hits_by_tier"`
	HitsByClass   map[Class]int64  `json:"hits_by_class"`
	MissesByClass map[Class]int64  `json:"misses_by_class"`
	HitsByModel   map[string]int64 `json:"hits_by_model"`

	HitLatencyMicros  Percentiles `json:"hit_latency_micros"`
	MissLatencyMicros Percentiles `json:"miss_latency_micros"`

	NearMissSimilarity Percentiles `json:"near_miss_similarity"`

	TimeSavedSeconds float64 `json:"time_saved_seconds"`
	Evictions        int64   `json:"evictions"`
	UptimeSeconds    float64 `json:"uptime_seconds"`
}

// Percentiles is the shape every latency and score distribution is reported in.
type Percentiles struct {
	Count int     `json:"count"`
	P50   float64 `json:"p50"`
	P95   float64 `json:"p95"`
	P99   float64 `json:"p99"`
	Mean  float64 `json:"mean"`
}

func (metrics *Metrics) Snapshot(cache *Cache) Snapshot {
	metrics.mutex.Lock()
	defer metrics.mutex.Unlock()

	total := metrics.hits + metrics.misses
	snapshot := Snapshot{
		Hits:               metrics.hits,
		Misses:             metrics.misses,
		Bypasses:           metrics.bypasses,
		Errors:             metrics.errors,
		Stores:             metrics.stores,
		HitsByTier:         copyCounts(metrics.hitsByTier),
		HitsByClass:        copyCounts(metrics.hitsByClass),
		MissesByClass:      copyCounts(metrics.missesByClass),
		HitsByModel:        copyCounts(metrics.hitsByModel),
		HitLatencyMicros:   metrics.hitLatency.percentiles(),
		MissLatencyMicros:  metrics.missLatency.percentiles(),
		NearMissSimilarity: metrics.nearMisses.percentiles(),
		TimeSavedSeconds:   metrics.timeSaved.Seconds(),
		Evictions:          metrics.evictions,
		UptimeSeconds:      time.Since(metrics.startedAt).Seconds(),
	}
	if total > 0 {
		snapshot.HitRate = float64(metrics.hits) / float64(total)
	}
	if cache != nil {
		snapshot.Entries = cache.Len()
	}
	return snapshot
}

func copyCounts[Key comparable](counts map[Key]int64) map[Key]int64 {
	copied := make(map[Key]int64, len(counts))
	for key, value := range counts {
		copied[key] = value
	}
	return copied
}

// WritePrometheus renders the metrics in the Prometheus text exposition format, so a real
// Prometheus can scrape this without anything else being installed.
func (metrics *Metrics) WritePrometheus(out io.Writer, cache *Cache) {
	snapshot := metrics.Snapshot(cache)

	counter(out, "engrex_cache_lookups_total", "Lookups by outcome.", map[string]int64{
		`outcome="hit"`:    snapshot.Hits,
		`outcome="miss"`:   snapshot.Misses,
		`outcome="bypass"`: snapshot.Bypasses,
		`outcome="error"`:  snapshot.Errors,
	})

	byTier := make(map[string]int64, len(snapshot.HitsByTier))
	for tier, count := range snapshot.HitsByTier {
		byTier[fmt.Sprintf("tier=%q", tier)] = count
	}
	counter(out, "engrex_cache_hits_by_tier_total", "Hits by how they were found.", byTier)

	byClass := make(map[string]int64, len(snapshot.HitsByClass)+len(snapshot.MissesByClass))
	for class, count := range snapshot.HitsByClass {
		byClass[fmt.Sprintf("class=%q,outcome=\"hit\"", class)] = count
	}
	for class, count := range snapshot.MissesByClass {
		byClass[fmt.Sprintf("class=%q,outcome=\"miss\"", class)] = count
	}
	counter(out, "engrex_cache_lookups_by_class_total", "Lookups by call class.", byClass)

	gauge(out, "engrex_cache_hit_rate", "Hits over hits plus misses.", snapshot.HitRate)
	gauge(out, "engrex_cache_entries", "Live entries held.", float64(snapshot.Entries))

	// A counter, not a gauge: the _total suffix is reserved for counters and promtool
	// flags the mismatch.
	counter(out, "engrex_cache_evictions_total", "Entries dropped.", map[string]int64{
		`reason="all"`: snapshot.Evictions,
	})
	gauge(out, "engrex_cache_time_saved_seconds", "Generation time avoided by serving hits.", snapshot.TimeSavedSeconds)

	quantiles(out, "engrex_cache_hit_latency_microseconds", "Time to serve a hit.", snapshot.HitLatencyMicros)
	quantiles(out, "engrex_cache_miss_latency_microseconds", "Time to serve a miss, provider included.", snapshot.MissLatencyMicros)
	quantiles(out, "engrex_cache_near_miss_similarity", "Best similarity on lookups that missed.", snapshot.NearMissSimilarity)
}

func counter(out io.Writer, name, help string, values map[string]int64) {
	fmt.Fprintf(out, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	labels := make([]string, 0, len(values))
	for label := range values {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	for _, label := range labels {
		fmt.Fprintf(out, "%s{%s} %d\n", name, label, values[label])
	}
}

func gauge(out io.Writer, name, help string, value float64) {
	fmt.Fprintf(out, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", name, help, name, name, value)
}

func quantiles(out io.Writer, name, help string, percentiles Percentiles) {
	fmt.Fprintf(out, "# HELP %s %s\n# TYPE %s summary\n", name, help, name)
	fmt.Fprintf(out, "%s{quantile=\"0.5\"} %g\n", name, percentiles.P50)
	fmt.Fprintf(out, "%s{quantile=\"0.95\"} %g\n", name, percentiles.P95)
	fmt.Fprintf(out, "%s{quantile=\"0.99\"} %g\n", name, percentiles.P99)
	fmt.Fprintf(out, "%s_count %d\n", name, percentiles.Count)
}

// sampleCapacity bounds what the percentile estimates are computed over. A ring rather
// than every observation ever, so a long-running proxy's memory does not grow with its
// traffic, and so the numbers reflect recent behavior rather than the cold start.
const sampleCapacity = 10000

type samples struct {
	values []float64
	next   int
	filled bool
	total  float64
	count  int
}

func newSamples() *samples {
	return &samples{values: make([]float64, sampleCapacity)}
}

func (ring *samples) add(value float64) {
	ring.values[ring.next] = value
	ring.next = (ring.next + 1) % sampleCapacity
	if ring.next == 0 {
		ring.filled = true
	}
	ring.total += value
	ring.count++
}

func (ring *samples) percentiles() Percentiles {
	size := ring.next
	if ring.filled {
		size = sampleCapacity
	}
	if size == 0 {
		return Percentiles{}
	}

	sorted := make([]float64, size)
	copy(sorted, ring.values[:size])
	sort.Float64s(sorted)

	return Percentiles{
		Count: ring.count,
		P50:   quantile(sorted, 0.50),
		P95:   quantile(sorted, 0.95),
		P99:   quantile(sorted, 0.99),
		Mean:  ring.total / float64(ring.count),
	}
}

func quantile(sorted []float64, fraction float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	position := int(fraction * float64(len(sorted)))
	if position >= len(sorted) {
		position = len(sorted) - 1
	}
	return sorted[position]
}
