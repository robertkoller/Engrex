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
│   ├── socket/           # Unix socket server (CLI + Swift app)
│   ├── httpserver/       # localhost HTTP endpoint (browser extension)
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
| 7 — Privacy (optional) | Encryption at rest | Planned |

---

## Notes

I built this project using claude as a helper to plan the systems design and exaplain new concepts for me. All working backend code was written by hand (by me) as a way to learn systems design in Go. Majority of frontend code was written by claude.