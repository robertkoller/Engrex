# Data Model

Engrex stores everything in a single SQLite database at `~/.engrex/engrex.db`,
created and migrated automatically on first run (`internal/db/db.go`).

## Tables

### `chunks` — the text and its metadata

```sql
CREATE TABLE IF NOT EXISTS chunks (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    text       TEXT NOT NULL,
    source     TEXT NOT NULL DEFAULT 'cli',
    origin     TEXT NOT NULL DEFAULT '',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
```

- **`text`** — the chunk of content (one overlapping segment of a larger note).
- **`source`** — a human-readable label of where it came from: `"cli"`, `"hotkey"`,
  a file path (for watched/dropped files), or a page title (for browser captures).
- **`origin`** — where a file was *originally* added from, when known: the original
  path you dragged in, or a web URL. Empty for CLI/hotkey text and Finder drops.
  The UI's sources panel prefers `origin` over `source` so it can reopen the real file.
- **`created_at`** — ingestion timestamp, used for "saved on" citations.

### `vec_chunks` — the embedding vectors

```sql
CREATE VIRTUAL TABLE IF NOT EXISTS vec_chunks USING vec0(
    embedding float[768]
);
```

This is a **`sqlite-vec` virtual table**. It stores one 768-dimension float vector
per chunk. The link between the two tables is by row id: a chunk's `id` in `chunks`
is used as the `rowid` in `vec_chunks`, so they're joined on `chunks.id = vec_chunks.rowid`.

### `relations` — the knowledge-graph edges

```sql
CREATE TABLE IF NOT EXISTS relations (
    source_id INTEGER NOT NULL,
    target_id INTEGER NOT NULL,
    distance  REAL NOT NULL,
    PRIMARY KEY (source_id, target_id)
);
```

When a chunk is inserted, `store.relate` finds its nearest neighbours (up to
`maxEdges`, within the `edgeThreshold` distance) and records an edge to each. The
graph server maps these chunk-level edges up to one node per document to draw the
knowledge graph. Deleting chunks (via `Delete` or `DeleteBySource`) also removes any
edges that touch them, so the table never keeps rows pointing at ids that no longer
exist.

### `fts_chunks` — the full-text (BM25) index

```sql
CREATE VIRTUAL TABLE IF NOT EXISTS fts_chunks USING fts5(
    text,
    content='chunks',
    content_rowid='id',
    tokenize='porter unicode61'
);
```

An **FTS5 external-content** table: it indexes `chunks.text` for keyword/BM25 search
without storing a second copy (`content='chunks'` points it at the `chunks` table, keyed
by `id`). Three triggers on `chunks` (`chunks_after_insert/delete/update`) keep it in
sync automatically, so no write path has to touch it. Query it with `MATCH` and rank by
`bm25(fts_chunks)`; this is the keyword half of hybrid retrieval (see
[rag-pipeline.md](rag-pipeline.md)). On first run after the index is added, `migrate`
backfills it from existing chunks with the FTS5 `'rebuild'` command.

### `documents` — content hashes for re-ingestion

```sql
CREATE TABLE IF NOT EXISTS documents (
    doc_key    TEXT PRIMARY KEY,
    hash       TEXT NOT NULL,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
```

One row per *replaceable* document — a file (keyed by its path) or a web page (keyed
by its URL). `doc_key` is the document's stable identity and `hash` is a SHA-256 of
its extracted text at last ingest. This is what lets a re-saved file be skipped when
unchanged, or have its old chunks replaced when it changed — see
[ingestion.md](ingestion.md#re-ingestion-editing-a-file-in-place). Typed CLI/hotkey
notes have no stable identity and get no row here.

## Why sqlite-vec

`sqlite-vec` is a SQLite extension that adds vector columns and K-nearest-neighbour
search directly in SQL. It lets Engrex do semantic search with a normal SQL query
instead of a separate vector database — everything stays in one file.

A KNN search looks like:

```sql
SELECT rowid, distance
FROM vec_chunks
WHERE embedding MATCH ?      -- the query vector, as a JSON array
ORDER BY distance
LIMIT 20;
```

`sqlite-vec` **requires** a `LIMIT` (or `k = ?`) on the virtual table for KNN
queries. That's why `store.Search` wraps the vector match in a subquery with a
`LIMIT 20`, then joins the result back to `chunks` for the text and metadata.

`distance` is **cosine distance**: 0 means identical direction, larger means less
similar. Engrex uses two thresholds, calibrated empirically for `nomic-embed-text`:

- **Dedup (0.35)** — for typed CLI/hotkey notes, if a new chunk is within 0.35 of an
  existing one it's skipped as a near-duplicate (`store.Insert`). Replaceable documents
  (files, web pages) don't use this — they're stored in full and deduplicated at the
  document level instead (see [ingestion.md](ingestion.md#re-ingestion-editing-a-file-in-place)).
- **Edge (`edgeThreshold`)** — on insert, neighbours closer than this become graph edges
  in `relations` (`store.relate`).
- **Search (~0.85–0.95)** — on query, only chunks within the threshold are treated as
  relevant (`DefaultSearchDistance` in `rag`). This is the vector half of retrieval; it's
  fused with a BM25 keyword search (`fts_chunks`) via Reciprocal Rank Fusion — see
  [rag-pipeline.md](rag-pipeline.md).

## The CGo requirement

`sqlite-vec` is C code that needs symbols from a real shared SQLite library, which
go-sqlite3's internal static build doesn't expose. So Engrex links against Homebrew's
SQLite via CGo flags set in the `Makefile` (`-tags libsqlite3` plus include/lib
paths). This is why you must build with `make`, never bare `go build` — see
[development.md](development.md).

## Inspecting the DB

```bash
sqlite3 ~/.engrex/engrex.db ".tables"
sqlite3 ~/.engrex/engrex.db "SELECT id, source, origin, created_at FROM chunks;"
```

(You can't meaningfully `SELECT` the raw vectors — they're an opaque virtual table.)
