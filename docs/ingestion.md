# Ingestion

There are four ways content gets into Engrex. All of them ultimately call
`rag.Add(text, source, origin)`.

## 1. CLI — `engrex add "text"`

The CLI sends `{"type":"add","text":"…","source":"cli"}` over the Unix socket. The
daemon ingests it with `source="cli"` and no origin. A `.txt` stub is written to
`~/Engrex/RawText/`.

## 2. Hotkey — ⌘⇧B (Swift app)

`AccessibilityReader` grabs the selected text from whatever app is focused:

- First it tries the **Accessibility API** (`kAXSelectedText`) — works in native apps,
  touches nothing.
- If that returns nothing (e.g. Chrome and other browsers don't expose selection via
  AX), it **falls back to synthesizing ⌘C**, reading the pasteboard, then restoring
  your previous clipboard contents.

The text is sent as `{"type":"add","source":"hotkey"}`. A toast confirms the save.

## 3. File watcher — `~/Engrex/`

The daemon watches `~/Engrex/` with `fsnotify` (`internal/watcher`). When a file is
created or written there, after a 500ms debounce it's ingested:

- `ingest.ExtractText(path)` reads the file and returns text based on type.
- Supported types (`ingest.IsSupported`):
  - `.md`/`.markdown` (inline formatting stripped, **structure kept** — see
    [Markdown handling](#markdown-handling)), `.html`/`.htm` (tags stripped)
  - `.pdf` (text extracted via PDFium, see [PDF extraction](#pdf-extraction))
  - `.docx` (unzipped and its WordprocessingML flattened to text — stdlib only, no
    dependency; see `extractDOCX`)
  - Plain-text and code/config files read as-is: `.txt`, `.go`, `.py`, `.js`, `.ts`,
    `.java`, `.c`, `.cpp`, `.rs`, `.sh`, `.json`, `.yaml`, `.yml`, `.toml`, `.csv`,
    `.tsv`, `.org`, `.rst`, `.tex`, `.log`
  - Anything else (and any file under `minFileSize`) returns empty and is skipped.
- The extracted text is added with `source = <the path in ~/Engrex>` and no origin
  (a Finder drop has no recoverable original location).

`~/Engrex/RawText/` is a subfolder but is **not** watched (the watch is
non-recursive), so the `.txt` stubs written there are never re-ingested.

Because you'll edit and re-save watched files, the watcher fires on the same path
repeatedly. Engrex handles that with document-level re-ingestion rather than blindly
appending — see [Re-ingestion](#re-ingestion-editing-a-file-in-place) below.

## Markdown handling

`cleanMarkdown` strips inline formatting noise — emphasis, inline code fences, image
tags, link URLs (keeping the link text), math delimiters, horizontal rules — while
**deliberately preserving headings, list markers, and blockquote markers**.

That preservation is the whole point. An earlier version stripped `^#{1,6}\s+` along
with everything else, which meant that by the time text reached the chunker every
heading had become an ordinary sentence indistinguishable from body text. Section-aware
chunking was impossible by construction: there was no structure left to split on.

With headings intact, `chunker.ChunkDocument` can split at section boundaries and give
every chunk a heading path — see [rag-pipeline.md](rag-pipeline.md#chunking).

## Content type classification

`ingest.ContentType(path)` maps a file to how it should be split:

| Extension | Content type | Split strategy |
|---|---|---|
| `.md`, `.markdown` | `markdown` | Heading boundaries, then sentences within a section |
| `.go`, `.py`, `.js`, `.ts`, `.java`, `.c`, `.cpp`, `.rs`, `.sh`, `.json`, `.yaml`, `.yml`, `.toml`, `.tex` | `code:<language>` | Top-level declarations, line-windowed fallback |
| everything else | `text` | Sentence packing |

PDF, DOCX, and HTML come back as `text` — whatever structure they had is gone by the
time extraction has flattened them to prose.

The code path exists because source files must never go through the sentence splitter.
It breaks on the `.` in method chains, decimals, and version numbers, producing chunks
that start and end mid-expression.

## PDF extraction

`internal/ingest/pdf.go`. PDFs are the one format where extraction is genuinely hard: a
PDF stores positioned glyphs, not sentences, so the spaces you see on screen are usually
*gaps* rather than space characters. The reader has to reconstruct word boundaries from
glyph geometry.

Engrex uses **PDFium** — the engine Chrome uses — through
[`go-pdfium`](https://github.com/klippa-app/go-pdfium), compiled to WebAssembly and run
via wazero. WebAssembly rather than the cgo build keeps this dependency-free: nothing to
`brew install`, and it builds with `CGO_ENABLED=0`.

### Why not a pure-Go reader

`ledongthuc/pdf` was used originally and **silently produced unusable text** on ordinary
academic PDFs. On a CVPR paper it returned every glyph at the same X coordinate with zero
width and zero font size:

```
ROW y=675  elements=463
  [0] S="Deep"      X=153.45 W=0.00 FontSize=0.00
  [1] S="Residual"  X=153.45 W=0.00 FontSize=0.00
```

With no widths there is nothing to infer spacing from, so the text came out as
`DeepResidualLearningforImageRecognitionKaimingHe…` — 1,015 tokens averaging 35.9
characters. `dslipak/pdf`, a fork of the same engine, was worse. PDFium reads the same
file correctly: 7,082 tokens averaging 5.2 characters.

The damage was not limited to readability. The chunker counts words by whitespace, so a
9-page paper looked like ~1,000 words and became 6 oversized chunks instead of 21; BM25
recall collapsed, because a merged run of words is a single unsearchable token
(`residual` matched 1 chunk before the fix, 17 after).

### Cost

Compiling the PDFium WebAssembly module takes several seconds, so the instance pool is
built **lazily on the first PDF** and reused for the life of the process. Ingesting no
PDFs costs nothing; the long-lived daemon pays it once. Extraction itself is fast
(~300 ms for 9 pages). The pool holds one instance, so concurrent PDF ingests queue.

### If you change extractors

Stored chunks keep whatever text was extracted at ingest time, so you have to re-ingest
to pick up an improvement. **Re-ingest the same way the document was originally added**,
or you will get a duplicate rather than a replacement — see
[Re-ingestion](#re-ingestion-editing-a-file-in-place) for why.

To find what is actually stored and under which key:

```bash
sqlite3 "file:$HOME/.engrex/engrex.db?mode=ro" -header -column \
  "SELECT COALESCE(NULLIF(origin,''),source) AS doc_key, COUNT(*) AS chunks,
          MIN(id) AS first_id, MAX(id) AS last_id
   FROM chunks GROUP BY doc_key;"
```

If a stale copy is already there, delete it by id range and keep the fresh one:

```bash
engrex delete 1-6
```

## 4. Browser extension — ⌘⇧E

The extension (`extension/`) POSTs to the daemon's HTTP endpoint at
`127.0.0.1:7777/capture` with `{text, url, title}`. It captures the current selection,
or the whole page's text if nothing is selected. The daemon ingests with
`source = title`, `origin = url`. See [daemon.md](daemon.md) for the endpoint.

## Adding a file through the app (the `addfile` command)

When you drag a file onto the query window (or use the upload panel), the Swift app
does **not** copy it itself. It sends `{"type":"addfile","text":"<original path>"}`
over the socket. The daemon then:

1. Rejects unsupported types up front (`ingest.IsSupported`).
2. Copies the file into `~/Engrex/` with a collision-safe name.
3. Extracts text and ingests it with `origin = <the original path>` — so the sources
   panel can reopen the file you actually dragged in.
4. Tells the watcher to **skip** that copy (`ingest.MarkPending` / `ClaimPending`), so
   it isn't ingested twice with an empty origin.

If the extracted text is empty (a supported but unreadable file), the copy is removed
and an error is returned, so you get a real "couldn't add file" message instead of a
false success.

## Re-ingestion (editing a file in place)

The same document usually gets ingested more than once — you edit a note in `~/Engrex/`
and save it, and the watcher fires again. Per-chunk dedup alone doesn't save you here:
the chunker packs the text into overlapping windows, so inserting a single line near
the top shifts every later window and produces *different* chunks that don't match the
old ones. Left unchecked, one edit would re-insert most of the document as brand-new
chunks, and old versions would accumulate forever.

So documents with a **stable identity** — files (keyed by path) and web pages (keyed by
URL), as decided by `store.DocumentIdentity` — are re-ingested as a unit:

1. **Hash check.** `rag.Add` takes a SHA-256 of the extracted text and compares it to
   the last-ingest hash in the `documents` table. If it matches, the whole add is
   **skipped** (`"Unchanged since last ingest — skipped."`) — no re-embedding, no writes.
2. **Replace on change.** If the hash differs (or the document is new), the previous
   version's chunks — and their graph edges — are deleted (`store.DeleteBySource`), then
   the fresh chunks are inserted **in full** (`store.InsertDocumentChunk`, which skips
   the cross-document 0.35 dedup so a re-ingested file never silently loses a chunk that
   happens to resemble one in another document).
3. **Record the new hash** (`store.UpsertDocument`) for next time.

Typed CLI/hotkey notes have no stable identity, so they're never replaced — they keep
the append-with-dedup behavior described above.

> Existing files ingested before this behavior existed have no `documents` row yet, so
> the first edit-and-save of each one takes the replace path and cleans out any stale
> duplicate chunks left over from earlier saves.

### The identity trap: the same file can become two documents

Replacement only happens when the **document key matches**, and the key depends on *how*
the file was added, not on where its bytes are:

| How it was added | `source` | `origin` | Document key |
|---|---|---|---|
| App / `addfile` (copies into `~/Engrex/`) | `~/Engrex/paper.pdf` | `~/Downloads/paper.pdf` | `~/Downloads/paper.pdf` |
| Dropped straight into `~/Engrex/` (watcher) | `~/Engrex/paper.pdf` | *(empty)* | `~/Engrex/paper.pdf` |

Both rows describe the same bytes in the same place, but they hash to different keys. So
copying a file into `~/Engrex/` that was originally added through the app does **not**
replace it — the watcher sees no origin, derives a different key, and stores a second
document. You end up with one entry labelled `Downloads/paper.pdf` and another labelled
`Engrex/paper.pdf`, the first holding the older text.

Nothing is corrupted when this happens, but retrieval now has two copies competing, and
the stale one can outrank the fresh one. Check with the `doc_key` query in
[If you change extractors](#if-you-change-extractors) and delete the outdated id range
with `engrex delete`.

## Source vs origin

- **`source`** — a display label (`cli`, `hotkey`, a path, or a page title).
- **`origin`** — the *real* location to reopen: the original file path you added, or a
  URL. Empty when unknown.

`collectSources` (in `rag`) builds the sources panel list, preferring `origin` and
keeping only things the UI can open — absolute file paths and `http(s)://` URLs.

## RawText stubs

For text that didn't come from a real file (CLI, hotkey, web captures), Engrex writes
a browsable `.txt` into `~/Engrex/RawText/`:

- **CLI/hotkey:** named from the first 20 characters of the text.
- **Web captures:** named from the page title, with a header prepended:
  ```
  Title: <page title>
  Source: <url>

  <captured text>
  ```

The header is in the *file only* — it's never re-ingested, so it doesn't pollute the
embeddings (which were computed from the body text at insert time).
