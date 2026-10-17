# The Daemon

`engrex daemon` is the long-running process that owns everything. It's assembled in
`internal/daemon/daemon.go` and runs four concurrent listeners, each in its own
goroutine, shutting them all down cleanly on `SIGTERM`/`SIGINT`.

```
daemon.Start()   → opens DB, builds store + rag, creates the four listeners
daemon.Run()     → go watcher.Start(); go socket.Start(); go server.Start(); go graph.Start()
                 → wait for signal
                 → on signal: watcher.Stop(); socket.Stop(); server.Stop(); graph.Stop(); db.Close()
```

Every `Start()` is launched with a bare `go`, so its error is discarded — a failed port
bind is silent, and the symptom is a listener that simply never answers.

## The four listeners

### 1. Unix socket — `internal/socket`

Listens on `~/.engrex/daemon.sock`. This is how the **CLI**, the **Swift app**, and the
**MCP bridge** talk to the daemon. Each connection carries one JSON `Command`
(`internal/protocol`):

```json
{ "type": "add | query | delete | addfile | search | document | graph | models",
  "text": "...", "source": "...", "limit": 0, "depth": 0, "model": "",
  "history": [{ "role": "user | assistant", "content": "..." }] }
```

Write and query commands:

- **add** — `rag.Add(text, source, "")`, replies `{}` or `{"error":"…"}`.
- **query** — streams the answer back (sources JSON line, then tokens). See
  [rag-pipeline.md](rag-pipeline.md#query-wire-protocol). Accepts an optional `model`
  (see below) and an optional `history`, the earlier turns of a chat, which the
  handler passes to `rag.Converse`. The daemon stores no conversations; see
  [rag-pipeline.md](rag-pipeline.md#conversations).
- **delete** — `store.Delete(spec)` where `spec` is like `1,5,7-9`.
- **addfile** — copies + ingests a file with origin (see [ingestion.md](ingestion.md)).
- **models** — reports the model names this daemon offers, as
  `{"default": "...", "deep": "...", "cloud": "..."}`. Clients use it so model names
  live in one place instead of being hardcoded into every UI. `cloud` is the Gemini
  model, and is left out when no Gemini key is found. The key is looked up on each
  `models` call, so adding one needs no restart.

### Per-query model selection

`query` takes an optional `model`. Empty means the daemon's configured default; a name
routes that one question elsewhere. This is how the app's **Deep thinking** toggle
works — a slower, more capable model for the questions that need it, with no daemon
restart and no saved-config change.

A `gemini-` model name sends that question's answer step, retrieved passages included,
to Google instead of Ollama. See [rag-pipeline.md](rag-pipeline.md#gemini-optional-cloud-answers).

The handler routes through `rag.WithModel`, which returns a **copy** of the pipeline
rather than mutating it. The daemon shares one `RAG` across concurrent connections, so
mutating in place would let one request's model choice leak into another's — or race
outright.

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
Two routes:

```
POST /capture        { "text": "...", "url": "...", "title": "...", "format": "markdown" | "" }
POST /capture-file?name=<file name>&url=<page url>&title=<title>   (body: the raw file)
```

`/capture` sets CORS headers (the extension is a different origin), handles the
`OPTIONS` preflight, and calls `rag.AddTyped(text, title, url, contentType)`. The title
falls back to the URL when empty. `format: "markdown"` (Google Docs exports) runs the
text through `ingest.CleanMarkdown` and chunks it on headings. Anything else is plain
text, with vertical tabs (how Slides' text export marks line breaks) turned into
newlines.

`/capture-file` takes a file the extension downloaded from Google Drive. It rejects
types `ingest.IsSupported` doesn't know (415) and bodies over 100 MB (413). It writes the
body to a temp file with the same extension, extracts it with the same code as a file
dropped into `~/Engrex`, deletes the temp file, and ingests with the file name as the
source. It sends no CORS headers, so a web page can't read its response. Details in
[ingestion.md](ingestion.md#google-drive-files).

**Known gap:** neither route checks who is calling. `/capture` answers any origin
(`Access-Control-Allow-Origin: *`). `/capture-file` doesn't check `Content-Type`, so a
page can send a `text/plain` POST, which skips the CORS preflight. Either way, any
website open in your browser could post text or a file into your notes. It can't read
anything back.

### 3. Graph server — `internal/graphserver`

Binds `127.0.0.1:7778`. Serves `/graph` and `/query` as JSON for the document-web view,
and the static assets behind it from `web/`, which `web/embed.go` embeds into the binary
(`http.FS(web.FS)`). The page works wherever the daemon was started, but a change to
`web/` only shows after a rebuild, `make install` and a daemon restart.

`/graph` comes from `store.GraphData`, which folds chunk-level `relations` into one edge
per document pair. See [data-model.md](data-model.md#relations--the-knowledge-graph-edges)
for when a pair gets an edge. The page (`web/app.js`) lays it out with a D3 force
simulation. Repulsion only reaches 260px (`distanceMax`) and a weak `forceX`/`forceY`
pull (0.08) keeps documents with no edges near the rest. The view zooms to fit all nodes
once, when the first layout settles.

### 4. File watcher — `internal/watcher`

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
