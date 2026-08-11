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

Listens on `~/.engrex/daemon.sock`. This is how the **CLI**, the **Swift app**, and the
**MCP bridge** talk to the daemon. Each connection carries one JSON `Command`
(`internal/protocol`):

```json
{ "type": "add | query | delete | addfile | search | document | graph",
  "text": "...", "source": "...", "limit": 0, "depth": 0 }
```

Write and query commands:

- **add** — `rag.Add(text, source, "")`, replies `{}` or `{"error":"…"}`.
- **query** — streams the answer back (sources JSON line, then tokens). See
  [rag-pipeline.md](rag-pipeline.md#query-wire-protocol).
- **delete** — `store.Delete(spec)` where `spec` is like `1,5,7-9`.
- **addfile** — copies + ingests a file with origin (see [ingestion.md](ingestion.md)).

Read-only commands (`internal/socket/readonly.go`), added for MCP:

- **search** — `rag.Retrieve` + document annotation. `limit` caps results (default 10,
  max 50).
- **document** — resolves a loose identifier to a document and returns its full text
  plus metadata.
- **graph** — the neighborhood around a document, `depth` hops out (default 1, max 5).

These three are gated on `mcp_enabled` in `~/.engrex/config.json`, checked **per
request** — so `engrex mcp disable` takes effect without restarting the daemon. They
reply with a single JSON `Response`:

```json
{ "data": { … } }                                  ← success
{ "error": "…", "code": "not_found" }              ← failure
```

The `code` is one of `invalid_input`, `not_found`, `index_unavailable`,
`embedder_unavailable`, `mcp_disabled`, `internal_error` — a stable taxonomy so clients
can branch without matching on message text. See [mcp.md](mcp.md#errors).

Each connection is handled in its own goroutine so one slow request doesn't block
others. The read-only commands never write, so they run safely alongside an in-flight
ingestion under this model.

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

## The MCP bridge is not a fourth listener

`engrex mcp serve` runs as a **separate short-lived process spawned by the MCP client**,
not inside the daemon. It connects *in* to the socket listener above, exactly like the
CLI does.

That is deliberate. MCP clients spawn stdio subprocesses, so the daemon — already
running — could never be the thing they launch. Giving MCP its own port inside the
daemon would have meant a second network listener, contradicting the local-only
guarantee. Bridging through the existing socket keeps one process owning SQLite and adds
no listener at all. Details in [mcp.md](mcp.md).

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
