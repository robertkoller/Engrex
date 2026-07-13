# Architecture

## Processes

Engrex is not one program — it's a long-running **daemon** plus a set of thin
**clients** that talk to it, and **Ollama** doing the heavy ML.

```
        CLI (engrex ...)            Swift menu-bar app          Browser extension
              │                            │                           │
              │ Unix socket                │ Unix socket               │ HTTP POST
              │ ~/.engrex/daemon.sock      │                           │ 127.0.0.1:7777
              └──────────────┬─────────────┴───────────────┬───────────┘
                             ▼                              ▼
                     ┌───────────────────────────────────────────┐
                     │              Engrex daemon                 │
                     │  socket listener · HTTP server · watcher   │
                     │  ─────────────────────────────────────────│
                     │  rag: chunk → embed → store / search → LLM │
                     └───────────────┬─────────────────┬──────────┘
                                     │                 │
                          SQLite + sqlite-vec     Ollama (localhost:11434)
                          ~/.engrex/engrex.db     embeddings + generation
```

- **Daemon** — a single Go process (`engrex daemon`). It owns the SQLite database
  and the RAG pipeline, and runs three concurrent listeners (see [daemon.md](daemon.md)).
- **Clients** — the CLI, the Swift app, and the browser extension. None of them touch
  the database or Ollama directly for writes/queries; they send the daemon a command
  and render what comes back. (The CLI's read-only `list`/`clear`/`debug` are the
  exception — they open the DB directly.)
- **Ollama** — a separate local server. Engrex calls it for embeddings (`/api/embed`)
  and for answer generation (`/api/generate`).

## Why a daemon?

A single long-running process is the **one owner** of the database and the file
watcher. If every CLI invocation opened the DB and the watcher separately, you'd get
concurrent writers fighting over SQLite and duplicate ingestion. Centralizing on a
daemon means writes are serialized through one place, and the file watcher, socket,
and HTTP endpoint all share the same in-memory pipeline.

## Data flow: saving something

Take `engrex add "some text"` (the hotkey and extension are variations of this):

1. The CLI dials the Unix socket and sends `{"type":"add","text":"some text","source":"cli"}`.
2. The daemon's socket handler calls `rag.Add(text, source, origin)`.
3. `rag.Add` runs the text through the **chunker** (sentence-aware, overlapping chunks).
4. Each chunk is sent to **Ollama** to get a 768-dimension embedding vector.
5. Each `(text, vector)` pair is written to SQLite — the text into `chunks`, the
   vector into the `vec_chunks` virtual table — inside a transaction, after a
   dedup check. (Files and web pages take a document-level re-ingestion path instead;
   see [ingestion.md](ingestion.md#re-ingestion-editing-a-file-in-place).)
6. A `.txt` "stub" of the raw text is written to `~/Engrex/RawText/` for browsing.

## Data flow: asking a question

Take `engrex query "how does X work?"`:

1. The CLI sends `{"type":"query","text":"how does X work?"}` over the socket.
2. The daemon parses any `--date`/`--source` flags out of the question.
3. The question is embedded via Ollama into a query vector.
4. **Hybrid retrieval**: a `sqlite-vec` K-nearest-neighbours search over `vec_chunks`
   (semantic) and a BM25 full-text search over the `fts_chunks` FTS5 index (keyword) run
   in parallel, and their rankings are merged with Reciprocal Rank Fusion — so exact-term
   and semantic matches both surface. See [rag-pipeline.md](rag-pipeline.md).
5. The daemon sends the list of **source files** back as the first line of the
   response, then builds a RAG prompt (the retrieved chunks + the question) and
   streams the LLM's answer token-by-token over the same connection.
6. The client renders the streaming answer and the clickable sources.

See [rag-pipeline.md](rag-pipeline.md) for the details of each step.

## Where things live on disk

| Path | What |
|---|---|
| `~/.engrex/engrex.db` | The SQLite database (chunks + vectors) |
| `~/.engrex/daemon.sock` | The Unix socket the CLI/app connect to |
| `~/Engrex/` | The watched "inbox" — drop files here to ingest them |
| `~/Engrex/RawText/` | `.txt` stubs of CLI/hotkey/web captures, for browsing (not watched) |
| `~/Library/LaunchAgents/com.robertkoller.engrex.plist` | Optional launchd agent for auto-start |
