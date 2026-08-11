# Engrex

### Your memory, on your machine.

Engrex is a local-first AI second brain. Everything you deliberately save — text you select, notes you write, pages you read — gets embedded and indexed on your machine. Ask it anything in natural language and it surfaces your own thoughts back to you. No cloud. No subscription. No data leaving your device.

---

## How It Works

Capture content any way you like — select text and hit ⌘⇧B, drop a file into
`~/Engrex/`, or save a web page with the browser extension (⌘⇧E). Engrex splits it
into overlapping, sentence-aware chunks, embeds each chunk with a local model, and
stores the vectors in a local SQLite database (via `sqlite-vec`). Later, open the
query window with ⌘⇧Space (or `engrex query`), ask a natural-language question, and
Engrex retrieves your most relevant chunks — fusing semantic (vector) and keyword
(BM25) search — and answers with a local LLM, entirely on-device. Answers come with
clickable **sources** so you can jump back to the original file or page.

A single background **daemon** owns the database and RAG pipeline and listens on a
Unix socket (CLI + app), a localhost HTTP endpoint (extension), and a file watcher
(`~/Engrex/`). Full details in **[docs/](docs/)**.

---

## Architecture

```
┌─────────────────────────────────────────────────────┐
│                    Your Machine                     │
│                                                     │
│  ┌─────────────┐  ┌──────────────┐  ┌───────────┐  │
│  │  Hotkey UI  │  │   Browser    │  │   File    │  │
│  │  (Swift)    │  │  Extension   │  │  Watcher  │  │
│  └──────┬──────┘  └──────┬───────┘  └─────┬─────┘  │
│         │                │                │         │
│         └────────────────▼────────────────┘         │
│                   Unix Socket / HTTP                 │
│                          │                          │
│              ┌───────────▼───────────┐              │
│              │    Engrex Daemon      │              │
│              │                       │              │
│              │  - Chunk & embed      │              │
│              │  - Hybrid search      │              │
│              │  - RAG pipeline       │              │
│              │  - Graph relations    │              │
│              └───────────┬───────────┘              │
│                          │                          │
│         ┌────────────────┼────────────────┐         │
│         │                │                │         │
│  ┌──────▼──────┐  ┌──────▼──────┐  ┌─────▼──────┐  │
│  │   SQLite    │  │   Ollama    │  │  Web UI    │  │
│  │ + sqlite-   │  │ (local LLM  │  │  (Graph    │  │
│  │   vec       │  │ + embeddings│  │   viz)     │  │
│  └─────────────┘  └─────────────┘  └────────────┘  │
└─────────────────────────────────────────────────────┘
```

### Internal package structure

```
engrex/
├── cmd/engrex/           # CLI entry point — cobra commands + the socket client
├── internal/
│   ├── db/               # Opens SQLite, loads sqlite-vec + FTS5, runs migrations
│   ├── chunker/          # Sentence-aware overlapping chunks + size guardrails
│   ├── embedder/         # Calls Ollama /api/embed, returns []float32
│   ├── store/            # Insert, hybrid vector+BM25 search, graph edges, re-ingestion, delete
│   ├── rag/              # Wires chunker + embedder + store + LLM; rank fusion, prompts, sources
│   ├── ingest/           # Text extraction (md/txt/html/pdf/docx + code/config) + socket↔watcher hand-off
│   ├── watcher/          # fsnotify watcher on ~/Engrex/
│   ├── socket/           # Unix socket server (CLI + Swift app) + read-only MCP commands
│   ├── httpserver/       # localhost HTTP endpoint (browser extension)
│   ├── mcpserver/        # MCP stdio server — read-only tools bridged to the daemon
│   ├── protocol/         # Socket wire types + error codes shared by daemon and clients
│   ├── config/           # ~/.engrex/config.json (the MCP toggle)
│   └── daemon/           # Ties the three listeners together
├── ui/                   # Swift menu-bar app (Xcode project)
├── extension/            # Browser extension (vanilla JS, Manifest V3)
├── docs/                 # Architecture & component docs
└── Makefile
```

See **[docs/](docs/)** for a full walkthrough of how each part works.

---

## Tech Stack

| Layer | Technology |
|---|---|
| Core daemon | Go |
| Embeddings | Ollama — `nomic-embed-text` |
| LLM | Ollama — `llama3.2` |
| Vector store | SQLite + `sqlite-vec` |
| Keyword search | SQLite `FTS5` (BM25) |
| PDF text extraction | PDFium via `go-pdfium` (WebAssembly, no cgo) |
| MCP interface | `modelcontextprotocol/go-sdk` (stdio) |
| Menubar + hotkey UI | Swift (Phase 3) |
| Graph visualization | React + D3 (Phase 5) |
| Browser extension | Vanilla JS (Phase 4) |

---

## Prerequisites

- macOS (Apple Silicon or Intel)
- Go 1.21 or later
- Xcode Command Line Tools
- Homebrew

---

## Setup

### 1. Install Xcode Command Line Tools

Required for CGo (go-sqlite3 compiles SQLite from C source).

```bash
xcode-select --install
```

### 2. Install Homebrew dependencies

```bash
brew install go sqlite ollama
```

### 3. Pull the Ollama models

```bash
ollama pull nomic-embed-text   # embedding model (~270MB)
ollama pull llama3.2           # generation model (~2GB)
```

### 4. Clone the repo

```bash
git clone https://github.com/robertkoller/engrex
cd engrex
```

### 5. Install Go dependencies

```bash
go mod download
```

---

## Running

### 1. Start Ollama

Ollama must be running before the daemon starts (the daemon pings it on startup).

```bash
ollama serve
```

### 2. Install and start the daemon

```bash
make install     # builds bin/engrex and installs to /usr/local/bin
engrex daemon    # run in its own terminal so you see logs; Ctrl+C to stop
```

### 3. Use it

```bash
# Save something (goes through the daemon)
engrex add "Go uses goroutines for concurrency, not OS threads"

# Ask a question — streams an answer from your own saved content, with sources
engrex query "how does Go handle concurrency?"

# Add date/source citations to the answer
engrex query "how does Go handle concurrency? --source --date"

# Drop files into ~/Engrex/ to ingest them
# (.md .txt .html .pdf .docx, plus common code/config files like .go .py .json .csv)
# Or use the Swift app (⌘⇧Space) and the browser extension (⌘⇧E)

engrex --help    # all commands
```

For background auto-start on login, use `make daemon-start` instead of running the
daemon in a terminal (see [docs/development.md](docs/development.md)).

---

## MCP (Claude Desktop and other MCP clients)

Engrex speaks the [Model Context Protocol](https://modelcontextprotocol.io), so an MCP
client can search your knowledge base as a tool. It is a fourth interface onto the same
daemon — alongside the Unix socket, the HTTP endpoint, and the file watcher — not a
second copy of the pipeline: every tool call is forwarded over the daemon's existing
socket, so an MCP client gets the exact same hybrid retrieval the CLI does.

**This stays local.** The MCP server talks over stdio to a child process that the client
spawns; it binds no port and accepts no network connections. Nothing leaves the machine.

### Enable it

Off by default — MCP is local-only, but it is still another way into your notes, so you
have to opt in:

```bash
engrex mcp enable    # writes mcp_enabled: true to ~/.engrex/config.json
engrex mcp status    # check the current state
engrex mcp disable   # revoke it; takes effect on the next request, no restart needed
```

### Claude Desktop config

Add this to `~/Library/Application Support/Claude/claude_desktop_config.json`, then
restart Claude Desktop. Use the absolute path — the app is not launched from your shell,
so it does not inherit your `PATH`.

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

`engrex daemon` must be running; the MCP process is only a bridge to it.

### The tools

All three are read-only. Ingestion, deletion, and re-indexing are deliberately not
exposed — letting a model write to your knowledge base is a separate trust decision, and
if write tools are ever added they will sit behind their own explicit opt-in rather than
arriving with this one.

| Tool | Input | Returns |
|---|---|---|
| `search_notes` | `query` (required), `limit` (default 10, max 50) | Ranked chunks with `document_id`, path, chunk index, snippet, and RRF score |
| `get_document` | `document_id` **or** `path` | Full text plus format, size, last-modified, and the ingestion hash |
| `query_knowledge_graph` | `node_id` **or** `document_id`, `depth` (default 1, max 5) | The neighboring documents recorded at insert time, with edge distances |

Chaining works off `document_id`: search, then fetch the source or walk out to what sits
near it in embedding space.

```jsonc
// search_notes
{"query": "how are vector and keyword results combined?", "limit": 3}
// → {"count": 2, "results": [{"rank": 1, "document_id": "/Users/you/Engrex/rrf-notes.md",
//     "chunk_id": 2, "chunk_index": 0, "score": 0.0328, "snippet": "..."}, ...]}

// get_document
{"document_id": "/Users/you/Engrex/rrf-notes.md"}
// → {"format": "md", "content_source": "file", "size_bytes": 370,
//     "ingestion_hash": "73f36b9a...", "content": "..."}

// query_knowledge_graph
{"document_id": "/Users/you/Engrex/rrf-notes.md", "depth": 1}
// → {"root_node_id": 2, "nodes": [{"node_id": 1, "depth": 1, ...}],
//     "edges": [{"source_node_id": 1, "target_node_id": 2, "distance": 0.876}]}
```

Failures come back as MCP tool errors led by a stable code, so a client can tell them
apart without reading prose: `invalid_input`, `not_found`, `index_unavailable` (an
ingestion is mid-flight — retry), `embedder_unavailable` (Ollama is down), `mcp_disabled`,
`daemon_unavailable`, `internal_error`. An empty result set is **not** an error — a
search that matches nothing returns `count: 0`.

Full details in [docs/mcp.md](docs/mcp.md).

---

## Development

Always use `make` instead of bare `go` commands. The Makefile sets the correct CGo flags to link against Homebrew's SQLite, which is required for sqlite-vec to work on macOS.

```bash
make install   # build the binary to bin/engrex
make test    # run all tests
```

Running `go build` or `go test` directly without the Makefile flags will fail with linker errors on macOS.

### Database

The database lives at `~/.engrex/engrex.db` and is created automatically on first run. You can inspect it directly:

```bash
sqlite3 ~/.engrex/engrex.db ".tables"
sqlite3 ~/.engrex/engrex.db "SELECT id, text, created_at FROM chunks;"
```

---

## Build Phases

| Phase | What it adds | Status |
|---|---|---|
| 1 — Core RAG | `engrex add` / `engrex query` from the CLI | ✅ Done |
| 2 — Passive capture | Background daemon, file watcher, Unix socket, launchd | ✅ Done |
| 3 — Hotkey UI | Swift menubar app, global hotkeys, query window, sources | ✅ Done |
| 4 — Smarter ingestion | Browser extension, HTTP endpoint, sentence chunking, source/origin metadata | ✅ Done |
| 5 — Knowledge graph | Force-directed graph viz, semantic edges, web UI | ✅ Done |
| 6 — Retrieval & ingestion quality | Hybrid search (BM25 + vector, RRF), document-level re-ingestion, `.docx` + code/config ingestion | ✅ Done |
| 7 — MCP interface | Read-only `search_notes` / `get_document` / `query_knowledge_graph` tools over stdio for Claude Desktop | ✅ Done |
| 8 — Privacy (optional) | Encryption at rest | Planned |

---

## Notes

I built this project using claude as a helper to plan the systems design and exaplain new concepts for me. All working backend code was written by hand (by me) as a way to learn systems design in Go. Majority of frontend code was written by claude.