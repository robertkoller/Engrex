# RAG Upgrade — Findings and Plan

An audit of the current retrieval pipeline against a "production-grade RAG" target,
plus a prioritized plan. Written 2026-08-14 against commit `af30ef1`.

Read [rag-pipeline.md](rag-pipeline.md) first — this document assumes it.

## Objective

**This work is optimized for demonstrable engineering depth, not for retrieval quality
on the current corpus.** Engrex already answers questions well enough for daily use;
the goal is a system that reads as advanced to a technical reviewer.

That changes what "worth building" means. A feature can be correct to build here even
when it does not measurably improve answers — provided the *engineering* is real and
the claims made about it are honest. It also raises the bar on measurement: an
unbenchmarked advanced feature is a worse signal than not having built it, because the
obvious interview question has no answer.

Two rules follow, and they apply to every phase below:

1. **Every advanced component ships with numbers.** Not "I built X" — "I built X, here
   is what it cost and what it bought."
2. **Never claim a win the benchmark doesn't show.** If the fancy path loses, say it
   lost and say where the crossover is. That reads as stronger, not weaker.

## Summary

The pipeline is further along than "super simple RAG". Hybrid retrieval is already
built and correct. The real gaps are (a) structure is destroyed at ingest time before
chunking can use it, (b) the embedding model is being called incorrectly, and (c)
there is no way to measure whether any change helps — which under the objective above
is the single most costly gap.

## Already done — do not rebuild

### Hybrid search

`rag.Retrieve` (`internal/rag/rag.go:175`) already runs dense vector KNN and FTS5 BM25
in parallel and fuses them with Reciprocal Rank Fusion at k=60 (`fuseRRF`,
`rag.go:269`). `toFTSQuery` (`rag.go:255`) quotes each term so user punctuation can't
be misread as FTS5 syntax. This is the real technique, not a placeholder.

### Non-blind chunking

`chunker.Chunk` packs whole sentences into a 400-word budget with sentence-granular
overlap of ~50 words, and only hard-splits when a single sentence exceeds the budget
(`splitLongSentence`, `chunker.go:61`). A recursive character text splitter — the
paragraph → sentence → character cascade — is roughly what this already does for
prose. Porting one would be a sideways move, not an upgrade.

## Findings

### 1. `stripMarkdown` destroys document structure before chunking

`ingest.go:191` strips `^#{1,6}\s+` — along with list markers, blockquote markers, and
emphasis — during text extraction. By the time text reaches `chunker.Chunk`, every
heading has become an ordinary sentence indistinguishable from body text.

This is the architectural blocker behind section-aware chunking, heading metadata, and
heading-scoped retrieval. None of them can be built on the current extraction output.
Fixing it means extraction has to emit structured content (a heading tree, or text with
preserved markers) rather than a flat string, which changes the `ExtractText` contract.

**Severity: high.** Blocks several other items.

### 2. Code files are sentence-split

`ExtractText` routes `.go`, `.py`, `.js`, `.ts`, `.rs`, `.c`, `.cpp`, `.java`, `.json`,
`.yaml`, `.toml` and friends through the same path as prose (`ingest.go:100`), so they
hit the `[^.!?]+[.!?]*` sentence regex in `chunker.go:13`.

That regex splits on the `.` in `foo.Bar()`, on decimal points, and on `...`. Chunk
boundaries land mid-expression and mid-function. For a corpus containing code, those
chunks are close to unusable.

**Severity: high** if code is a meaningful share of the corpus, low otherwise.

### 3. `nomic-embed-text` is being called without its task prefixes

`embedder.Embed` (`internal/embedder/ollama.go:26`) sends raw text for both stored
chunks and queries.

`nomic-embed-text` was trained with mandatory task prefixes — `search_document: ` for
text being indexed and `search_query: ` for the query. The model is asymmetric by
design; omitting the prefixes measurably degrades retrieval quality because query and
document vectors end up in the same space rather than the intended paired spaces.

Fix is roughly ten lines (split `Embed` into `EmbedDocument` / `EmbedQuery`) plus a full
reindex. Cheapest quality win available.

**Confidence:** high on the requirement, unmeasured on the size of the delta here.

**Severity: high, effort: hours.**

### 4. The vector distance metric is probably L2, not cosine

`db.go:59` declares the virtual table as:

```sql
CREATE VIRTUAL TABLE IF NOT EXISTS vec_chunks USING vec0(
    embedding float[768]
);
```

There is no `distance_metric=` clause, so `sqlite-vec` uses **L2**.

L2 and cosine produce identical rankings *only for unit-normalized vectors*. If Ollama
returns unnormalized `nomic-embed-text` embeddings, the ranking is vector-length biased
— longer chunks systematically score closer regardless of semantic relevance.

**Check before changing anything:** embed a few chunks, compute `sqrt(sum(x^2))` for
each. If the norms are not ~1.0, either normalize in `embedder.Embed` before storage or
declare `distance_metric=cosine` on the table.

This also determines whether the calibrated thresholds still mean what they did:
`deduplicationThreshold` 0.35 (`store.go:45`), `edgeThreshold` 0.8 (`store.go:22`), and
`DefaultSearchDistance` 0.95 (`rag.go:27`) are all L2 distances under the current
schema, not cosine distances.

**Severity: high, effort: minutes to diagnose.**

### 5. `store.Search` ignores its own `topK` at the SQL layer

`store.Search` (`store.go:583`) takes a `topK` parameter but hardcodes `LIMIT 20` inside
the subquery (`store.go:592`), filters by `maxDistance` in Go afterward, then truncates
to `topK`.

It works today only because `hybridCandidates` (`rag.go:32`) happens to also be 20.
Raising `hybridCandidates` to widen the candidate pool for a reranker will silently do
nothing.

Second-order problem: because the distance filter runs *after* the 20-row cut,
tightening `maxDistance` shrinks the result set rather than reranking within a fixed
pool. `RawSearch` (`store.go:562`) has the same hardcoded limit.

**Severity: medium now, blocking once reranking lands.**

### 6. There is no evaluation harness

Every threshold in the system was set by feel. The code says so — `store.go:582`:

```go
// Search performs a K Nearest Neighbors search for the most similar chunks
// I like maxDistance to be 0.95 dont ask me how i chose that its just feeling
```

This is the actual bottleneck. Reranking, query rewriting, and multi-hop retrieval are
all changes whose value cannot currently be observed. Stacking them on unmeasured
retrieval means not knowing which ones helped, which hurt, and which did nothing.

**Severity: highest.** Gates everything else.

## Gaps against the target

Present in a production RAG system, absent here:

| Capability | Status | Notes |
|---|---|---|
| Reranking | Missing | Biggest single retrieval-quality win available. See the open question below. |
| Query rewriting | Missing | Multi-part question decomposition, pronoun/acronym expansion |
| Chunk metadata | Thin | `chunks` holds text, source, origin, created_at. No heading path, chunk index, doc title, or content type. |
| Citation verification | Missing | `buildPrompt` (`rag.go:375`) *asks* for citations; nothing checks them |
| Multi-hop retrieval | Missing | Single retrieval pass only |
| Agentic retrieval | Missing | No tool-use loop over the index |

## HNSW — build it, and benchmark it honestly

### Why it will not speed anything up

Stated plainly so it is never claimed otherwise:

768 dimensions × 4 bytes = 3 KB per vector. At 100k chunks that is a 300 MB linear
scan, on the order of tens of milliseconds. A personal corpus is realistically well
under that. Meanwhile every query pays one Ollama embedding round-trip and a full
`llama3.2` generation — each two or more orders of magnitude larger than the scan.

At the corpus sizes Engrex will actually see, **brute force is expected to win**, and
HNSW adds an approximation that can only lose recall relative to exact KNN. As a
retrieval-quality change this is negative.

### Why it is still the right thing to build here

Under the stated objective it is one of the strongest items on the list. `sqlite-vec`
has no HNSW, so there is nothing to wire up — it means implementing the graph index
directly: layer assignment, the greedy search with a dynamic candidate list, the
neighbor-selection heuristic, `efConstruction`/`efSearch`/`M` tuning, and persistence
and incremental maintenance across inserts and deletes.

That is genuine data-structures work, and it is the part of this plan least reducible
to library glue. It is also the item most likely to be asked about in detail.

### What makes it a strong claim rather than a weak one

The build is the easy half. The claim is the hard half:

- Keep exact and approximate search behind one interface, selectable at query time.
  Being able to switch is the whole benchmark.
- Sweep corpus size (1k → 10k → 100k → 1M synthetic vectors) and report **latency and
  recall@10 against exact KNN** at each point.
- **Find and publish the crossover** — the corpus size where HNSW starts beating brute
  force on this hardware, at a given recall target.
- Report the costs too: index build time, memory overhead, and recall lost at each
  `efSearch`.

The honest result — *"brute force wins below roughly N chunks on this corpus, so
Engrex defaults to exact search; HNSW is there and wins above N, here is the curve"* —
is a materially better story than an unqualified "I implemented HNSW." It demonstrates
the algorithm **and** the judgment to know when not to use it.

The failure mode to avoid is shipping the index with no numbers and defaulting to it
because it sounds better. That inverts the signal.

## Plan

Ordered so that each phase produces something demonstrable, and so the measurement
infrastructure exists before the components that need to be justified by it. Each
phase is independently shippable.

### Phase 0 — Evaluation harness ✅ built

Golden set of question → expected-source pairs, scored by an `engrex eval` command
reporting recall@k, MRR, and query latency, diffed against a committed baseline.

- `internal/eval/eval.go` — golden set loading, recall@k, MRR, hop slicing
- `internal/eval/report.go` — baseline save/load, delta table, miss list
- `cmd/engrex/eval.go` — the `eval` command
- `eval/golden.json` — starter set written against this repo's own `docs/`

The `Retriever` signature is deliberately just `question, topK → []source`, so the same
golden set scores the current pipeline, a reranked one, or an agentic one without the
harness changing.

Two design choices worth defending: recall counts *distinct* expected sources, so
retrieving one relevant document three times doesn't score as three hits; and cases
carry a `hops` field so single-hop and multi-hop performance report separately — that
split is the evidence that later justifies (or sinks) Phase 7.

**Still to do:** replace the ten starter cases with ~40 over the real corpus, then
`engrex eval --save --label baseline` to freeze the pre-upgrade numbers.

### Phase 1 — Correctness fixes ✅ built

- **Task prefixes** — `Embed` split into `EmbedDocument` / `EmbedQuery`, applying
  `search_document: ` and `search_query: ` (finding 3). `EmbedRaw` exists only for
  diagnostics.
- **Cosine, not L2** — `vec_chunks` now declares `distance_metric=cosine`, and vectors
  are normalized to unit length in the embedder (finding 4). `engrex doctor` reports
  the raw model magnitude so the original diagnosis is reproducible.
- **`topK` honored** — `store.Search` and `RawSearch` bind the KNN `LIMIT` instead of
  hardcoding 20, so widening the candidate pool for Phase 4 actually widens it
  (finding 5).

Because both embedding changes move every vector into a different space, the old index
is invalid. Migration 3 drops it and `engrex reindex` re-embeds from the chunk text
already in the database — no re-reading from disk, ids and metadata preserved.

**Still to do:** recalibrate `deduplicationThreshold` (0.35), `edgeThreshold` (0.8) and
`DefaultSearchDistance` (0.95) against the harness. They were calibrated on
unnormalized L2 distances and now mean something different.

### Phase 2 — Structure ✅ built

- **Structure survives extraction** — `stripMarkdown` became `cleanMarkdown`, which
  keeps headings, list markers, and blockquotes and strips only inline noise
  (finding 1). This was the blocker; everything else in the phase depended on it.
- **Section-aware chunking** — `chunker.ChunkDocument` splits at heading boundaries
  first and packs sentences only *within* a section, tracking a heading stack so each
  chunk knows its full ancestry. Each chunk is prefixed with its heading path, so
  "It defaults to 30 seconds" embeds as "Configuration > Timeouts: It defaults to 30
  seconds".
- **Code splitting** — code files break at top-level declarations instead of the prose
  sentence regex (finding 2), with line-windowed fallback inside oversized functions.
- **Metadata columns** — `heading_path`, `chunk_index`, `doc_title`, `content_type`,
  added by migration 2 and backfilled empty for pre-existing rows.

Also added: a real migration framework (`PRAGMA user_version`, forward-only, one
transaction per step) replacing the `CREATE TABLE IF NOT EXISTS` blob, since three
schema changes in one phase made the old approach untenable.

### Phase 3 — HNSW index ✅ built

`internal/hnsw` — layer assignment, greedy descent, dual-heap bounded search, the
neighbor-selection heuristic, degree pruning, and atomic `Save`/`Load`. Benchmarked via
`engrex bench-hnsw`. Ten tests including graph connectivity and degree bounds.

Full results and methodology in [hnsw.md](hnsw.md). The headline: **it is off by
default and should stay off.** At `efSearch=64` recall degrades with corpus size
(0.984 → 0.756 → 0.480) because a fixed candidate budget explores a shrinking fraction
of a growing graph; holding recall means raising `efSearch`, which spends the speedup
back. The 32.78× at 20k vectors is measured at 0.480 recall and is not a usable
operating point.

Writing the benchmark surfaced a methodology trap worth more than the index: uniform
random vectors are an invalid ANN corpus at 768 dimensions (concentration of measure
makes ground truth noise), and per-dimension Gaussian spread is not cluster spread
(`0.35 · sqrt(768) ≈ 9.7` swamps a unit centroid). Both produced plausible-looking,
meaningless numbers. Documented in full.

### Phase 4 — Reranking ✅ built

`internal/rerank` — `Reranker` interface with a listwise LLM implementation. Retrieval
widens to 40 candidates when a reranker is attached. Falls back to input order on any
failure, so an outage costs precision rather than availability; omitted passages are
appended so output is always a permutation of input.

**Deviation from the plan:** the resolved decision below called for a cross-encoder.
That needs an ONNX runtime or `llama.cpp` sidecar plus a model download — a second
inference dependency. The listwise reranker implements the same interface, so the
cross-encoder remains a constructor swap. Recorded as a deviation rather than quietly
substituted.

### Phase 5 — Query rewriting ✅ built

`internal/rewrite` — decomposition with a cheap syntactic gate (multiple question
marks, conjunctions, length) so simple questions never pay for a model call. Sub-query
rankings fused with RRF; the original question is always retained, so a bad rewrite can
only add candidates. Capped at 4 sub-queries.

### Phase 6 — Citation verification ✅ built

`internal/verify` — sentence-level claim extraction and per-claim entailment against
the retrieved passages, reporting a groundedness rate and listing unsupported claims.
Hedges are excluded from scoring so honest "not in your notes" answers aren't
penalised. Unparseable verdicts count as unsupported.

**Known limitation:** the judge is `llama3.2:3b` and it is unreliable. Observed a false
positive on this corpus — a claim supported by the retrieved table flagged unsupported.
Treat the percentage as a signal, not a measurement, until it runs on a larger model.

### Phase 7 — Agentic / multi-hop retrieval — ❌ not built (descoped)

Descoped by the project owner on 2026-08-14. The analysis below is kept because the
groundwork it describes is real and already in the repo — the MCP tool surface exists,
and an external agent (Claude Desktop) already drives an agentic loop over Engrex
today. What was descoped is the *internal* loop driven by the local model.

#### What it means

The current pipeline (`rag.Query`, `rag.go:198`) is a fixed sequence: embed → retrieve
top-k → stuff into prompt → generate. Every query takes the identical path. Retrieval
always runs, always once, always with the user's literal words.

Agentic RAG makes retrieval **a tool the model calls in a loop** rather than a step the
pipeline executes. The model decides whether to retrieve, what query to issue (often
not the user's phrasing), which tool to use, whether the results suffice, whether to go
again, and when to stop and answer.

The defining property: **control flow is decided at runtime by the model, not fixed at
design time.**

#### The spectrum

"Agentic RAG" is an umbrella over five distinct things with very different cost/value:

| Level | What it does | Where Engrex is |
|---|---|---|
| 1. Routing | Model picks which index or tool to query | Not present internally |
| 2. Query decomposition | Splits multi-part questions, retrieves each | Phase 5 |
| 3. Corrective (CRAG / Self-RAG) | Grades retrieved chunks; if weak, rewrites the query and retries. Self-RAG also grades its own output for groundedness | Overlaps Phase 6 |
| 4. Multi-hop / ReAct | Full retrieve → reason → retrieve loop, each hop informed by the last | This phase |
| 5. Full agent | Many tools, memory, open-ended planning | Out of scope |

Levels 2–4 hold the value. Level 5 is where projects become unreliable demos.

#### The tool layer already exists

`internal/mcpserver/mcpserver.go:92` already exposes exactly the toolset an agentic
loop needs, with descriptions and read-only annotations already written:

- `search_notes` — hybrid retrieval with a limit
- `get_document` — zoom from a chunk to full source text
- `query_knowledge_graph` — walk to semantically adjacent documents, with a `depth`

These are not approximations of agentic-RAG tools; they are the standard set. **Claude
Desktop already runs an agentic loop over Engrex today** via MCP. The missing piece is
an *internal* loop driven by the local model — not the tool surface.

The graph tool is the distinctive part. Most agentic RAG has a single search tool, so
the agent can only rephrase and retry. Engrex can traverse a semantic graph built at
ingest time, which is a genuine second-hop mechanism rather than a reworded first hop.

#### The two blockers

**1. The generation call cannot do tool calling.** `rag.go:226` posts to
`/api/generate` — the raw completion endpoint, no tool interface. This needs migrating
to `/api/chat` with a `tools` array and a message loop that handles `tool_calls` and
feeds results back as `role: "tool"` messages. That is a rewrite of `Query`, not a
tweak, and it also changes the streaming contract.

**2. `llama3.2` is the wrong model for the loop.** The default Ollama tag is the 3B.
Meta advertises tool calling for it, but at 3B multi-turn agentic reliability is poor
in practice — spurious tool calls, failure to terminate, ignoring results and answering
from priors. *Moderate-to-high confidence; verify before committing.*

Expected resolution: drive the loop with `qwen3` (strong tool use at 4B/8B) or
`llama3.1:8b`, optionally keeping `llama3.2` for final synthesis. Make the loop model
configurable in `internal/config` rather than another `const` in `rag.go`.

**Secondary cost — streaming.** `Query` currently streams tokens straight to the
caller. An agentic loop means dead air while it retrieves and reasons. The `protocol`
package's typed payloads are decent scaffolding for emitting step events instead, and
showing the reasoning trace is a better demo than a spinner.

#### Making it credible rather than buzzword

This is the most inflated item on the list. A reviewer who knows the space can spot a
`while` loop around a retriever quickly, and an unevidenced "added agentic RAG" reads
as padding. Two things make it the real version:

- **The graph traversal tool** — "multi-hop retrieval over a semantic knowledge graph
  the system built at ingest time" is specific, unusual, and already true here.
- **Evidence it is needed** — slice the Phase 0 eval set into single-hop and multi-hop
  questions. Show the agentic path wins on the multi-hop slice *and loses on latency
  everywhere else*. That comparison is what almost nobody has.

Keep it behind a flag and report both paths.

## Resolved decision — reranking via cross-encoder

Ollama exposes no rerank endpoint. The two options:

1. **Cross-encoder** (`bge-reranker-v2-m3`, `Qwen3-Reranker`) via ONNX Runtime bindings
   or a `llama.cpp` sidecar. Better quality, real plumbing, and a second runtime
   dependency in a project that currently has exactly one.
2. **Listwise LLM rerank** — a single `llama3.2` call that orders 40 candidates. Lower
   quality, no new dependency, far less code, ~1–3 s added latency.

Under the retrieval-quality objective, option 2 was the pragmatic pick. **Under the
stated objective, option 1 is correct.** Running a quantized cross-encoder in-process
and understanding why a cross-encoder beats a bi-encoder at reranking — joint
query-document attention versus independent embeddings — is substantially more
technical ground than prompting a chat model to sort a list.

Build option 1. Consider keeping option 2 behind the same interface as a fallback and
benchmarking both: the quality-versus-latency comparison is another free result.
