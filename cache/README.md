# A semantic cache in front of our LLM calls

**Proposal: put a caching layer between Engrex and Ollama. On a 2,000-request workload it
served 69.9% of requests from cache with a measured 0.00% wrong-answer rate, returning
them ~5,000x faster than a generation. With a real model, a repeated question goes from
2.64s to 17ms.**

## The problem

Every LLM call Engrex makes goes to one local Ollama server. A lot of that work is
repeated, and three cases stand out:

- **Verification re-sends the same passages on every claim.** `checkClaim` re-renders all
  retrieved passages and appends one `STATEMENT:` line, one HTTP call per claim, up to 20.
  Twenty near-identical prompts per answered question.
- **Re-indexing re-embeds the whole corpus.** Ingestion embeds one chunk per call, and
  `reindex` walks every stored chunk.
- **The eval harness replays the same 10 golden questions on every run** — and its entire
  purpose is to be run repeatedly while one thing changes.

Generation dominates latency. The comment on `maxAnswerTokens` records 53s for one
uncapped answer, 14s capped.

## What it does

It sits in front of Ollama speaking the same API, so adopting it is a change of one URL.
For each request it decides whether an equivalent one has already been answered.

The load-bearing decision is that **it does not embed the whole prompt.** An answer prompt
is a fixed preamble, five retrieved passages, and the question — and the question is about
1% of the tokens. Similarity over the whole thing is really similarity over the passages,
which produces false hits and false misses simultaneously. So the prompt is split: the
context is hashed into an exact-match key, and only the question is matched by meaning.

That single decision is what makes the numbers below possible. Because a namespace is one
exact context, it only ever holds different wordings of the same question — which is why
the false-hit rate is zero at every threshold tested, rather than something to be traded
away.

## What it costs to be wrong

A cache that serves the wrong answer is worse than no cache, so correctness is measured
rather than assumed. The benchmark's provider stamps every answer with the question it was
written for, and every hit is checked against every acceptable wording of the question
asked:

```
requests    2000        hit rate        69.9%
hits        1398        hits checked     1398
misses       602        wrong hits          0
                        false hit rate   0.00%

LATENCY        P50          P95          P99
hit          169µs       43.9ms       65.9ms
miss       856.0ms      884.2ms      901.4ms
```

Three findings worth stating plainly:

1. **Most of the value is recognizing a prompt already seen.** The exact tier carries
   ~1,280 of the 1,398 hits. Semantic matching adds ~115 more. Useful, not magic.
2. **The threshold the guide suggests would have broken it.** 0.95 is right for OpenAI's
   embeddings. On `nomic-embed-text` unrelated questions score 0.48 and genuine rewordings
   0.73, so 0.95 admits 5.6% of real paraphrases. The number has to be measured against
   the model in front of you; `engrex-cache calibrate` does that.
3. **The context-tolerant tier looks worthless on the benchmark and useful in reality.**
   On the synthetic workload it adds 2 hits, both wrong. On the real index it turns a
   13.1s paraphrase into a 0.655s cache hit with a correct answer, because two wordings of
   one question really do retrieve overlapping passages and the fixture does not model
   that. It ships off — a tolerant hit is the only kind that can be wrong — but it is
   worth enabling on a real corpus.

## What it would cost us

Nothing to run: one Go binary, no Redis, no containers, no second inference dependency. It
imports nothing that needs CGO, so it builds with a plain `go build` while the main binary
needs Homebrew sqlite and a build tag.

The risks, and what covers each:

| Risk | Cover |
|---|---|
| Serving an answer grounded in different passages | Impossible by construction — context is hashed into the key |
| A corrupted embedding entering the index | Embeddings are exact-match only, never approximate |
| Stale answers after new notes are saved | Strict tiers key on the exact passages, so new notes change the key; with `--tolerant`, ingestion is detected through the proxy and drops cached answers |
| A long answer truncating on replay | Replayed as chunked frames; pinned by a test with a 200KB answer |
| A half-finished generation being stored | Nothing is stored without a clean `done` frame |
| The proxy being down | Engrex is pointed at it explicitly, so it fails loudly rather than silently degrading |

## Adopting it

```bash
make cache-install
engrex-cache serve &
engrex-cache enable      # then restart the daemon
```

`engrex-cache disable` reverses it.

## Going deeper

| Doc | What it covers |
|---|---|
| [docs/design.md](docs/design.md) | Why it splits prompts, the three hit tiers, classes and thresholds |
| [docs/internals.md](docs/internals.md) | Entries, journal, index, eviction, locking |
| [docs/api.md](docs/api.md) | Endpoints, headers, metrics, CLI |
| [docs/testing.md](docs/testing.md) | What the tests prove, and the fixture bugs found on the way |
| [docs/measurements.md](docs/measurements.md) | Every number, its methodology, and what is simulated |

[../docs/caching.md](../docs/caching.md) covers how it fits into Engrex.

## Reproducing the numbers

```bash
make cache-calibrate    # the paraphrase/different-question separation
make cache-bench        # the 2,000-request load test
```

The benchmark uses a synthetic provider at 800ms per generation, because 2,000 real local
generations take hours. The hit rate, the cache-hit latency and the false-hit rate are
real measurements; the avoided time is the simulated per-call cost times the hit count.
The report says so every time it prints.
