# Semantic Caching

`cache/` is a caching layer that sits between Engrex and Ollama. It recognizes requests
that are semantically the same as ones already answered and serves the stored response
instead of generating it again.

It is a separate binary (`engrex-cache`) speaking the Ollama API, so putting it in the
path is a change of one URL and nothing else. Every source file lives under `cache/`;
every runtime file lives under `~/.engrex/cache/`.

```
  engrex daemon                         engrex-cache                    Ollama
  ─────────────                         ────────────                    ──────
  rag / rerank      ──/api/generate──▶  split the prompt      ──miss──▶  llama3.2
  rewrite / verify  ──/api/embed────▶   look it up                      nomic-embed-text
  embedder                              ◀──hit: 169µs──
                                             │
                                        ~/.engrex/cache/
                                        entries.jsonl · lookups.jsonl
```

## Why it does not embed the whole prompt

This is the part that makes it work, and the part a naive implementation gets wrong.

An Engrex answer prompt (`internal/rag/rag.go:871`) is a fixed instruction preamble, a
document manifest, five retrieved passages, and then the question. The question is about
**one percent of the tokens**. Embedding the whole thing means cosine similarity is
dominated by the passages, which fails in both directions at once:

- **False hits** — two unrelated questions that retrieved the same five passages embed
  almost identically, so the cache serves an answer to a different question.
- **False misses** — the same question with one passage swapped looks different, so the
  cache never fires when it should.

So every prompt is cut in two (`cache/split.go`):

```
namespace  = sha256(endpoint ‖ model ‖ params ‖ context part)   exact match
similarity = cosine(embed(semantic part), stored embedding)     within that namespace
```

A hit therefore guarantees the stored answer was generated from **identical** context.
The measurements below show this is what carries the whole design: with the context
hashed into the key, the false-hit rate is zero across every threshold tested, because a
namespace only ever contains different wordings of the same question.

The splitter anchors on each prompt builder's fixed opening line — position 0, the one
place a retrieved passage can never reach — then finds the delimiters from the end. The
end matters: the rewrite prompt carries a few-shot example that itself begins
`QUESTION: `, and a saved note can contain any string at all.

Prompts it does not recognize fall back to treating the last paragraph as the question,
which holds for essentially any RAG template. Failing to recognize a prompt costs hit
rate, never correctness.

## What gets cached, and how loosely

| Class | Threshold | TTL | Why |
|---|---|---|---|
| `embed` | exact only | 30d | Never matched approximately — see below |
| `answer` | 0.70 | 24h | Prose; a near-miss is a wrong answer |
| `rerank` | 0.70 | 7d | Deterministic given its input |
| `rewrite` | 0.70 | 7d | Deterministic given its input |
| `verify` | 0.85 | 7d | Compares claims, not questions — see below |

**Embeddings are never matched on similarity.** Serving a near neighbour's vector would
put a subtly wrong direction into the index, and every distance threshold in Engrex is
calibrated against vectors the model really produced. It would not fail loudly;
retrieval would just quietly get worse. This is a hard rule in `policy.go`, not a dial.

**Verification is held tighter** than the rest because it compares claims pulled out of
one answer — short, similarly worded, about the same passages. That is the case where two
genuinely different statements look most alike.

**Time-referencing questions are not cached at all.** "What did I save today" is exactly
the question a stale answer is most visibly wrong about, and this corpus moves under the
cache continuously.

**Ingestion invalidates cached answers when `--tolerant` is on.** Ingestion embeds each
chunk with the `search_document:` prefix through the proxy, and a *miss* on one means new
or edited content, so cached answers are dropped. Strict mode skips this: its tiers key on
the retrieved passages byte for byte, and Engrex retrieves before generating, so a new note
that changes retrieval changes the key by itself. Only the tolerant tier can reuse an answer
across a swapped passage, and that passage could be the new note.

## Thresholds are a property of the embedding model

The guide this is adapted from suggests starting at 0.95. That is sound for OpenAI's
embeddings and badly wrong here. Measured with `engrex-cache calibrate` over the
workload's hand-written paraphrases:

```
DISTRIBUTION                            MIN   MEDIAN      MAX        N
----------------------------------------------------------------------
same question, different words       0.5045   0.7320   0.9747       18
different questions                  0.3740   0.4803   0.6502       45
```

On `nomic-embed-text`, unrelated questions sit at 0.48 and genuine rewordings at 0.73. At
a 0.95 threshold **5.6%** of real paraphrases clear the bar — a cache that does not fire.
0.70 is the highest unrelated pair observed (0.65) plus a margin.

The distributions overlap: the weakest paraphrase scores 0.50, below the closest pair of
different questions at 0.65. No threshold catches every rewording without ever serving
something wrong. That is a real trade, not a tuning failure, and the default sits on the
safe side of it — a miss costs time, a false hit is a confident answer to a question
nobody asked.

Run `make cache-calibrate` after changing the embedding model.

## Measured

2,000 requests, 35% never asked before, 25% reworded, 40% repeats, against a synthetic
provider at 800ms per generation (`make cache-bench`):

```
requests                         2000
hits                             1398
misses                            602
hit rate                        69.9%
hits checked                     1398
wrong hits                          0
false hit rate                  0.00%

LATENCY                         P50          P95          P99
hit                           169µs       43.9ms       65.9ms
miss                        856.0ms      884.2ms      901.4ms
```

**69.9% hit rate, 0.00% false hits, a hit served ~5,000x faster than a miss.** The
false-hit number is a real measurement, not an assumption: the synthetic provider stamps
every answer with the question it was written for, and the load test checks the stamp on
all 1,398 hits against every acceptable wording of the question asked.

With a real model, `llama3.2` answering a small grounded prompt: **2.64s cold, 17ms on
the repeat.**

Threshold sweep, same workload, all with correct ground truth:

| Threshold | Hit rate | False hits | Namespace-tier hits |
|---|---|---|---|
| 0.55 | 70.0% | 0.00% | 221 |
| 0.65 | 70.0% | 0.00% | 145 |
| 0.70 | 69.9% | 0.00% | 115 |
| 0.80 | 69.8% | 0.00% | 12 |

The hit rate barely moves because the exact tier carries it. That is the honest headline:
**most of the value is recognizing a prompt already seen**, not clever matching. Semantic
matching adds ~100 hits out of 1,400 and, thanks to the namespace, adds no errors.

### The context-tolerant tier, and why it is off

Retrieval is a function of the question, so rewording one usually changes which passages
come back. That breaks the exact-context match in exactly the case a semantic cache is
meant for. `--tolerant` relaxes the namespace and re-adds safety with a MinHash overlap
guard requiring the two contexts to share ≥0.65 Jaccard — one swapped passage out of five
is 4-of-6 distinct, about 0.67; two swapped is 0.43.

The synthetic benchmark and the real corpus disagree about this one, and the real corpus
is the one to believe.

On the benchmark it earns 2 extra hits and both are wrong — because the fixture gives
every generated question its own scattered passage set, so two different questions
essentially never retrieve overlapping context, and the only things the guard lets
through are accidents.

On the real index it does exactly what it was built for. Asking "tell me what is cifar"
and then "explain cifar to me" retrieves *different* passages, so the two land in
different namespaces and tier 1 cannot match them however similar the questions are
(measured 0.9355). With `--tolerant` the second is served from cache — 13.1s down to
0.655s, and the answer it reuses is a correct answer to the second wording.

So the honest summary: the benchmark understates this tier because its fixture does not
reproduce how much real retrieval overlaps between two wordings of one question. It stays
off by default because a tolerant hit is the only kind that can be wrong, and turning it
on should be a decision rather than a default — but on a real corpus it is worth turning
on.

## Running it

```bash
make cache-install                 # builds and installs engrex-cache
engrex-cache serve                 # proxy :11435, dashboard :11436
engrex-cache enable                # writes ollama_url into ~/.engrex/config.json
                                   # then restart the daemon
```

`enable` is explicit rather than auto-detected: whether the cache is in the path should
never be a guess, least of all while measuring it. `engrex-cache disable` puts it back.

### Endpoints

`/api/generate`, `/api/chat` and `/api/embed` are cached. Everything else is reverse
proxied untouched, so putting this in front of Ollama never removes a capability.

Engrex itself only ever calls `/api/generate` and `/api/embed`. `/api/chat` is here
because the Ollama desktop app and most third-party clients use it, and a drop-in that
quietly stopped caching for them would not be much of a drop-in. A conversation is
flattened so the last message becomes the semantic part and the system prompt plus the
history become context — which means two identical questions under different system
prompts cannot share an entry, falling out of the split rather than needing a rule.

You can point the real Ollama CLI at it:

```bash
OLLAMA_HOST=127.0.0.1:11435 ollama run llama3.2 "reply with exactly: hello"
```

Measured: 270ms on the first call, 13ms on the repeat.

```bash
engrex-cache stats                 # what it has done
engrex-cache calibrate             # measure paraphrase separation, recommend a threshold
engrex-cache tune                  # replay the lookup log at other thresholds
engrex-cache loadtest --requests 2000
engrex-cache invalidate --model llama3.2   # after a model upgrade
engrex-cache invalidate --class answer     # after editing buildPrompt
curl localhost:11436/metrics       # Prometheus text exposition
```

Every response carries `X-Engrex-Cache: hit|miss|bypass`, plus the tier, the similarity,
and how many milliseconds the stored answer originally took.

### Auto-start

There is no checked-in plist, for the same reason the daemon has none
([development.md](development.md)). A sibling agent looks like the one in [mcp.md](mcp.md)
with `Label` `com.robertkoller.engrex-cache`, `ProgramArguments`
`["/usr/local/bin/engrex-cache", "serve"]`, and its own log path. `launchd` does not
expand `~`.

## Streaming, and a trap worth knowing about

On a miss the response is relayed frame by frame while being buffered, and stored only if
a frame arrives with `done` set and no error — a stream that dies halfway is served as far
as it got and then forgotten.

On a hit the answer is replayed as a **sequence** of NDJSON frames, never one. `rag.Query`
used to read the stream with a default `bufio.Scanner`, which abandons any line over 64KB,
and never checked `scanner.Err()`, so a long answer replayed as one frame truncated
**silently**. It now raises the limit to 16MB and checks the error, but the proxy still
chunks, because other Ollama clients read the stream the same naive way. `cache/proxy_test.go` pins this with a 200KB answer read through a default
scanner; raising the chunk size makes it fail with `token too long`.

Ollama's final frame carries a `context` array large enough to hit the same limit on its
own. Nothing in Engrex reads it, so it is dropped from replayed responses rather than
risking the truncation.

## Storage

`~/.engrex/cache/` — never inside the repository.

| File | Contents |
|---|---|
| `entries.jsonl` | append-only log of stores and evictions, replayed at startup |
| `lookups.jsonl` | every lookup and how close it came, for `tune` |

Embeddings serialize as base64 rather than JSON float arrays: 768 dimensions cost about
4KB written that way against roughly 15KB as decimal text, and the journal is almost
entirely embeddings.

The journal is compacted automatically, at startup and whenever dead records outnumber
live ones by a margin, so it tracks the live set rather than growing forever.

Eviction is TTL plus an LRU cap. `internal/hnsw` has no delete — its `unlink` path exists
only to replace an id, and using it otherwise leaves dangling edges — so evicted entries
are tombstoned and the graph is rebuilt once a quarter of it is dead.

## Deeper documentation

This file covers the integration: what the cache is, how to switch it on, and what it
changes about Engrex. The design itself is documented inside the package:

- [`cache/docs/design.md`](../cache/docs/design.md) — prompt splitting, hit tiers, thresholds
- [`cache/docs/internals.md`](../cache/docs/internals.md) — data structures and storage
- [`cache/docs/api.md`](../cache/docs/api.md) — endpoints, headers, metrics, CLI
- [`cache/docs/testing.md`](../cache/docs/testing.md) — what the tests prove
- [`cache/docs/measurements.md`](../cache/docs/measurements.md) — every number and its methodology

## How it relates to the rest of Engrex

`cache/` imports `internal/hnsw` and `internal/embedder` and nothing else from the
project. Neither reaches sqlite, so **it builds with a plain `go build`** — no
`-tags libsqlite3`, no Homebrew sqlite, unlike every other binary here.

The changes outside `cache/` are four:

1. `internal/config/config.go` — `OllamaURL()`, resolving `ENGREX_OLLAMA_URL` → config
   file → default, following the `GenerateModelName()` template.
2. `internal/rag/rag.go` — the `ollamaBaseURL` const became a field resolved once in
   `New`. The answer call also now pins `"temperature": 0`, matching what reranking,
   rewriting and verification already sent. Without it Ollama samples at its own default,
   so the same question gives differently worded answers of different lengths — and
   length dominates latency, which makes a cached and an uncached run incomparable.
3. `internal/socket/readonly.go` — the "Ollama unreachable" heuristic matched the literal
   `11434`; it now reads the configured port.
4. `Makefile` — `cache`, `cache-install`, `cache-serve`, `cache-bench`, `cache-calibrate`.
