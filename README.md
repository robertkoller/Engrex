# Engrex

### Your memory, on your machine.

Engrex is a local-first AI second brain. Everything you deliberately save — text you select, notes you write, pages you read — gets embedded and indexed on your machine. Ask it anything in natural language and it surfaces your own thoughts back to you. No cloud. No subscription. No data leaving your device.

---

## How It Works

You select text anywhere on your screen and hit a hotkey (`Cmd+Shift+B`). Engrex reads the selection via the macOS Accessibility API, splits it into chunks, embeds each chunk using a local model, and stores the vectors in a local SQLite database. Later, you open the query window with a global hotkey, ask a natural language question, and Engrex retrieves the most relevant chunks from your own content and answers using a local LLM — entirely on-device.

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
│              │  - Vector search      │              │
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
├── cmd/
│   └── engrex/
│       └── main.go        # CLI entry point — cobra root + subcommands
├── internal/
│   ├── db/
│   │   └── db.go          # Opens SQLite, loads sqlite-vec, runs migrations
│   ├── chunker/
│   │   └── chunker.go     # Splits text into overlapping chunks
│   ├── embedder/
│   │   └── ollama.go      # Calls Ollama /api/embed, returns []float32
│   ├── store/
│   │   └── store.go       # Inserts chunks + vectors, KNN search
│   └── rag/
│       └── rag.go         # Wires chunker + embedder + store + LLM together
└── Makefile
```

---

## Tech Stack

| Layer | Technology |
|---|---|
| Core daemon | Go |
| Embeddings | Ollama — `nomic-embed-text` |
| LLM | Ollama — `llama3.2` |
| Vector store | SQLite + `sqlite-vec` |
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

### Start Ollama

Ollama must be running before you use Engrex. In Phase 1 you start it manually. In Phase 2 the daemon will manage this automatically.

```bash
ollama serve
```

### Install

```bash
make install
```

This outputs the binary to `bin/engrex`.

### Use

```bash
# Save something to your knowledge base
engrex add "Go uses goroutines for concurrency, not OS threads"

# Ask a question — streams an answer from your own saved content
engrex query "how does Go handle concurrency?"

# See all commands
engrex --help
```

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
| 1 — Core RAG | `engrex add` / `engrex query` from the CLI | In progress |
| 2 — Passive capture | Background daemon, file watcher, launchd | Planned |
| 3 — Hotkey UI | Swift menubar app, global hotkeys, query window | Planned |
| 4 — Smarter ingestion | Browser extension, better chunking, source metadata | Planned |
| 5 — Knowledge graph | Force-directed graph viz, semantic edges, web UI | Planned |
| 6 — Privacy + polish | Encryption at rest, exclusion rules, export, forget | Planned |

---

## Notes

I built this project using claude as a helper to plan the systems design and exaplain new concepts for me. All working backend code was written by hand (by me) as a way to learn systems design in Go. Majority of frontend code was written by claude.