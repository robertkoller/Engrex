# Engrex

### Your memory, on your machine.

Engrex is a local-first AI second brain. Everything you deliberately save — text you select, notes you write, pages you read — gets silently embedded and indexed on your machine. Ask it anything in natural language and it surfaces your own thoughts back to you. No cloud. No subscription. No data leaving your device.

---

## The Problem

AI memory tools like Mem, Notion AI, and Obsidian Copilot all require you to actively put things into them. The friction kills the habit. Engrex is passive — it watches what you're already doing and captures it when you tell it to. The knowledge base compounds over time without any effort.

---

## How It Works

You select text anywhere on your screen and hit a dedicated hotkey (`Cmd+Shift+B`). Engrex reads the selection via the macOS Accessibility API, chunks it, embeds it using a local model, and stores it in a local vector database. No clipboard required. Later, you open the query window with a global hotkey, ask a natural language question, and Engrex retrieves the most relevant chunks from your own content and answers using a local LLM — entirely on-device.

---

## Capture Sources

Everything is intentional. Nothing is captured passively or without a deliberate trigger.

| Source | Trigger |
|---|---|
| Selected text (any app) | Select → `Cmd+Shift+B` |
| Notes / markdown files | Save a file in your watched folder |
| Browser pages / selections | Manual `Cmd+B` via browser extension |

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

---

## Tech Stack

| Layer | Technology |
|---|---|
| Daemon | Go |
| Embeddings | Ollama — `nomic-embed-text` |
| LLM | Ollama — `llama3.2` or `mistral` |
| Vector store | SQLite + `sqlite-vec` |
| Menubar + hotkey UI | Swift |
| Graph visualization | React + D3 |
| Browser extension | Vanilla JS |

---

## Build Plan

### Phase 1 — Core RAG *(1–2 days)*
The retrieval pipeline works end to end from the command line.

- Set up Ollama with `nomic-embed-text` and a local generation model
- Create SQLite schema: `chunks(id, text, source, timestamp, embedding)`
- CLI: `engrex add "some text"` → chunk → embed → store
- CLI: `engrex query "what did I write about X"` → embed query → nearest-neighbor search → return top chunks
- Wire LLM: feed retrieved chunks as context, generate a natural language answer

**Milestone:** Working command-line RAG over your own content.

---

### Phase 2 — Passive Capture *(2–3 days)*
The daemon watches for saves so you don't have to feed it manually.

- Build the Engrex daemon — runs as a background process, auto-starts via launchd
- File watcher using `fsnotify`: monitor a `~/Engrex/` notes folder, ingest on save
- Debounce + filter: skip files under ~20 characters, skip binary content
- Deduplication: hash each chunk before inserting, skip if already stored
- Daemon communicates over a Unix socket

**Milestone:** Save a markdown file, query it seconds later.

---

### Phase 3 — Hotkey UI *(3–5 days)*
Makes the system feel native and instant.

- Global hotkey `Cmd+Shift+B` reads the current text selection via macOS Accessibility API and sends it to the daemon — no clipboard involved
- Global hotkey `Cmd+Shift+Space` opens a floating Spotlight-style query window (Swift)
- Query window talks to daemon over Unix socket; results stream back
- Display: answer at the top, source snippets with timestamps below
- Menubar icon: green = daemon running, red = stopped; click to open query window

**Milestone:** Select text anywhere, save with a hotkey, query with a hotkey. First version you'd show someone.

---

### Phase 4 — Smarter Ingestion *(2–3 days)*
More sources, better chunking.

- Browser extension: on manual trigger (`Cmd+B`), POST selected text or full page to daemon's local HTTP endpoint; include source URL and page title
- Improve chunking: split on paragraph/sentence boundaries with overlap between chunks so context isn't cut mid-thought
- Source metadata: store URL, file path, or app name with every chunk
- Add source labels to query results so you know where something came from

**Milestone:** Captures from browser, files, and any app. Knowledge base starts to feel complete.

---

### Phase 5 — Knowledge Graph + Visualization *(3–5 days)*
Surfaces structure, not just search results.

- Add a `relations` table: for every new chunk, compute cosine similarity against recent chunks, store edges above a threshold
- Build a local web UI served by the daemon on `localhost:7778`
- Render a force-directed graph with D3: nodes are chunks/sources, edges are semantic similarity, color nodes by recency
- Click a node → see full text, related nodes, an "ask about this" button that pre-fills the query window
- Time filtering: "show me what I was thinking about last week"

**Milestone:** The graph visualization. This is the GIF that goes in the README.

---

### Phase 6 — Privacy + Polish *(2–3 days)*
Something you'd actually trust and ship.

- Encryption at rest — encrypt SQLite with SQLCipher or encrypt chunks individually
- Exclusion rules — a config file where you list apps, domains, or file paths to never capture
- Retention policy — auto-delete chunks older than N days (configurable)
- Manual forget — `engrex forget "keyword"` removes matching chunks
- Export — `engrex export` dumps everything to a folder of markdown files so you're never locked in

**Milestone:** A complete, trustworthy, privacy-first product.

---

## Rough Timeline

| Phase | Effort |
|---|---|
| 1 — Core RAG | 1–2 days |
| 2 — Passive capture | 2–3 days |
| 3 — Hotkey UI | 3–5 days |
| 4 — Smarter ingestion | 2–3 days |
| 5 — Graph viz | 3–5 days |
| 6 — Polish | 2–3 days |

**Total: ~3 weeks of focused work.**

---

## What Makes It Impressive

- **Fully offline** — no API keys, no data leaves your machine, works without internet
- **Intentional capture** — you decide what goes in, nothing is logged without a deliberate trigger
- **Gets better over time** — the longer you run it, the more useful it becomes
- **Immediately demonstrable** — select text, save it, ask about it 10 seconds later
- **Privacy-first by design** — encrypted at rest, exclusion rules, full export/delete control

## Little Notes
I created this project in junction with claude and essentially had claude plan it out for me, however all of the working usable code was written all by me by hand and I find this a good way to learn systems design.
