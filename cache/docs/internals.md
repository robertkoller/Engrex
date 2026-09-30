# Internals

Data structures, storage formats, and the constraints that shaped them. Most of the
non-obvious decisions here exist because of a specific failure; each is named.

## Layout

```
cache.go        Cache: Lookup / Store / Invalidate. Ties everything together
key.go          Namespace and ExactKey hashing; which options are keyed on
split.go        Splitter interface, the marker table, tail and whole fallbacks
entry.go        Entry, Payload, and the base64 Vector encoding
index.go        Entries by id and by namespace, plus the HNSW graph and tombstones
journal.go      Append-only durable log and compaction
policy.go       Thresholds, TTL tiers, volatility, capacity
sketch.go       MinHash over context, for the tolerant tier's overlap guard
provider.go     Provider interface and the routing registry
ollama.go       The Ollama contract: parse, forward, replay
proxy.go        HTTP handlers, cache headers, admin routes
stream.go       Flushing writer so streamed frames reach the client immediately
server.go       Wires it all up; runs the proxy and dashboard listeners
metrics.go      Counters, percentile rings, Prometheus text exposition
dashboard.go    /metrics, /api/stats, and the embedded HTML page
calibrate.go    Measures paraphrase separation, recommends a threshold
tune.go         Lookup log and threshold replay
loadtest.go     Workload generation and the benchmark driver
synthetic.go    Stand-in provider for benchmarking without a real model
report.go       Terminal tables in the house style
```

## Entry

One cached response and everything needed to decide whether it may be served again.

```go
type Entry struct {
    ID             int64      // index key; starts at 1
    ExactKey       string     // hash of the whole request
    Namespace      string     // exact-match bucket
    Class          Class
    Model, Endpoint string
    Prompt         string     // kept whole for debugging; nothing matches on it
    Semantic       string     // the part that was embedded
    Embedding      Vector
    ContextSketch  []uint64   // MinHash, only when the tolerant tier is on
    Payload        Payload
    GenerationTime time.Duration
    CreatedAt, ExpiresAt, LastUsedAt time.Time
    HitCount       int
    Tags           []string
}
```

`GenerationTime` is how long the provider took the one time this was really generated.
Summing it across hits is where the "time saved" figure comes from — it is the honest
version of the guide's cost saving, since nothing here is billed and what is actually saved
is wall-clock.

`ExactKey` is persisted rather than recomputed. Without it the fast path would be empty
after every restart until each prompt had been seen again.

### Payload

Normalised rather than stored verbatim:

```go
type Payload struct {
    Text      string         // the whole completion, for generation
    Embedding Vector         // the vector, for embeddings
    Meta      map[string]any // provider token counts, durations, finish reason
}
```

Whether the caller streamed changes how a response was framed, not what the model produced.
Storing the content and re-framing on the way out lets a streaming and a non-streaming call
share one entry, and lets `/api/generate` and `/api/chat` use the same storage with
different output shapes.

Ollama's `context` array is deliberately not kept — see the streaming section of
[design.md](design.md).

### Vector encoding

`Vector` is `[]float32` with custom JSON marshalling to base64 of little-endian float32s.
768 dimensions cost about 4KB written that way against roughly 15KB as decimal text, and
the journal is almost entirely embeddings.

## Index

```go
type Index struct {
    graph       *hnsw.Index          // approximate search across all entries
    entries     map[int64]*Entry     // the authority on what is live
    byNamespace map[string][]int64   // exact-bucket lookup
    tombstones  int
    nextID      int64
}
```

Two search paths, because the two lookups have genuinely different shapes:

- **`SearchNamespace`** scans `byNamespace` linearly and computes cosine directly. A
  namespace is one exact context and holds a handful of entries, so this is exact and
  faster than an approximate index. No recall to lose.
- **`SearchGlobal`** uses HNSW, over-fetching `limit * 8` because the graph knows nothing
  about class, expiry or tombstones and some results will be filtered out. Only the
  tolerant tier uses it.

### Constraints inherited from `internal/hnsw`

Three, all discovered by reading it rather than by assuming:

**There is no delete.** `unlink` is called only from `Add` when replacing an existing id,
and that path leaves dangling edges (it removes the target from its own neighbours' lists,
but a node that links *to* the target without being linked back keeps a stale edge) and
never lowers `maxLayer`. So eviction here uses **tombstones**: the entry is removed from
`entries` and `byNamespace`, its node stays in the graph until enough dead nodes
accumulate, and the graph is rebuilt from live entries once tombstones pass 25% of it.
This is what hnswlib and FAISS do, and it keeps the change inside `cache/`. Only removals
of entries that were actually in the graph count as tombstones; counting vectorless
embedding entries used to trigger a full rebuild every few dozen evictions during
ingestion, when those entries are most of the cache and none of the graph.

**The graph only exists when `--tolerant` is on.** Nothing else searches across
namespaces, so a strict cache builds `Index` without one and skips the HNSW insert on
every store.

**Id `-1` is the empty-index sentinel.** `hnsw.New` sets `entryPoint: -1`, and `Add`
branches on it to detect the first node ever inserted. Adding a node with id `-1` corrupts
that invariant. Entry ids start at 1; 0 is avoided too, being too easy to produce from a
zero-valued struct.

**`Add` retains the caller's slice.** It does not copy, so a caller reusing an embedding
buffer would silently rewrite what is already indexed. `Index.Add` copies before inserting.
`TestAddCopiesTheEmbedding` pins this.

### Entries with no vector

An embed-class entry is matched exactly and never by similarity, so nothing ever embeds its
semantic part and `Entry.Embedding` is nil. Such entries are held in `entries` — so exact
lookups, capacity accounting and eviction all still work — but **kept out of the graph and
out of `byNamespace`**. Every embedding of one model shares a single namespace, so listing
them there made each removal scan all of them for nothing.

This was a crash, not a theoretical concern. Handing a zero-length vector to `hnsw.Add`
seeds the graph with a node of dimension 0; the next real insert calls `distance` against
it and panics with `index out of range [0] with length 0`, taking down the request handler
mid-stream. Since every Engrex query is *embed then generate*, it fired on essentially any
real use. `SearchNamespace` and `rebuildLocked` skip vectorless entries for the same reason.
`TestVectorlessEntryDoesNotBreakTheIndex` reproduces the original crash.

## Journal

An append-only log at `~/.engrex/cache/entries.jsonl`, one JSON object per line:

```json
{"op":"put","entry":{...}}
{"op":"del","id":7}
```

A log rather than a rewritten file because writing is on the hot path — every cache miss
ends with one — and appending a line is the cheapest durable thing available.

`ReadJournal` replays it at startup, applying puts and deletes in order and keeping
first-seen ordering. **A half-written final line is dropped rather than treated as an
error**: the process can be killed between the write and the flush, and losing the last
cached response is not a reason to refuse to start. `TestTornFinalLineIsDropped` pins it.

Entries carry a whole embedding, so a line is a few kilobytes — well past `bufio.Scanner`'s
default limit. The replay uses `bufio.Reader.ReadBytes` instead.

`Compact` rewrites the log as one put per live entry, via a temporary file and a rename, so
a crash mid-compaction leaves the old journal intact rather than a half-written one — the
same approach `internal/hnsw` takes for its snapshots. It runs at startup and after any
store once the journal holds more than `2 × live + 1000` records, so the file tracks the
live set instead of growing with every store and eviction ever made. The rewrite carries
each entry's current recency and hit count, which is how LRU order survives a restart.

The HNSW graph is **not** persisted. It is rebuilt from the journal at startup, which for a
capped cache takes seconds and removes a whole class of "index and journal disagree" bugs.
Note also that `hnsw`'s own snapshot encoding ranges over a Go map, so two saves of an
identical index produce different bytes — index files must never be content-hashed.

## Eviction

TTL expiry first, then an LRU cap at `MaxEntries` (10,000). Expiry is checked on every
lookup, so an expired entry is never served; the sweep that frees its memory runs at most
once a minute. When the cap is exceeded, eviction clears 5% of headroom below it, so the
LRU sort is paid once per ~500 stores rather than on every store once the cache is full.

Recency and hit counts live in `Cache.usage`, not on the `Entry`. Entries are read by the
index and the journal without the cache lock, so an `Entry` is never written after it is
stored; mutating `LastUsedAt` in place was a data race with journal serialization.
A hit deliberately does **not** write to the journal: it is the hot path, and compaction
persists recency anyway.

If two identical misses both store, the newer entry replaces the older one rather than
leaving it in the index unreachable by key.

## Locking

`Cache.mutex` guards `byExactKey` and `usage`, and serialises every change to the index
together with its journal record. `Index` and `Journal` each have their own lock, always
taken inside the cache lock and never the other way round. Journalling under the cache
lock is deliberate: with it outside, an eviction could journal a delete before the put it
undoes, and the entry would come back on the next restart.

The proxy also coalesces identical in-flight misses: a request whose exact key is already
being generated waits for that generation, then looks up and gets an exact hit. The wait
is only held for a cacheable miss; hits, bypasses and failed lookups release it at once.

`hnsw.Index` itself is safe for concurrent reads with an exclusive write lock, so `Add`
stalls readers; that is a throughput constraint, not a correctness one, and cache stores
are infrequent relative to lookups.

## Metrics

Counters plus three fixed-size sample rings (hit latency, miss latency, near-miss
similarity) of 10,000 entries each. A ring rather than every observation ever, so a
long-running proxy's memory does not grow with its traffic and the percentiles reflect
recent behaviour rather than the cold start.

Prometheus text exposition is written by hand — the format is a few lines, this project has
a habit of building its own index rather than adding a dependency, and `go.mod` stays
short. `_total` names are emitted as counters and everything else as gauges or summaries,
because promtool flags the mismatch.

## Storage summary

| Path | Contents |
|---|---|
| `~/.engrex/cache/entries.jsonl` | The journal — stores and evictions |
| `~/.engrex/cache/lookups.jsonl` | Every lookup and how close it came, for `tune` |
| `~/.engrex/cache.log` | Service log when run under launchd |

Nothing is written inside the repository.
