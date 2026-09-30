# Design

## The problem this is solving

Every LLM call Engrex makes goes to one local Ollama server, and generation dominates
latency — the comment on `maxAnswerTokens` in `internal/rag/rag.go` records 53s for one
uncapped answer, 14s capped. A lot of that work repeats:

- **Verification re-sends the same passages on every claim.** `verify.checkClaim` renders
  every retrieved passage and appends one `STATEMENT:` line, one call per claim, up to 20
  per answer. Twenty prompts differing by one sentence.
- **Re-indexing re-embeds the corpus.** Ingestion embeds one chunk per call, and `reindex`
  walks every stored chunk.
- **The eval harness replays the same 10 golden questions every run**, and exists to be
  run repeatedly while one variable changes.

## Why a naive semantic cache is worse than none

The obvious implementation embeds the incoming prompt, finds the nearest stored prompt,
and serves it above some similarity. In this codebase that fails badly in both directions
at once.

Look at what `internal/rag.buildPrompt` produces:

```
You are a personal knowledge assistant with access to the user's OWN saved notes...
RULES:
1. Answer using ONLY the CONTEXT below.
...
SAVED DOCUMENTS these passages come from (3):
  1. daemon.md
  ...
CONTEXT:
[1] document: daemon.md | section: ... | saved: 2026-08-14
<a few hundred tokens of retrieved text>
[2] ... x5
QUESTION: what distance threshold is used for deduplication?

Answer from the CONTEXT above only. If it is not there, say so rather than guessing.
ANSWER:
```

The question is roughly **1% of the tokens**. Cosine similarity over the whole prompt is
therefore mostly similarity between the retrieved passages, which produces:

- **False hits.** Two unrelated questions that happened to retrieve the same five passages
  embed almost identically. The cache serves an answer to a different question, fluently
  and with citations.
- **False misses.** The same question with one passage swapped looks different, so the
  cache never fires when it should.

The verify prompts are the extreme case: twenty consecutive prompts that are byte-identical
except for one trailing sentence would all collapse onto each other.

## The split

Every prompt is cut into two parts with different matching rules:

```
namespace  = sha256(endpoint ‖ model ‖ params ‖ context part)   must match exactly
similarity = cosine(embed(semantic part), stored embedding)     compared within a namespace
```

The **context part** is everything that establishes what the answer must be grounded in:
the instruction preamble, the document manifest, the retrieved passages. It is hashed, so
it either matches to the byte or the two prompts never meet.

The **semantic part** is the thing actually being asked — the question, the claim, the
query to decompose. Only this is embedded.

A hit therefore carries a structural guarantee: **the stored answer was generated from
identical source material.** This is not a probabilistic property that a threshold trades
away; it is true at every threshold. The measurements bear that out — the false-hit rate is
zero from 0.55 upward, because a namespace only ever contains different wordings of the
same question. See [measurements.md](measurements.md).

### Finding the boundary

`split.go` holds a table of the prompt formats this project emits. Each entry is a fixed
opening line plus the delimiters that wrap the caller's input:

| Format | Built by | Semantic part sits between |
|---|---|---|
| `engrex-answer` | `rag.buildPrompt` | `\nQUESTION: ` and `\n\nAnswer from the CONTEXT above only.` |
| `engrex-no-context` | `rag.buildNoContextPrompt` | `\n\nQuestion: ` and end of prompt |
| `engrex-rerank` | `rerank.buildPrompt` | `\nQUESTION: ` and `\n\nPASSAGES:\n` |
| `engrex-verify` | `verify.checkClaim` | `\nSTATEMENT: ` and `\n\nIf a passage states` |
| `engrex-rewrite` | `rewrite.buildPrompt` | `\nQUESTION: ` and `\nLOOKUPS:` |

Two details in `marker.apply` are load-bearing:

**Identification is by prefix, not by search.** A saved note can contain the literal text
`QUESTION: `. If the splitter identified a format by searching for its delimiters anywhere,
stored content could steer how its own prompt gets cut — and a note could be written that
makes two different questions share a cache entry. Position 0 is the one place a retrieved
passage can never reach. `TestPassageCannotHijackTheSplit` pins this.

**Delimiters are found from the end.** The rewrite prompt contains a few-shot example that
itself begins `QUESTION: ` and is followed by `LOOKUPS:`. Splitting on the first occurrence
returns the example's question instead of the caller's.
`TestSplitsRealRewritePromptPastItsFewShotExample` pins this against the real builder.

Anything unrecognised falls through to `TailSplitter` — the last paragraph is the question,
everything above it is context — which holds for essentially any RAG template, and then to
`WholeSplitter` for short prompts that are nothing but a question. Failing to recognise a
prompt costs hit rate, never correctness.

Chat requests (`/api/chat`) are flattened by `renderChat` into `role: content` blocks
separated by blank lines, so the last message lands as its own trailing paragraph and
`TailSplitter` treats it as the question. The system prompt and conversation history become
context, which is matched exactly — so two identical questions under different system
prompts cannot share an entry. That is a requirement the reference guide states explicitly,
satisfied here without a special case.

## How a lookup is decided

`Cache.Lookup` tries three things, cheapest first, and stops at the first hit.

### Tier 1 — exact (`TierExact`)

A hash of the entire request: endpoint, model, canonical options (JSON, so `stop:["a","b"]`
and `stop:["a b"]` cannot collide), any other output-affecting request field, and the full
prompt text.
If the same prompt has been seen before, the stored response is returned with no embedding
call at all. This is free and cannot be wrong.

**In practice this tier carries the cache.** On the benchmark it is ~1,280 of 1,398 hits.
Most of the value is recognising work already done, not clever matching.

### Tier 2 — same namespace (`TierNamespace`)

Embed the semantic part, then scan the entries sharing the request's namespace and take the
closest above the class threshold. The scan is linear and exact rather than approximate:
a namespace is one exact context, and in practice holds a handful of entries — the twenty
claims checked against one answer's passages, say — so scanning is both faster and lossless
compared with an approximate index.

### Tier 3 — context-tolerant (`TierTolerant`), off by default

Tier 2 has a real limitation. Retrieval is itself a function of the question, so rewording
one usually changes which passages come back, which changes the context hash, which means
the two prompts land in different namespaces and never meet — in exactly the case a
semantic cache exists for.

The tolerant tier drops the context from the key and searches across namespaces using the
HNSW index, then re-adds safety with a **context overlap guard**: each entry stores a
MinHash of its context, and a hit additionally requires Jaccard overlap ≥ 0.65 with the
incoming one.

The threshold comes from set arithmetic rather than feel. Swap one passage out of five and
the two contexts share four of six distinct passages — Jaccard ≈ 0.67, measured 0.72 on a
real prompt where the shared preamble lifts it. Swap two and it falls to three of seven,
≈ 0.43. 0.65 sits between them.

It is off by default because a tolerant hit is the only kind that can be wrong. On the
real index it works — a reworded question dropped from 13.1s to 0.655s with a correct
answer — while on the synthetic benchmark it looks useless. [measurements.md](measurements.md)
explains that disagreement.

## Classes and thresholds

Every prompt is classified by which format matched, and the class decides how much
approximation is tolerated:

| Class | Threshold | TTL | Reasoning |
|---|---|---|---|
| `embed` | exact only | 30d | Never approximate — see below |
| `answer` | 0.70 | 24h | Prose; a near-miss is a wrong answer |
| `rerank` | 0.70 | 7d | Deterministic given its input |
| `rewrite` | 0.70 | 7d | Deterministic given its input |
| `verify` | 0.85 | 7d | Compares claims, not questions |
| `other` | 0.75 | 24h | Unknown shape, so err tighter |

`verify` is held higher than the rest because it compares claims pulled out of one answer:
short, similarly worded, about the same passages. That is the case where two genuinely
different statements look most alike. It earns most of its keep from exact repeats anyway.

The three pipeline classes can afford 0.70 because they already send `temperature: 0` and
their prompts are pure functions of their inputs, so caching them is unobservable. Since
this work, the answer call pins `temperature: 0` too — otherwise Ollama samples at its own
default, the same question gives differently worded answers of different lengths, and a
cached run and an uncached run are not comparable.

### Thresholds are a property of the embedding model

The reference guide suggests starting at 0.95. That is reasonable for OpenAI's embeddings
and badly wrong here. Measured on `nomic-embed-text` over hand-written paraphrases:

```
same question, different words   min 0.5045   median 0.7320   max 0.9747   n=18
different questions              min 0.3740   median 0.4803   max 0.6502   n=45
```

At 0.95, **5.6%** of genuine rewordings clear the bar — a cache that does not fire. 0.70 is
the highest unrelated pair observed (0.65) plus a margin. `engrex-cache calibrate`
reproduces this against whatever model is configured; re-run it if the embedding model
changes.

The two distributions overlap: the weakest paraphrase scores 0.50, below the closest pair
of different questions at 0.65. **No threshold catches every rewording without ever serving
something wrong.** That is a real trade, not a tuning failure, and the default sits on the
safe side of it — a miss costs time, a false hit is a confident answer to a question nobody
asked.

## Embeddings are never matched approximately

`/api/embed` is cached by exact hash only. Serving a near neighbour's vector would put a
subtly wrong direction into the index, and every distance threshold in Engrex —
`rag.DefaultSearchDistance`, `store.DefaultEdgeThreshold` — is calibrated against vectors
the model actually produced. It would not fail loudly; retrieval would quietly get worse.

This is enforced structurally rather than by configuration: `ClassEmbed`'s threshold is
`exactOnly = 1.1`, which no two distinct unit vectors can reach, and
`Policy.ApproximateAllowed` returns false for it so the embedding step is skipped entirely.
`TestEmbeddingsAreNeverMatchedApproximately` asserts a miss even at 0.9999 similarity.

Note that the `search_document:` and `search_query:` task prefixes
(`internal/embedder/ollama.go`) are part of the hashed text, so an ingest embedding and a
query embedding of the same string can never collide.

## Freshness

**Time-referencing questions are not cached at all.** `IsVolatile` matches words like
`today`, `latest`, `currently`. The guide offers either a short TTL or no caching; this
takes the second, because the corpus moves under the cache continuously — the watcher
ingests whatever lands in `~/Engrex` — and "what did I save today" is precisely the
question a stale answer is most visibly wrong about. These queries are rare, so bypassing
them costs almost no hit rate.

Volatility is judged on the **semantic part only**. Running it over a whole answer prompt
would match on the retrieved passages, and one saved note containing the word "latest"
would make every question about that document uncacheable.
`TestVolatilityIsJudgedOnTheQuestionNotThePassages` pins this.

**Ingestion invalidates cached answers when `--tolerant` is on.** The signal is free:
ingestion embeds each chunk with the `search_document:` prefix, and that call comes through
the proxy. A *miss* on one means the chunk's text was not seen before — new or edited
content — so cached answers are dropped. A *hit* means identical text, so nothing changed.

It is limited to tolerant mode because only that tier can go stale this way. The exact and
namespace tiers key on the retrieved passages byte for byte, and Engrex retrieves before it
generates, so once a new note changes what comes back the prompt changes and those tiers
miss on their own. Dropping their answers too only threw away hits that were still correct.
A tolerant hit may reuse an answer whose passages differ by one, and that one could be the
new note. Only answers go; reranking, rewriting and verification are functions of the
inputs they were handed.

Manual invalidation is by model, class, or tag. Upgrading the generation model makes every
stored answer stale (`--model`); editing a prompt builder makes one class stale
(`--class`). An empty criteria matches nothing, so a malformed request cannot empty the
cache by accident.

## Streaming

On a **miss**, the upstream response is relayed frame by frame through a flushing writer
while being assembled in parallel. Streaming is what makes a slow local model bearable, and
buffering the whole answer to cache it would take that away. The entry is written only if a
frame arrives with `done` set and no error field — a stream that dies halfway is served to
the caller as far as it got, then forgotten.

On a **hit**, the stored text is replayed as a *sequence* of NDJSON frames, never one.
`rag.Query` used to read the stream with a default `bufio.Scanner`, which abandons any line
over 64KB, without checking `scanner.Err()`, so a long cached answer replayed as a single
frame truncated **silently**. `rag.Query` now raises the limit and checks the error; the
chunking stays because any other client that reads the stream this way still breaks.
`TestLongCachedAnswerSurvivesADefaultScanner` pins this with a 200KB answer.

Ollama's final frame carries a `context` array large enough to hit the same limit on its
own. Nothing in Engrex reads it, so it is dropped from replayed responses rather than
risking the truncation — recorded here rather than left as a silent omission.

Because the stored payload is the assembled text rather than the provider's raw frames, a
streaming and a non-streaming call with the same prompt share one entry, and each is
re-framed on the way out.

## Provider abstraction

`Provider` isolates everything vendor-shaped: which field holds the prompt, whether a
stream is NDJSON or server-sent events, what a finished response looks like. The cache
itself deals only in a prompt, a namespace and a payload.

`Ollama` implements `/api/generate`, `/api/chat` and `/api/embed`. Everything else is
reverse-proxied untouched, so putting this in front of Ollama never removes a capability.
OpenAI and Anthropic exist as `unsupportedProvider` stubs: routing reaches them and they
refuse. Neither can be exercised by Engrex or tested without API keys, and shipping an
untested path that claims to talk to a paid API is worse than shipping nothing.
