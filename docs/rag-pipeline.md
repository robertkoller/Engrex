# RAG Pipeline

RAG = Retrieval-Augmented Generation. Engrex answers questions by *retrieving* your
most relevant saved chunks and feeding them to a local LLM as context, so the answer
is grounded in your own content. The pipeline lives in `internal/rag/rag.go` and wires
together the chunker, embedder, and store.

Three optional stages — query rewriting, reranking, and citation verification — sit
around this core and are documented separately in
[retrieval-stages.md](retrieval-stages.md). They are off by default.

## Ingest path: `rag.Add(text, source, origin)`

1. **Classify** — `ingest.ContentType(source)` decides how the text should be split:
   `"markdown"`, `"code:<language>"`, or `"text"`.
2. **Chunk** — `chunker.ChunkDocument(text, contentType)` splits the text using
   whatever structure that type provides (see [Chunking](#chunking)), returning pieces
   that each carry a heading path and an ordinal.
3. **Embed** — each chunk goes to Ollama's `/api/embed` via
   `embedder.EmbedDocument`, returning a unit-length `[]float32` of length 768.
4. **Store** — how the chunks are written depends on whether the source is a
   *replaceable document* (a file or web page, identified by `store.DocumentIdentity`)
   or a *typed note* (`cli`/`hotkey`):
   - **Documents** take the re-ingestion path: if the text's SHA-256 matches the last
     ingest (`documents` table) the whole add is **skipped**; otherwise the document's
     previous chunks are **deleted** (`DeleteBySource`) and the fresh ones inserted in
     full (`InsertDocumentChunk`, no per-chunk dedup). This keeps an edited-and-resaved
     file from piling up stale, overlapping copies — see
     [ingestion.md](ingestion.md#re-ingestion-editing-a-file-in-place).
   - **Typed notes** use `store.Insert`, which does a KNN dedup check first: if the new
     vector is within `deduplicationThreshold` of an existing chunk it's skipped.
   Either way, each stored chunk is linked to its nearest neighbours (`store.relate`)
   to build the graph, and its structural metadata is written alongside it.
5. **Stub** — for non-file sources, a `.txt` copy is written to `~/Engrex/RawText/`
   so you can browse what you saved (see [ingestion.md](ingestion.md)).

## Query path: `rag.Query(out, question, maxDistance, topK)`

1. **Parse flags** — `parseQueryFlags` pulls `--date` / `--source` out of the question
   so they don't pollute the embedding, and records them as options.
2. **Retrieve** — hand off to [`rag.Retrieve`](#retrieval-path), which returns the
   fused (and optionally reranked) chunk list.
3. **Emit sources first** — the daemon writes `{"sources":[...]}` as the first line of
   the response, then the answer. See [the wire protocol](#query-wire-protocol).
4. **Build the prompt** — `buildPrompt` assembles grounding rules, a document manifest,
   the retrieved passages, and the question (see [Prompting](#prompting)). If nothing
   relevant was found, `buildNoContextPrompt` asks the model to answer from general
   knowledge and label it `[outside knowledge]:`.
5. **Stream** — the prompt goes to Ollama's `/api/generate` with `"stream": true` and
   an explicitly sized `num_ctx` (see [Context sizing](#context-sizing-num_ctx)).
   Tokens are written to `out` as they arrive, and accumulated so verification can see
   the finished answer.
6. **Verify** (optional) — if a verifier is attached, the answer is checked against the
   passages it came from and a grounding report is appended.

### Query wire protocol

```
{"sources":["/Users/you/Engrex/paper.pdf","https://example.com"]}\n   ← first line, JSON
<answer text streamed token by token…>                                ← everything after
```

The newline after the JSON is the delimiter. Clients read the first line as sources,
then treat the rest as the streaming answer. The CLI and the Swift `SocketClient` both
implement this split.

This applies to `query` only. The read-only `search`/`document`/`graph` commands used
by MCP reply with a single JSON object and no stream — see
[daemon.md](daemon.md#1-unix-socket--internalsocket).

## Retrieval path

`rag.Retrieve(question, maxDistance, topK)` is factored out of `Query` so it can be
used without generation. `Query` calls it, the MCP `search_notes` tool calls it, and
the eval harness calls it — so **every interface ranks results identically** and there
is no second implementation to drift.

1. **Rewrite** (optional) — if a rewriter is attached, the question may be decomposed
   into sub-queries. The original is always retained.
2. **Embed the question** with `embedder.EmbedQuery`. Note this uses a *different task
   prefix* from stored chunks — see [Embedding](#embedding).
3. **Hybrid retrieve** — two searches run over the same chunks:
   - **Vector** (`store.Search`) — KNN over `vec_chunks`, keeping hits within
     `maxDistance` (cosine).
   - **Keyword** (`store.KeywordSearch`) — BM25 over the `fts_chunks` FTS5 index,
     catching exact terms, proper nouns, and IDs that embeddings smear over.
     `toFTSQuery` first turns the raw question into a safe MATCH expression (each word
     quoted and OR'd, so punctuation and reserved words can't break the query).
   - **Fuse** (`fuseRRF`) — each list contributes `1/(60 + rank)` to a chunk's score, so
     results ranked highly by either method — and especially both — rise to the top.
     Ranks are fused rather than raw scores, because cosine distance and BM25 aren't
     comparable. When several sub-queries ran, `fuseRankings` fuses all of them.
4. **Rerank** (optional) — with a reranker attached, retrieval widens to
   `rerankCandidates` (40) and the reranker cuts back to `topK`. Reranking always
   scores against the user's actual question, never a rewritten sub-query.

Each returned chunk carries its RRF `Score` alongside its vector `Distance`; `Distance`
is `0` for chunks only the keyword search found.

### How many passages reach the prompt

`DefaultSearchResults` is **5**. Measured on `qwen3:4b` at 5 vs 10: **56s vs 100s per
query** — roughly 1.8x faster — with no loss of correctness on hand-checked facts, and
one case where the smaller context produced the *better* answer because there was less
irrelevant text competing for attention.

That result comes from a single-document corpus, where any 5 passages are about as good
as any 10. It should be re-measured against the eval harness once the corpus spans
several topics.

### Reassembling a document from its chunks

Because chunks overlap by design, joining a document's chunks naively repeats every
seam. `store.stitchChunks` trims the longest exact overlap between each consecutive
pair before joining. This is what the MCP `get_document` tool falls back to for web
captures and typed notes; documents still on disk are re-read from the file instead,
which is more faithful.

## Chunking

`internal/chunker`. Two paths, chosen by content type.

### Structure-aware (`chunker.ChunkDocument`)

- **Markdown** splits at heading boundaries first, packing sentences only *within* a
  section. A heading stack tracks each chunk's full ancestry, and every chunk is
  **prefixed with its heading path** — so `"It defaults to 30 seconds"` embeds as
  `"Configuration > Timeouts: It defaults to 30 seconds"`, which lands near queries
  about timeout configuration instead of nowhere. Fenced code blocks are opaque, so a
  `#` inside one is a comment rather than a heading.
- **Code** splits at top-level declarations, with line-windowed fallback inside
  oversized functions. Source must never go through the sentence splitter — it would
  break on the `.` in method chains and decimals.
- **Plain text** falls back to sentence packing with no heading path.

### Sentence packing

- Sentences are packed until adding the next would exceed `chunkLength` (400 words).
- The next chunk is **seeded with overlap** — trailing sentences totalling about
  `chunkOverlap` (50 words) — so context isn't lost at the seam.
- Input over `maxInputChars` (500,000) is rejected.
- A single sentence larger than `chunkLength` is hard-split by words.

### Sentence splitting

`splitSentences` returns **slices of the original text** and never reassembles it. A
terminator only ends a sentence when whitespace or end-of-text follows, and a period
preceded by a known abbreviation or single-letter initial doesn't count.

This matters more than it sounds. The previous implementation matched sentences with a
regex and rejoined them with spaces, which silently rewrote `0.01` as `0. 01` and
`arXiv:1207.0580` as `arXiv:1207. 0580` — corrupting the stored text, its embedding,
and whatever the model read.

## Embedding

`nomic-embed-text` is an **asymmetric** retrieval model: it was trained with a task
prefix on every input, and stored text and queries get different ones.

- `embedder.EmbedDocument` prepends `search_document: `
- `embedder.EmbedQuery` prepends `search_query: `

Omitting them puts queries and documents in the same space rather than the paired
spaces the model was trained to produce, which degrades retrieval. `EmbedRaw` exists
only for diagnostics.

Vectors are normalized to unit length before storage. Ollama already returns unit
vectors for this model (`engrex doctor` reports the raw magnitude), so this is a
guarantee rather than a correction — but cosine distance is only well behaved on unit
vectors, and the guarantee is what keeps the distance thresholds meaningful.

## Prompting

`buildPrompt` structures the prompt for a small local model:

1. **Grounding rules first** — answer only from the context, never invent titles or
   numbers, say plainly when the notes don't cover something.
2. **A document manifest** — the distinct documents the passages came from, named once
   as their own section. Per-passage labels alone are not enough: questions like *"bring
   up my paper about X"* are about which document *exists*, and no individual passage
   ever states "this document is a paper titled X".
3. **The passages**, each labelled with document, section, and date.
4. **The question**, then **the constraint repeated**. Small models weight the end of a
   long prompt far more heavily than the beginning, so an instruction given only up
   front is effectively gone by generation time.

The previous prompt told the model to "supplement with your own knowledge" whenever the
notes fell short, prefixing outside claims with `[outside knowledge]:`. A 3B model reads
that as permission to answer from priors and then ignores the labelling rule, because
conditional formatting is exactly what models that size fail at. The escape hatch is now
a single narrow instruction to say what the notes *do* contain.

## Context sizing (`num_ctx`)

Ollama does **not** use a model's full architectural context unless asked. With no
`num_ctx` it allocates **4096 tokens** regardless of what the model supports, then
silently discards the **oldest** tokens of anything longer.

A RAG prompt puts its instructions first, so this ate the rules and the document
manifest while leaving the passages — the model answered having never seen what it was
told to do, and looked like it was ignoring instructions rather than never receiving
them. Confirmed via `prompt_eval_count`: **4095 tokens evaluated out of ~7053**.

`contextWindowFor` now sizes the window from the prompt: floor 4096, ceiling 32768,
3 chars/token (deliberately pessimistic — guessing low costs a little memory, guessing
high silently truncates), plus 1024 reserved for the answer.

Use `engrex debug-prompt "<question>"` to see exactly what the model receives. It
separates "wrong context" from "right context, bad answer" — two failures that look
identical from the answer alone.

## Models

- **Embeddings:** `nomic-embed-text` (768 dims).
- **Generation:** configurable, default `llama3.2`.

Generation model resolution, in precedence order:

```
--model flag  →  ENGREX_GENERATE_MODEL  →  ~/.engrex/config.json  →  llama3.2
```

Resolved **once at construction**, so answering, reranking, rewriting, and verifying
all use the same model — an environment change mid-run can't split one query across
two.

Model choice is the largest single lever on answer quality here. Measured on the same
question and corpus:

| Model | RAM resident | Names the saved document correctly |
|---|---|---|
| `llama3.2:3b` | 3.2 GB | ❌ invents papers instead |
| `qwen3:4b` | 4.0 GB | ✅ |
| `qwen3:8b` | 6.4 GB | ✅ (but no better than 4b, and slower) |

Cold start (loading a model into RAM) costs ~1 second against a ~90 second query, so
it is not worth optimizing. Generation speed is.
