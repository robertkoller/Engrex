# Development

## Prerequisites

- macOS (Apple Silicon or Intel)
- Go 1.21+ (the chunker uses the builtin `min`, which is 1.21+)
- Xcode Command Line Tools (for CGo)
- Homebrew
- Ollama, with the two models pulled:
  ```bash
  brew install go sqlite ollama
  ollama pull nomic-embed-text
  ollama pull llama3.2
  ```

## Always build with `make`

`sqlite-vec` needs symbols from Homebrew's SQLite, so the build must link against it.
The `Makefile` sets the CGo flags (`-tags libsqlite3` plus Homebrew include/lib
paths). **Bare `go build` / `go test` will fail with linker errors.**

```bash
make build     # → bin/engrex
make install   # rm + cp bin/engrex to /usr/local/bin/engrex
make test      # go test with the right tags
```

## The `make install` design

`install` does `sudo rm -f` then `sudo cp` — **not** a plain overwrite. Overwriting a
running, code-signed binary in place corrupts the kernel's signature mapping and the
next launch dies with **"Killed: 9"**. Removing first gives a fresh inode, so a running
daemon (foreground or launchd) keeps its old inode while the new binary is clean.

If you ever see `zsh: killed engrex …`: the installed binary got overwritten in place.
Fix: `sudo rm -f /usr/local/bin/engrex && sudo cp bin/engrex /usr/local/bin/engrex`.

## The launchd agent

For background auto-start on login:

```bash
make daemon-start   # launchctl load the plist
make daemon-stop    # launchctl unload
make daemon-logs    # tail ~/.engrex/daemon.log
```

The plist is `~/Library/LaunchAgents/com.robertkoller.engrex.plist`. It runs
`engrex daemon` at login. **Don't** run the launchd daemon and a foreground
`engrex daemon` at the same time — they'd both try to bind `~/.engrex/daemon.sock`, and
the loser fails silently because `socket.Start`'s error is discarded by the goroutine
that launches it.

Note the daemon needs Ollama running; if Ollama is down at login the daemon exits and
`KeepAlive` restart-loops it. Start Ollama at login too (`brew services start ollama`).

The plist is **not checked into the repo** — a fresh clone has nothing for
`make daemon-start` to load. Its contents, and the full boot-survival checklist (Ollama →
daemon → verify), are in [mcp.md](mcp.md#permanent-setup-surviving-a-reboot). That
section is written for MCP but the daemon half applies to every interface.

## Typical dev loop

```bash
# terminal 1 — Ollama
ollama serve

# terminal 2 — the daemon (foreground, so you see logs and can Ctrl+C)
make install
engrex daemon

# terminal 3 — try things
engrex add "test note"
engrex query "test"
```

After changing daemon code: `make install`, then Ctrl+C the foreground daemon and
re-run it (a running process doesn't pick up a new binary).

## Schema changes

There **is** a migration framework now — `PRAGMA user_version` with an ordered,
forward-only list in `internal/db/db.go`, one transaction per step. To add a column:

1. Append a `migration` entry with the next version number. **Never edit or reorder a
   shipped entry** — databases in the field would disagree with the code about what
   that version means.
2. Bump the `SchemaVersion` constant.
3. If the new column feeds chunk queries, add it to `chunkColumns` and `scanChunk` in
   `internal/store/store.go` — every chunk-returning query shares both, which is what
   stops the column list and scan targets drifting apart.

`engrex clear` still drops and rebuilds everything via `db.Rebuild()`, which resets
`user_version` to 0 first so the migrations re-run.

Anything that changes how text is *embedded* (model, task prefixes, normalization)
invalidates every stored vector — old vectors sit in a different space from new query
vectors. Run `engrex reindex` to re-embed from the chunk text already in the database;
nothing is re-read from disk and ids, sources, and metadata are preserved.

## Testing

```bash
make test                                   # all packages
go test -tags libsqlite3 ./internal/hnsw/   # one package
go test -tags libsqlite3 -run TestRecall -v ./internal/hnsw/
```

Tests that need Ollama (`internal/rag`, `internal/store` integration cases) **skip**
rather than fail when it isn't reachable, so the suite is green offline. The
`rerank`/`rewrite`/`verify` packages test against an `httptest` server serving canned
completions, so they never need a real model.

## Inspecting state

```bash
sqlite3 ~/.engrex/engrex.db "SELECT id, source, origin, chunk_index, content_type FROM chunks;"
sqlite3 ~/.engrex/engrex.db "PRAGMA user_version;"   # schema version
lsof -nP -iTCP:7777 -sTCP:LISTEN     # is the HTTP endpoint up?
launchctl list | grep engrex          # is the launchd daemon loaded?
engrex mcp status                     # is the MCP interface enabled?
engrex doctor                         # embedding + index health, schema version
ollama ps                             # which models are resident, and their unload timer
```

## Diagnosing a bad answer

Work outward from the data, not inward from the model. The order that matters:

```bash
engrex doctor                          # is the index even consistent?
engrex debug "your question"           # what did retrieval actually return?
engrex debug-prompt "your question"    # what did the model actually receive?
```

`debug-prompt` is the one that saves the most time — it separates *"the model was given
the wrong context"* from *"the model was given the right context and answered badly"*,
two failures that look identical from the answer alone. A long stretch of this project
was spent blaming a model for ignoring instructions it had never received, because
Ollama was silently truncating them away.

## Working on MCP

The socket is the seam, so you can test the daemon half without an MCP client at all:

```bash
# hit a read-only command directly (requires `engrex mcp enable`)
echo '{"type":"search","text":"goroutines","limit":3}' | nc -U ~/.engrex/daemon.sock
```

For the MCP half, drive `engrex mcp serve` with raw JSON-RPC — keep stdin open past the
last request, since the SDK's stdio transport tears the session down on EOF. Full recipe
in [mcp.md](mcp.md#verifying-by-hand).

To test without touching your real knowledge base, point `HOME` at a scratch directory:
`HOME=/tmp/engrex-test engrex daemon`. Keep the path **short** — macOS caps Unix socket
paths at ~104 bytes, and a long one makes the daemon fail to bind with no visible error
(`socket.Start`'s error is discarded by the goroutine that launches it).

## The Swift app & browser extension

- **App:** open `ui/EngrexUI/EngrexUI.xcodeproj` in Xcode → Run. See [ui.md](ui.md).
- **Extension:** `chrome://extensions` → Developer mode → Load unpacked →
  `extension/`. See `extension/README.md`.

## Package layout

```
cmd/engrex/            CLI entry point (cobra) + the socket client used by the CLI
                       main.go / mcp.go / eval.go (eval, ask, reindex, doctor,
                       debug-prompt) / bench.go (bench-hnsw)
internal/db/           SQLite open + versioned migrations (sqlite-vec + FTS5)
internal/store/        insert, hybrid vector+BM25 search, graph edges, re-ingestion, delete
internal/embedder/     Ollama /api/embed client — task prefixes + unit normalization
internal/chunker/      chunker.go: sentence packing + the sentence splitter
                       document.go: structure-aware markdown/code chunking
internal/rag/          the pipeline: add / query, rank fusion, prompts, context sizing
internal/ingest/       text extraction (md/txt/html/pdf/docx + code/config), content-type
                       classification, and the socket/watcher hand-off registry
internal/eval/         golden set, recall@k / MRR scoring, baselines, report rendering
internal/hnsw/         from-scratch HNSW index + its benchmark harness
internal/rerank/       Reranker interface + listwise LLM implementation
internal/rewrite/      query decomposition and expansion
internal/verify/       claim extraction + per-claim entailment against passages
internal/watcher/      fsnotify watcher on ~/Engrex/
internal/socket/       Unix socket server (+ readonly.go: the search/document/graph commands)
internal/httpserver/   localhost HTTP endpoint for the extension
internal/mcpserver/    MCP stdio server — read-only tools bridged to the daemon socket
internal/protocol/     socket wire types + error codes, shared by the daemon and clients
internal/config/       ~/.engrex/config.json — MCP toggle + generation model
internal/daemon/       ties the three listeners together
eval/                  golden.json (question set) + baseline.json (committed numbers)
ui/                    the Swift menu-bar app (Xcode project)
extension/             the browser extension (vanilla JS, MV3)
docs/                  you are here
```

## Command reference

| Command | What it does |
|---|---|
| `add` / `query` / `list` / `delete` / `clear` | Core operations, over the daemon socket |
| `ask` | Query **in-process** with opt-in stages (`--rerank`, `--rewrite`, `--verify`, `--all`, `--model`, `--top-k`) |
| `eval` | Score retrieval against the golden set (`--rerank`, `--rewrite`, `--save`) |
| `bench-hnsw` | Benchmark the HNSW index against exact search |
| `doctor` | Embedding magnitude, schema version, chunk/vector counts |
| `reindex` | Re-embed every chunk and rebuild graph edges |
| `debug` / `debug-edges` / `debug-prompt` | Raw distances, edge distribution, assembled prompt |
| `reindex-edges` | Rebuild graph edges at a given threshold |
| `daemon` / `mcp` | Run the daemon; manage the MCP interface |

`ask` runs in-process rather than through the daemon deliberately: the optional stages
are per-invocation experiments, and threading a matrix of flags through the daemon
protocol would fix a configuration the daemon owns rather than one the caller chooses.
