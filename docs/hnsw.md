# HNSW Index

`internal/hnsw` is a from-scratch Hierarchical Navigable Small World index — an
approximate nearest-neighbor structure built directly rather than pulled from a
library, because `sqlite-vec` has no ANN index to wire up.

This document reports what it actually costs and buys. **The short version: Engrex
still defaults to exact search, because at the corpus size this project realistically
reaches, brute force is competitive and lossless.**

## How it works

A stack of proximity graphs. Layer 0 contains every vector; each layer above holds an
exponentially thinning sample. The upper layers act as an express network that carries
a search most of the way to its target in a few hops, and the dense bottom layer
refines it.

- **Layer assignment** — a node's top layer is drawn from an exponentially decaying
  distribution, `floor(-ln(uniform) / ln(M))`, so roughly `1/M` of the nodes at each
  layer are promoted. This keeps the expected layer count logarithmic in corpus size.
- **Insert** — greedily descend from the entry point through the layers above the new
  node's own level, then at each layer from its level down to 0 run a full candidate
  search, choose neighbors with the selection heuristic, and link bidirectionally.
  Linking back can push a neighbor over its degree budget, so it is re-pruned.
- **Search** — greedy descent from the top, then a bounded-candidate exploration of
  layer 0. Two heaps: a min-heap of candidates still worth expanding, and a max-heap
  of the best results so far. Expansion stops as soon as the nearest unexpanded
  candidate is further than the worst kept result — nothing reachable through it can
  improve the set. That early exit is what makes the search sublinear.
- **Neighbor selection** — a candidate is kept only if it is closer to the query than
  to any already-kept neighbor. Simply taking the nearest `M` would fill a node's links
  with one tight cluster, stranding greedy search in local minima; the heuristic keeps
  links pointing in diverse directions, which is what preserves connectivity.

Distance is cosine, matching `vec0`'s `distance_metric=cosine`, so recall against exact
search is a like-for-like comparison.

## Parameters

| Parameter | Effect | Default |
|---|---|---|
| `M` | Links per node per layer (layer 0 gets `2M`). Higher = better recall, more memory. | 16 |
| `EfConstruction` | Candidate list while inserting. Higher = better graph, slower build. No query cost. | 200 |
| `EfSearch` | Candidate list while querying. **The main recall/latency dial.** | 64 |

## Benchmark methodology, and a trap worth documenting

`engrex bench-hnsw` sweeps corpus size and reports build time, query latency against a
brute-force baseline, recall@10, and memory.

The first two runs of this benchmark produced **wrong** numbers, and the reason
generalizes to any ANN benchmark:

**Uniform random vectors are not a valid corpus.** Drawing each vector as a random
direction on the 768-dimensional unit sphere seems like the neutral choice. It isn't.
Under concentration of measure, independent random high-dimensional vectors are almost
exactly equidistant from one another, so "the true 10 nearest neighbors" is a
near-arbitrary pick among thousands of ties. Recall is then measured against ground
truth that is itself noise, and *any* index scores badly. Measured here: recall fell
0.94 → 0.56 → 0.24 as the corpus grew, which says nothing about the index.

**Per-dimension spread is not cluster spread.** The fix — generating Gaussian clusters
around random centroids — was initially applied by drawing each dimension from
`N(0, 0.35)`. That gives the offset vector a magnitude of `0.35 · sqrt(768) ≈ 9.7`
against a unit-length centroid: the centroid contributes essentially nothing and the
result is uniform random again, with extra steps. Recall barely moved (0.97 → 0.61 →
0.23). Dividing the per-dimension noise by `sqrt(dimensions)` fixes the offset's total
magnitude at `spread` regardless of dimensionality, which is what the constant reads
as and what real embeddings look like.

Real corpora cluster by topic and occupy a low-dimensional manifold inside the ambient
768. The generator now reproduces that.

## Results

MacBook Air, 768 dimensions, `M=16`, `efConstruction=100`, `efSearch=64`, k=10,
25 queries, clustered synthetic corpus.

| Corpus | Build | Exact | HNSW | Speedup | Recall@10 | Memory |
|---|---|---|---|---|---|---|
| 1,000 | 3.1s | 692µs | 411µs | 1.69× | 0.984 | 3.2 MB |
| 5,000 | 17.0s | 3.64ms | 296µs | 12.31× | 0.756 | 15.9 MB |
| 20,000 | 1m37s | 15.03ms | 458µs | 32.78× | 0.480 | 63.6 MB |

### The efSearch trade-off (5,000 vectors)

| efSearch | Latency | Recall@10 |
|---|---|---|
| 10 | 128µs | 0.548 |
| 25 | 182µs | 0.648 |
| 50 | 254µs | 0.752 |
| 100 | 407µs | 0.804 |
| 200 | 723µs | 0.932 |

## What the numbers actually say

**The speedup column is not the headline. The recall column is.**

At a fixed `efSearch=64`, recall *degrades as the corpus grows* — 0.984 → 0.756 →
0.480. This is expected and is the central property of the structure: `efSearch` is an
absolute candidate budget, so holding it constant while the corpus grows means
exploring a shrinking fraction of the graph. Maintaining recall requires raising
`efSearch` with corpus size, and that spends the speedup back. The 32.78× at 20,000
vectors is a number achieved at 0.480 recall — that is, while missing half the correct
answers. It is not a usable operating point.

The honest read: at 5,000 vectors, `efSearch=200` buys 0.932 recall at 723µs against
3.64ms exact — a real ~5× win at acceptable recall. Below ~1,000 vectors the margin
is small enough that exact search wins outright on being lossless.

**Engrex's corpus is currently 21 chunks.** Brute force scans it in microseconds.
Every query also pays one Ollama embedding round-trip (tens of ms) and a full
generation (seconds), each two or more orders of magnitude larger than either search
path. The index changes nothing a user would notice.

## Why it is here anyway

Two defensible reasons, and one that isn't.

**Defensible:** it establishes the crossover empirically rather than by assumption, and
it means the vector layer is no longer bounded by what `sqlite-vec` happens to provide
— if the corpus ever reaches the scale where linear scan hurts, the answer already
exists and its behavior is characterized.

**Not defensible:** using it by default because it sounds impressive. It is off by
default, and should stay off until a measurement says otherwise.

## Usage

```bash
engrex bench-hnsw                                    # full sweep, default parameters
engrex bench-hnsw --sizes 1000,5000 --queries 25     # quick run
engrex bench-hnsw --ef-search 200 --ef-sweep-size 5000
```

The index itself is a library (`internal/hnsw`) with `Save`/`Load` persistence. It is
not yet wired into the query path, which still goes through `sqlite-vec` exact search —
deliberately, per the numbers above.
