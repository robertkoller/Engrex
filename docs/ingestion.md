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

- `ingest.ExtractText(path)` reads the file and returns plain text based on type.
- Supported types (`ingest.IsSupported`):
  - `.md` (markdown stripped), `.html`/`.htm` (tags stripped)
  - `.pdf` (text extracted via `ledongthuc/pdf`)
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
