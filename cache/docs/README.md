# `cache/` documentation

In-depth notes on the semantic caching layer. For the short version — what it is, how to
switch it on, what it saves — read [`../README.md`](../README.md), and
[`../../docs/caching.md`](../../docs/caching.md) for how it fits into Engrex.

| Doc | What it covers |
|---|---|
| [design.md](design.md) | Why it splits prompts, how a lookup is decided, the three hit tiers, and the class system |
| [internals.md](internals.md) | Entries, the journal, the index and its tombstones, eviction, locking, storage formats |
| [api.md](api.md) | The HTTP surface: cached endpoints, headers, admin routes, metrics |
| [testing.md](testing.md) | What the tests prove, how to run them, and how to check they can still fail |
| [measurements.md](measurements.md) | Every number quoted anywhere, how it was produced, and which are real vs simulated |

## The one-paragraph version

Engrex sends every LLM call to a local Ollama server. The cache sits in front of it
speaking the same API. For each request it splits the prompt into the retrieved context
and the question, hashes the context into an exact-match key, and compares only the
question by meaning. A hit therefore always came from identical source material. Measured
on a 2,000-request workload: 69.9% of requests served from cache, 0.00% of them wrong,
about 5,000x faster than a generation.

## Reading order

If you want to understand the design, [design.md](design.md) alone is enough. If you are
about to change something, read [internals.md](internals.md) and [testing.md](testing.md)
together — most of the non-obvious constraints in the code exist because of a specific
failure, and each is pinned by a test that will tell you when you have reintroduced it.
