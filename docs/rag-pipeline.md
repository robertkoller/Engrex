# RAG Pipeline

RAG = Retrieval-Augmented Generation. Engrex answers questions by *retrieving* your
most relevant saved chunks and feeding them to a local LLM as context, so the answer
is grounded in your own content. The pipeline lives in `internal/rag/rag.go` and
wires together the chunker, embedder, and store.

## Ingest path: `rag.Add(text, source, origin)`

1. **Chunk** — `chunker.Chunk(text)` splits the text into overlapping segments
   (see [Chunking](#chunking)). It returns an error if the input exceeds the size cap.
2. **Embed** — each chunk is sent to Ollama's `/api/embed` (`internal/embedder`),
   returning a `[]float32` of length 768.
3. **Store** — how the chunks are written depends on whether the source is a
   *replaceable document* (a file or web page, identified by `store.DocumentIdentity`)
   or a *typed note* (`cli`/`hotkey`):
   - **Documents** take the re-ingestion path: if the text's SHA-256 matches the last
     ingest (`documents` table) the whole add is **skipped**; otherwise the document's
     previous chunks are **deleted** (`DeleteBySource`) and the fresh ones inserted in
     full (`InsertDocumentChunk`, no per-chunk dedup). This keeps an edited-and-resaved
     file from piling up stale, overlapping copies — see
     [ingestion.md](ingestion.md#re-ingestion-editing-a-file-in-place).
   - **Typed notes** use `store.Insert`, which does a KNN dedup check first: if the new
     vector is within 0.35 of an existing chunk it's skipped, otherwise it's written.
   Either way, each stored chunk is linked to its nearest neighbours (`store.relate`)
   to build the graph.
4. **Stub** — for non-file sources, a `.txt` copy is written to `~/Engrex/RawText/`
   so you can browse what you saved (see [ingestion.md](ingestion.md)).

## Query path: `rag.Query(out, question, maxDistance, topK)`

1. **Parse flags** — `parseQueryFlags` pulls `--date` / `--source` out of the question
   so they don't pollute the embedding, and records them as options.
2. **Embed the question** into a query vector.
3. **Hybrid retrieve** — two searches run over the same chunks and their rankings are
   fused:
   - **Vector** (`store.Search`) — KNN over `vec_chunks`, semantic matches within
     `maxDistance` (cosine).
   - **Keyword** (`store.KeywordSearch`) — BM25 full-text over the `fts_chunks` FTS5
     index, catching exact terms, proper nouns, and IDs that embeddings smear over. The
     raw question is first turned into a safe MATCH expression by `toFTSQuery` (each word
     quoted and OR'd, so punctuation and reserved words can't break the query); an empty
     result skips keyword search.
   - **Fuse** (`fuseRRF`) — each list contributes `1/(60 + rank)` to a chunk's score, so
     results ranked highly by either method — and especially both — rise to the top. Ranks
     are fused rather than raw scores, because cosine distance and BM25 aren't comparable.
   The fused list is capped at `topK`.
4. **Emit sources first** — the daemon writes `{"sources":[...]}` as the first line of
   the response (the deduped list of linkable file paths / URLs), then the answer.
   This is how the UI shows a clickable sources panel. See [the wire protocol](#query-wire-protocol).
5. **Build the prompt** — if chunks were found, `buildPrompt` assembles the system
   instructions + the retrieved chunks (with their date/source) + the question. If
   nothing relevant was found, `buildNoContextPrompt` asks the model to answer from
   general knowledge and label it `[outside knowledge]:`.
6. **Stream** — the prompt goes to Ollama's `/api/generate` with `"stream": true`, and
   tokens are written to `out` (the socket connection) as they arrive.

### Query wire protocol

A query response over the socket is:

```
{"sources":["/Users/you/Engrex/paper.pdf","https://example.com"]}\n   ← first line, JSON
<answer text streamed token by token…>                                ← everything after
```

The newline after the JSON is the delimiter. Clients read the first line as sources,
then treat the rest as the streaming answer. The CLI and the Swift `SocketClient`
both implement this split.

## Chunking

`internal/chunker/chunker.go`. The goal: **never cut a sentence in half**, and never
emit a chunk too big for the embedder.

- Text is split into **sentences** (`splitSentences`, a regex that keeps trailing
  `.!?` and also catches a final sentence with no terminator).
- Sentences are **packed** into a chunk until adding the next would exceed
  `chunkLength` (400 words).
- The next chunk is **seeded with overlap** — the trailing sentences of the previous
  chunk totalling about `chunkOverlap` (50 words) — so context isn't lost at the seam.
- **Guardrails:**
  - Input over `maxInputChars` (500,000) is rejected with an error.
  - A single sentence larger than `chunkLength` is **hard-split** by words
    (`splitLongSentence`) — the only case where a sentence gets broken, because a
    run-on has no boundaries to break on.
  - Empty/whitespace input yields zero chunks; nothing panics.

## Prompting

`buildPrompt` puts all instructions **first**, before the notes and the question.
This matters: instructions placed between the notes and the question tend to get
echoed back into the answer. The system prompt:

- Frames the notes as the user's **own private notes** and tells the model to answer
  directly from them without refusing or moralizing (fixes small-model refusals like
  declining to answer about personal content).
- Says to use *all* relevant notes, and to prefix any outside knowledge with
  `[outside knowledge]:`.
- Only when `--date`/`--source` are set: adds a concise, once-per-note citation
  instruction with a concrete format.

## Models

- **Embeddings:** `nomic-embed-text` (768 dims).
- **Generation:** `llama3.2`.

Both run in Ollama. Swapping the generation model is a one-line change
(`generateModel` in `rag.go`); a larger model (e.g. `llama3.1:8b`) follows
instructions more reliably at the cost of speed.
