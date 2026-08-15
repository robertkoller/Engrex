# Data Model

Engrex stores everything in a single SQLite database at `~/.engrex/engrex.db`,
created and migrated automatically on first run (`internal/db/db.go`).

## Migrations

The schema is versioned with `PRAGMA user_version` and advanced by an ordered,
forward-only list of migrations, each applied in its own transaction so a failure
part-way leaves the database at its previous version rather than half-migrated.

| Version | What it does |
|---|---|
| 1 | Initial schema — chunks, vectors, relations, documents, FTS index |
| 2 | Chunk metadata columns (`heading_path`, `chunk_index`, `doc_title`, `content_type`) |
| 3 | Rebuild `vec_chunks` with an explicit cosine distance metric |

Databases created before versioning existed report `user_version` 0 but already have
the v1 schema, so `migrate` detects the existing `chunks` table and stamps them at 1
rather than re-running the initial migration against live data.

The migrations list is **append-only**. Editing or reordering a shipped entry makes
databases in the field disagree with the code about what a version means.

`store.Clear()` drops the tables and calls `db.Rebuild()`, which resets `user_version`
to 0 first — without that reset the migrations would be skipped and the database would
come back with no tables while still claiming to be fully migrated.

## Tables

### `chunks` — the text and its metadata

```sql
CREATE TABLE chunks (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    text         TEXT NOT NULL,
    source       TEXT NOT NULL DEFAULT 'cli',
    origin       TEXT NOT NULL DEFAULT '',
    created_at   DATETIME DEFAULT CURRENT_TIMESTAMP,
    heading_path TEXT    NOT NULL DEFAULT '',   -- v2
    chunk_index  INTEGER NOT NULL DEFAULT 0,    -- v2
    doc_title    TEXT    NOT NULL DEFAULT '',   -- v2
    content_type TEXT    NOT NULL DEFAULT ''    -- v2
);
```

- **`text`** — the chunk of content (one overlapping segment of a larger note).
- **`source`** — a human-readable label of where it came from: `"cli"`, `"hotkey"`,
  a file path (for watched/dropped files), or a page title (for browser captures).
- **`origin`** — where a file was *originally* added from, when known: the original
  path you dragged in, or a web URL. Empty for CLI/hotkey text and Finder drops.
  The UI's sources panel prefers `origin` over `source` so it can reopen the real file.
- **`created_at`** — ingestion timestamp, used for "saved on" citations.
- **`heading_path`** — the chain of enclosing headings, outermost first, joined with
  `" > "` (e.g. `"Deployment > Rollback > Manual steps"`). For code chunks this holds
  the language. Empty for content with no structure above it.
- **`chunk_index`** — the chunk's ordinal within its document, in reading order. This
  is what lets `documentChunkQuery` reassemble a document correctly.
- **`doc_title`** — the document's display name: a page title for web captures, a base
  filename for files, empty for typed notes.
- **`content_type`** — how the text was split: `"markdown"`, `"code:<language>"`, or
  `"text"`.

Chunks stored before v2 carry zero values in the metadata columns — they were chunked
before structure was preserved, so there is no correct value to infer. They pick up
real metadata on the next re-ingest.

Every chunk-returning query selects the same `chunkColumns` list and scans through
`scanChunk`, which is what stops the column list and the scan targets drifting apart
as columns get added.

### `vec_chunks` — the embedding vectors

```sql
CREATE VIRTUAL TABLE vec_chunks USING vec0(
    embedding float[768] distance_metric=cosine
);
```

This is a **`sqlite-vec` virtual table**, one 768-dimension float vector per chunk.
The link is by row id: a chunk's `id` in `chunks` is the `rowid` in `vec_chunks`, so
they join on `chunks.id = vec_chunks.rowid`.

`distance_metric=cosine` is explicit. **`vec0` defaults to L2**, which ranks identically
to cosine only for unit-length vectors. Engrex's embeddings are unit length, so nothing
about ranking changed when this was declared — but the numbers themselves live on a
different scale, which is why the thresholds had to be converted (see
[Distance thresholds](#distance-thresholds)).

Migration 3 drops and recreates this table empty rather than copying the old vectors,
because it shipped alongside the embedder adopting `nomic-embed-text`'s required task
prefixes. Vectors from the old scheme sit in a different space and would poison results.
Run `engrex reindex` after upgrading to re-embed from the chunk text already stored.

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

Edges are recorded **once, at insert time** — nothing recomputes similarity on read.
Both the web UI's graph and the MCP `query_knowledge_graph` tool read this table through
`store.GraphData`, so they cannot disagree about what is connected to what.

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

`doc_key` doubles as the **document id** exposed by the MCP tools — it is what
`search_notes` returns and `get_document` accepts, and `hash` is surfaced as
`ingestion_hash` so a caller can tell whether a document has changed since it last read
it. Documents with no row (typed notes) report an empty hash. See [mcp.md](mcp.md).

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

`sqlite-vec` **requires** a `LIMIT` (or `k = ?`) on the virtual table for KNN queries.
`store.knn` binds that limit as a parameter, so asking for a wider candidate pool
actually widens it — it was previously hardcoded to 20 while accepting a `topK`
argument that did nothing at the SQL layer.

The distance filter is applied to those `topK` results, which means a tight
`maxDistance` returns *fewer* than `topK` rather than reaching further down the
neighbour list. Filtering is a quality floor, not a way to backfill.

### Distance thresholds

`distance` is **cosine distance**: 0 means identical direction, larger means less
similar.

| Threshold | Value | Where | Purpose |
|---|---|---|---|
| Dedup | 0.061 | `store.deduplicationThreshold` | A new typed note within this of an existing chunk is skipped as a near-duplicate |
| Edge | 0.32 | `store.DefaultEdgeThreshold` | Neighbours closer than this become graph edges on insert |
| Search | 0.451 | `rag.DefaultSearchDistance` | Only chunks within this count as relevant vector hits |

These were originally calibrated as **L2** distances, back when `vec_chunks` used the
default L2 metric. Because the embeddings are unit length, the two metrics rank
identically — but they are on different scales:

```
L2² = 2(1 - cos)   and   cosineDistance = 1 - cos   ⟹   cosineDistance = L2² / 2
```

Each value above is the old L2 threshold (0.35 / 0.80 / 0.95) converted through that
identity, which preserves the previously tuned behaviour exactly. Left unconverted they
would have been 2–5x more permissive than intended — a cosine distance of 0.95 accepts
vectors that are very nearly orthogonal.

⚠️ **These are conversions, not fresh calibration.** Recalibrating properly needs a
corpus containing several unrelated topics, so the distance distributions of "related"
and "unrelated" pairs can actually be told apart. On a single-document corpus every
pair is related and there is no negative class to separate. See
[evaluation.md](evaluation.md).

Replaceable documents (files, web pages) skip the dedup check entirely — they are stored
in full and deduplicated at the document level instead (see
[ingestion.md](ingestion.md#re-ingestion-editing-a-file-in-place)).

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
