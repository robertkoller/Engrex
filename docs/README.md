# Engrex Documentation

Engrex is a local-first AI "second brain" for macOS. You save text and files; it
embeds and indexes them locally; you ask questions in natural language and it
answers from your own content using a local LLM. By default nothing leaves your
machine. The optional Gemini mode is the one exception, described below.

These docs explain how each part works.

| Doc | What it covers |
|---|---|
| [architecture.md](architecture.md) | The big picture — processes, data flow, how a save and a query travel through the system |
| [data-model.md](data-model.md) | The SQLite schema (`chunks`, `vec_chunks`, `fts_chunks`, `relations`, `documents`), `sqlite-vec`, and FTS5 |
| [rag-pipeline.md](rag-pipeline.md) | Chunking, embedding, hybrid retrieval (vector + BM25, rank fusion), prompt building, streaming answers, chat follow-ups, and the optional Gemini backend |
| [ingestion.md](ingestion.md) | Every way content gets in: CLI, hotkey, file watcher, browser extension (including Google Docs, Slides and Drive) — supported file types, re-ingestion, origin metadata, and RawText stubs |
| [daemon.md](daemon.md) | The background daemon and its four servers (Unix socket, HTTP, graph, file watcher) |
| [mcp.md](mcp.md) | The MCP interface: the three read-only tools, the trust boundary, and Claude Desktop setup |
| [ui.md](ui.md) | The Swift menu-bar app: hotkeys, the query window, follow-up chat and Markdown rendering, the Deep thinking and Use Gemini toggles, themes, file upload |
| [development.md](development.md) | Building, installing, testing, the launchd agent, and the macOS gotchas |
| [evaluation.md](evaluation.md) | Scoring retrieval quality: the golden set, `engrex eval`, recall@k / MRR, baselines, `doctor` and `reindex` |
| [retrieval-stages.md](retrieval-stages.md) | The optional stages — query rewriting, reranking, citation verification — and why each is off by default |
| [caching.md](caching.md) | The semantic cache in front of Ollama: prompt splitting, thresholds measured rather than guessed, and what it actually saves. Deeper notes live in [`cache/docs/`](../cache/docs/) |
| [hnsw.md](hnsw.md) | The from-scratch HNSW index, its benchmark methodology, and where it does and doesn't beat brute force |
| [rag-upgrade-plan.md](rag-upgrade-plan.md) | Audit of the current retrieval pipeline and the phased plan to take it to production-grade RAG |

## Quick mental model

There are three moving parts:

1. **The Go daemon** (`engrex daemon`) — the brain. Owns the database and the RAG
   pipeline. Listens on a Unix socket (for the CLI and Swift app), an HTTP endpoint
   (for the browser extension), and watches `~/Engrex/` for dropped files.
2. **The clients** — the CLI (`engrex ...`), the Swift menu-bar app, and the browser
   extension. They all just send the daemon commands and render its responses.
3. **Ollama** — a local server providing the embedding model (`nomic-embed-text`)
   and the generation models (`llama3.2` by default, `qwen3:8b` for Deep thinking).

By default everything is local. The only network calls are to Ollama on `localhost:11434`,
or, with the semantic cache running, to the cache proxy on `localhost:11435`, which
forwards to Ollama. Two features reach out on purpose. The browser extension downloads
Google Docs, Slides and Drive files with your own browser session when you capture
them. And if you add a Gemini key and switch Use Gemini on, each question's retrieved
passages are sent to Google to write the answer.
