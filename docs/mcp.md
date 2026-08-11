# MCP interface

Engrex exposes its knowledge base to [Model Context Protocol](https://modelcontextprotocol.io)
clients — Claude Desktop and anything else that speaks MCP — as three read-only tools.

This is the fourth interface onto the daemon, next to the Unix socket (CLI + Swift app),
the localhost HTTP endpoint (browser extension), and the file watcher. It is a transport,
not a second retrieval pipeline.

---

## Where it sits

```
Claude Desktop
      │  spawns as a child process, talks JSON-RPC over stdin/stdout
      ▼
engrex mcp serve                 ← internal/mcpserver
      │  read-only commands: search / document / graph
      ▼
~/.engrex/daemon.sock            ← internal/socket
      │
      ▼
engrex daemon
      ├── rag.Retrieve()         ← the same hybrid search the CLI uses
      └── store.*                ← the same edges the graph UI draws
```

Two properties fall out of this shape, and both are the reason for it:

**One process owns SQLite.** `engrex mcp serve` never opens the database. It holds no
`*sql.DB`, runs no migrations, and loads no sqlite-vec. It dials the daemon's socket and
forwards a command, exactly as the CLI does. The daemon stays the single writer and the
single point of serialization — the invariant everything else in Engrex depends on.

**One retrieval implementation.** `search_notes` calls `rag.Retrieve`, which is the
retrieval half of `rag.Query` factored out. `engrex query` calls the same function and
then feeds the result to the LLM. There is no MCP-specific ranking to drift out of sync.

---

## Trust boundary

- **Local only, always.** stdio to a child process. No port is bound, no socket is
  exposed to the network, and there is no configuration that would do so.
- **Off by default.** `mcp_enabled` in `~/.engrex/config.json` starts false. The gate is
  checked in two places: `engrex mcp serve` refuses to start when disabled, and the
  daemon rejects MCP commands when disabled. The second check is what actually protects
  the data — it is read per request, so `engrex mcp disable` takes effect immediately
  without a daemon restart.
- **Read-only, by construction.** The three tools map onto three read-only socket
  commands. `add`, `addfile`, `delete`, and `clear` are not reachable through MCP.

### If write tools are added later

Ingest, delete, and re-index are deliberately absent from this first pass. Letting a
model write to the knowledge base is a materially different decision from letting it
read: a bad read wastes a turn, a bad write loses notes. Whenever those arrive they
should get their own opt-in (something like `mcp_write_enabled`, defaulting off and
independent of `mcp_enabled`), destructive operations should be individually gated rather
than bundled, and the tool annotations must stop claiming `readOnlyHint: true`.

---

## Tools

### `search_notes`

| Field | Type | Notes |
|---|---|---|
| `query` | string, required | Natural language or exact keywords |
| `limit` | int, optional | Defaults to 10, capped at 50 |

Runs hybrid retrieval: dense vector KNN over `vec_chunks` plus BM25 over `fts_chunks`,
merged with Reciprocal Rank Fusion (k=60). Returns ranked chunks:

```json
{
  "query": "how are vector and keyword results combined?",
  "count": 2,
  "results": [
    {
      "rank": 1,
      "chunk_id": 2,
      "document_id": "/Users/you/Engrex/rrf-notes.md",
      "path": "/Users/you/Engrex/rrf-notes.md",
      "chunk_index": 0,
      "snippet": "Engrex merges its vector search and its BM25 keyword search using ...",
      "score": 0.03278688524590164,
      "distance": 0.9163674712181091,
      "saved_at": "2026-08-04T21:01:33Z"
    }
  ]
}
```

`score` is the RRF score — a chunk ranked highly by both searches scores roughly twice
what a chunk ranked highly by one does. `distance` is the raw cosine distance and is `0`
for chunks that only the keyword search found. Snippets are capped at 600 runes.

### `get_document`

| Field | Type | Notes |
|---|---|---|
| `document_id` | string | The id from a search result or graph node — preferred |
| `path` | string | A file path, URL, or bare filename |

One of the two is required. Identifier resolution is deliberately forgiving: a chunk id,
`chunk:N`, the original path a file was added from, the path of the copy in `~/Engrex`,
and a trailing fragment such as a bare filename all resolve to the same document.

When the document is a file that still exists, the text is re-extracted from the file —
more faithful than the stored chunks, which are normalized during chunking. Web captures
and typed notes have no file to re-read, so their chunks are stitched back together with
the chunker's deliberate overlap removed. `content_source` says which happened. Content
over 200,000 runes is truncated and `truncated` is set.

`ingestion_hash` is the SHA-256 the daemon compares against to decide whether a re-saved
document actually changed; it is empty for typed notes, which have no stable identity.

### `query_knowledge_graph`

| Field | Type | Notes |
|---|---|---|
| `node_id` | int | A graph node id |
| `document_id` | string | The id from a search result |
| `depth` | int, optional | Defaults to 1, capped at 5 |

One of `node_id` / `document_id` is required. Reads the `relations` table — the
nearest-neighbor edges recorded when each chunk was inserted — and walks breadth-first
from the root. Nothing is recomputed, so a neighborhood query cannot disagree with the D3
visualization at `localhost:7778`. Each node reports its hop distance from the root;
edges carry both the stored cosine `distance` and `similarity` (`1 - distance`).

A document with no edges returns just itself, which is correct rather than an error: it
means nothing else stored is close enough to have crossed the edge threshold.

---

## Errors

Failures are returned as MCP tool errors (`isError: true` on the result, so the model can
see them and recover) rather than protocol-level failures. Every message leads with a
stable code:

| Code | Meaning |
|---|---|
| `invalid_input` | Empty or missing arguments. Retrying identically will fail identically. |
| `not_found` | Well-formed identifier, nothing stored under it. |
| `index_unavailable` | SQLite is locked or busy — usually an ingestion in flight. Retrying shortly works. |
| `embedder_unavailable` | Ollama is unreachable, so the query cannot be embedded. |
| `mcp_disabled` | The interface is off in the config. |
| `daemon_unavailable` | The daemon is not running. Produced client-side. |
| `internal_error` | Anything else. |

An empty result set is **not** an error: `search_notes` returns `count: 0`, which lets a
client distinguish "you have nothing saved about this" from "the search failed".

---

## Setup

```bash
engrex mcp enable    # opt in
engrex mcp status    # show state and config path
engrex mcp disable   # revoke
engrex mcp serve     # the stdio server; the MCP client runs this, not you
```

Claude Desktop, in `~/Library/Application Support/Claude/claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "engrex": {
      "command": "/usr/local/bin/engrex",
      "args": ["mcp", "serve"]
    }
  }
}
```

The absolute path matters: Claude Desktop is not launched from your shell and does not
inherit your `PATH`. `engrex daemon` must be running.

---

## Permanent setup (surviving a reboot)

### Don't run the MCP server as a service

`engrex mcp serve` is **not** a daemon and must not be put under launchd. It is a stdio
program: it reads JSON-RPC from its own stdin and writes to its own stdout, and it exists
only for as long as the client that spawned it. Launching it from launchd would leave a
process with nothing attached to either end, doing nothing, forever.

Claude Desktop starts it on launch and kills it on quit — that is already permanent, and
it is handled entirely by the `mcpServers` entry above. There is nothing to install.

What actually has to survive a reboot is everything *underneath* it:

| Layer | Persists how | Needs setup? |
|---|---|---|
| `mcpServers` entry | Claude Desktop's config file on disk | Once (above) |
| `mcp_enabled` | `~/.engrex/config.json` on disk | Once (`engrex mcp enable`) |
| `engrex mcp serve` | Spawned per session by the client | No |
| **`engrex daemon`** | **Nothing, by default** | **Yes — launchd** |
| **Ollama** | **Nothing, by default** | **Yes — `brew services`** |

The bottom two rows are the whole job. If the daemon isn't running, every tool call
returns `daemon_unavailable`; if Ollama isn't running, the daemon won't even start.

### 1. Ollama at login

Do this **first** — the daemon pings Ollama on startup (`rag.New`) and exits if it can't
reach it, so a daemon that starts before Ollama just dies.

```bash
brew services start ollama     # replaces running `ollama serve` by hand
brew services list             # ollama should read "started"
```

`brew services` writes its own launchd agent and starts it at login. If you use the
Ollama desktop app instead, enable its "launch at login" setting and skip this step.

### 2. The daemon at login

```bash
make install        # launchd runs /usr/local/bin/engrex, not bin/engrex
make daemon-start   # launchctl load the agent
make daemon-logs    # tail ~/.engrex/daemon.log to confirm it came up
```

**Stop any foreground `engrex daemon` first.** Both bind `~/.engrex/daemon.sock`; the
loser fails silently, because `socket.Start`'s error is discarded by the goroutine that
launches it. You get a daemon that looks alive and answers nothing.

The agent lives at `~/Library/LaunchAgents/com.robertkoller.engrex.plist`. It is **not
checked into the repo**, so a fresh clone has nothing for `make daemon-start` to load —
create it before running that target:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.robertkoller.engrex</string>
    <key>ProgramArguments</key>
    <array>
        <string>/usr/local/bin/engrex</string>
        <string>daemon</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/Users/YOU/.engrex/daemon.log</string>
    <key>StandardErrorPath</key>
    <string>/Users/YOU/.engrex/daemon.log</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
    </dict>
</dict>
</plist>
```

Replace `/Users/YOU` with your real home — launchd does not expand `~` or `$HOME` in
path values.

`KeepAlive` is a double-edged setting. It restarts the daemon if it crashes, which is
what you want; it also restart-loops it every few seconds if Ollama is down, which is why
step 1 comes first. A loop looks like repeated startup lines in `make daemon-logs`.

### 3. Verify the whole chain after a reboot

```bash
launchctl list | grep engrex    # loaded, and a PID (not just a label)
engrex mcp status               # enabled
echo '{"type":"search","text":"test","limit":1}' | nc -U ~/.engrex/daemon.sock
```

If the last command returns JSON, Claude Desktop will work. If it hangs or the socket is
missing, the daemon didn't come up — check `make daemon-logs`.

### Updating the binary later

`make install` removes and replaces `/usr/local/bin/engrex` rather than overwriting it in
place, so a running launchd daemon keeps its old inode and doesn't die of a broken code
signature (see [development.md](development.md#the-make-install-design)). But it keeps
running the *old* code until restarted:

```bash
make install
make daemon-stop && make daemon-start
```

Claude Desktop picks up the new binary on its next launch, since it re-spawns
`engrex mcp serve` each session.

### Turning it off

```bash
engrex mcp disable    # revoke MCP without touching the daemon; effective immediately
make daemon-stop      # stop the daemon entirely (breaks the CLI and extension too)
```

Removing the `mcpServers` entry from Claude Desktop's config stops it from spawning the
bridge at all. `engrex mcp disable` is the narrower control: the daemon keeps serving the
CLI, the Swift app, and the extension, and rejects only MCP.

---

## Verifying by hand

`engrex mcp serve` is a plain JSON-RPC-over-stdio program, so you can drive it directly.
Keep stdin open past the last request — the SDK's stdio transport closes the session as
soon as stdin hits EOF, and will drop replies still in flight. A real client holds stdin
open for the life of the session, so the trailing `sleep` only matters when poking at it
by hand:

```bash
{ printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"manual","version":"0"}}}' \
  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'; sleep 2; } | engrex mcp serve
```

To confirm MCP and the CLI really share a pipeline, compare the documents `search_notes`
ranks against the `Sources:` list from `engrex query` for the same question — they come
from the same `rag.Retrieve` call and should agree in both content and order.

---

## Retesting after a rebuild

Three separate processes cache the old build, and they have to be refreshed in this
order. Skipping one is the usual reason a fix "didn't take".

```bash
# 1. Install — the daemon and Claude Desktop both run /usr/local/bin/engrex,
#    not bin/engrex.
make install

# 2. Restart the daemon. It keeps running the old code until you do; `make install`
#    deliberately does not disturb a running process.
#    Foreground: Ctrl+C in its terminal, then `engrex daemon`
#    launchd:    make daemon-stop && make daemon-start

# 3. Restart Claude Desktop — a full quit (Cmd+Q), not just closing the window.
#    It spawns `engrex mcp serve` once per session and will keep the old child
#    process alive otherwise.
```

Then confirm each layer is actually the new one, from the bottom up:

```bash
pgrep -fl "engrex daemon"                  # daemon is running
engrex mcp status                          # enabled
echo '{"type":"search","text":"test","limit":1}' | nc -U ~/.engrex/daemon.sock
pgrep -fl "engrex mcp serve"               # Claude Desktop spawned the bridge
```

The `nc` line is the decisive one: `search` only exists in the new build, so a JSON reply
proves the running daemon is current. If it hangs or errors, step 2 didn't happen.

Finally, test from Claude Desktop itself with a message that must hit the index:

> Search my Engrex notes for what I saved about residual learning, then open the full
> document it came from.

That exercises `search_notes` and `get_document` together, and the chained call proves
`document_id` round-trips. A good answer quotes text with **real word spacing** — if it
comes back as `DeepResidualLearningfor…`, the daemon is still serving chunks extracted by
the old PDF reader and the document needs re-ingesting, not the code rebuilding.

For a negative control, ask for something you have definitely never saved. The correct
result is `count: 0` with no error.

---

## Code map

| Path | Role |
|---|---|
| `internal/mcpserver/mcpserver.go` | Tool schemas, handlers, and error mapping |
| `internal/mcpserver/client.go` | The daemon socket client every tool call goes through |
| `internal/protocol/` | Wire types and error codes shared by the daemon and the bridge |
| `internal/socket/readonly.go` | Daemon-side handlers for search / document / graph |
| `internal/config/` | The `mcp_enabled` toggle |
| `internal/store/document.go` | Document reassembly and graph-neighborhood queries |
| `cmd/engrex/mcp.go` | The `engrex mcp` commands |

The SDK is the official [`github.com/modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk).
