# The Daemon

`engrex daemon` is the long-running process that owns everything. It's assembled in
`internal/daemon/daemon.go` and runs three concurrent listeners, each in its own
goroutine, shutting them all down cleanly on `SIGTERM`/`SIGINT`.

```
daemon.Start()   → opens DB, builds store + rag, creates the three listeners
daemon.Run()     → go watcher.Start(); go socket.Start(); go server.Start(); wait for signal
                 → on signal: watcher.Stop(); socket.Stop(); server.Stop(); db.Close()
```

## The three listeners

### 1. Unix socket — `internal/socket`

Listens on `~/.engrex/daemon.sock`. This is how the **CLI** and the **Swift app** talk
to the daemon. Each connection carries one JSON `Command`:

```json
{ "type": "add | query | delete | addfile", "text": "...", "source": "..." }
```

- **add** — `rag.Add(text, source, "")`, replies `{}` or `{"error":"…"}`.
- **query** — streams the answer back (sources JSON line, then tokens). See
  [rag-pipeline.md](rag-pipeline.md#query-wire-protocol).
- **delete** — `store.Delete(spec)` where `spec` is like `1,5,7-9`.
- **addfile** — copies + ingests a file with origin (see [ingestion.md](ingestion.md)).

Each connection is handled in its own goroutine so one slow request doesn't block
others.

### 2. HTTP server — `internal/httpserver`

Listens on `127.0.0.1:7777` (loopback only — nothing is exposed to the network). It
exists because **browsers can't speak Unix sockets**, so the extension needs HTTP.
One route:

```
POST /capture   { "text": "...", "url": "...", "title": "..." }
```

It sets CORS headers (the extension is a different origin), handles the `OPTIONS`
preflight, and on success calls `rag.Add(text, title, url)`.

### 3. File watcher — `internal/watcher`

Watches `~/Engrex/` for saves and ingests them. Details in
[ingestion.md](ingestion.md#3-file-watcher--engrex).

## Why the CLI still opens the DB sometimes

`add`, `query`, `delete`, and `addfile` all go **through the daemon** (so writes are
serialized and files are handled with origin). But `list`, `clear`, and `debug` open
the SQLite database directly from the CLI process — they're read-only or
intentional-destructive and don't need the daemon's coordination. (SQLite handles the
concurrent read access; a schema-changing `clear` propagates to the daemon's
connection automatically.)

## Running the daemon

**Development (recommended):** run it in a terminal so you see logs and can Ctrl+C:

```bash
engrex daemon
```

**Background (auto-start on login):** the launchd agent —

```bash
make daemon-start   # load the launchd agent
make daemon-stop    # unload it
make daemon-logs    # tail ~/.engrex/daemon.log
```

Don't run both at once — they'd both try to bind the same socket. See
[development.md](development.md) for the launchd details and the "Killed: 9" gotcha.

## Requirements

The daemon calls Ollama on startup (`rag.New` pings it) and will fail to start if
Ollama isn't running. Start Ollama first (`ollama serve`).
