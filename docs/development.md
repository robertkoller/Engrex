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
`engrex daemon` at the same time — they'd both try to bind `~/.engrex/daemon.sock`.
Note the daemon needs Ollama running; if Ollama is down at login the daemon exits and
launchd may restart-loop it.

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

`clear` drops and recreates the tables (not just `DELETE`), so it rebuilds the schema
— run `engrex clear` after adding a column to pick it up on an existing DB. There's no
migration framework; either `clear`, or delete `~/.engrex/engrex.db` and let it
recreate.

## Inspecting state

```bash
sqlite3 ~/.engrex/engrex.db "SELECT id, source, origin, created_at FROM chunks;"
lsof -nP -iTCP:7777 -sTCP:LISTEN     # is the HTTP endpoint up?
launchctl list | grep engrex          # is the launchd daemon loaded?
```

## The Swift app & browser extension

- **App:** open `ui/EngrexUI/EngrexUI.xcodeproj` in Xcode → Run. See [ui.md](ui.md).
- **Extension:** `chrome://extensions` → Developer mode → Load unpacked →
  `extension/`. See `extension/README.md`.

## Package layout

```
cmd/engrex/            CLI entry point (cobra) + the socket client used by the CLI
internal/db/           SQLite open + migrate (sqlite-vec + FTS5)
internal/store/        insert, hybrid vector+BM25 search, graph edges, re-ingestion, delete
internal/embedder/     Ollama /api/embed client
internal/chunker/      sentence-aware chunking + guardrails
internal/rag/          the pipeline: add / query, rank fusion, prompts, sources, stubs
internal/ingest/       text extraction (md/txt/html/pdf/docx + code/config) + the socket/watcher hand-off registry
internal/watcher/      fsnotify watcher on ~/Engrex/
internal/socket/       Unix socket server
internal/httpserver/   localhost HTTP endpoint for the extension
internal/daemon/       ties the three listeners together
ui/                    the Swift menu-bar app (Xcode project)
extension/             the browser extension (vanilla JS, MV3)
docs/                  you are here
```
