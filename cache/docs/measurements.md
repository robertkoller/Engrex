# Measurements

Every number quoted anywhere about this cache, how it was produced, and which parts are
real measurements versus simulated. Reproduction commands are given for all of it.

## Honesty note

Two of these are simulated and the rest are real. The simulated ones are:

- **Provider latency in the load test.** 2,000 real generations from a local model takes
  hours, so the benchmark uses a stand-in provider that returns canned text after a fixed
  800ms. Hit rate, cache-hit latency and the false-hit rate are all real measurements taken
  against it; the "time avoided" figure is the simulated per-call cost times the hit count.
  The report prints that caveat every run.
- **Nothing else.** The calibration, the threshold sweeps and every end-to-end figure use
  the real `nomic-embed-text` and `llama3.2`.

## Threshold calibration

```bash
make cache-calibrate
```

Embeds the workload's questions and their hand-written paraphrases and compares the two
distributions. On `nomic-embed-text`:

```
DISTRIBUTION                            MIN   MEDIAN      MAX        N
same question, different words       0.5045   0.7320   0.9747       18
different questions                  0.3740   0.4803   0.6502       45
```

| Threshold | Recall | False hits | Precision |
|---|---|---|---|
| 0.50 | 100.0% | 31.1% | 56.2% |
| 0.55 | 94.4% | 6.7% | 85.0% |
| 0.60 | 88.9% | 4.4% | 88.9% |
| 0.65 | 72.2% | 2.2% | 92.9% |
| **0.70** | **55.6%** | **0.0%** | **100.0%** |
| 0.75 | 38.9% | 0.0% | 100.0% |
| 0.85 | 16.7% | 0.0% | 100.0% |
| 0.95 | 5.6% | 0.0% | 100.0% |

**The headline finding.** The reference guide suggests starting at 0.95. At 0.95, 5.6% of
genuine rewordings clear the bar — a cache that does not fire. That number is a property of
OpenAI's embedding geometry, where unrelated text already scores high; `nomic-embed-text`
puts unrelated questions at 0.48. 0.70 is the highest unrelated pair observed plus a
margin.

The distributions overlap — weakest paraphrase 0.5045, closest unrelated pair 0.6502 — so
no threshold catches every rewording without ever serving something wrong. The default sits
on the safe side.

Sample sizes are small (18 and 45 pairs from a hand-written workload). Treat the shape as
solid and the exact boundary as approximate. Re-run after changing the embedding model.

## Load test

```bash
make cache-bench
```

2,000 requests: 35% never asked before, 25% reworded, 40% verbatim repeats, against a
synthetic provider at 800ms per generation.

```
requests                         2000        hit rate         69.9%
hits                             1398        hits checked      1398
misses                            602        wrong hits           0
                                             false hit rate    0.00%
LATENCY            P50          P95          P99
hit              169µs       43.9ms       65.9ms
miss           856.0ms      884.2ms      901.4ms
```

Hits by tier: ~1,280 exact, ~115 namespace. **The exact tier carries the cache.** Semantic
matching adds roughly 8% more hits on top and, thanks to the namespace constraint, adds no
errors.

The false-hit number is a measurement, not an assumption: the synthetic provider stamps
each answer with a hash of the question it was generated for, and all 1,398 hits were
checked against every acceptable wording of the question asked. See the fixture caveats in
[testing.md](testing.md) — three earlier versions of this benchmark reported confidently
wrong numbers.

### Threshold sweep, same workload

| Threshold | Hit rate | False hits | Namespace-tier hits |
|---|---|---|---|
| 0.55 | 70.0% | 0.00% | 221 |
| 0.60 | 69.9% | 0.00% | 146 |
| 0.65 | 70.0% | 0.00% | 145 |
| 0.70 | 69.9% | 0.00% | 115 |
| 0.75 | 69.9% | 0.00% | 77 |
| 0.80 | 69.8% | 0.00% | 12 |

Zero false hits all the way down to 0.55. **This validates the central design claim**: with
the context hashed into the key, a namespace only ever contains different wordings of the
same question, so the threshold is a second line of defence rather than the primary one.
The hit rate barely moves because the exact tier dominates.

Reproduce a single point:

```bash
./bin/engrex-cache serve --synthetic-upstream 800ms --data /tmp/bench --threshold 0.65 &
./bin/engrex-cache loadtest --requests 2000 --synthetic
```

## End-to-end, real model

All against `llama3.2` and `nomic-embed-text`, on an M-series laptop.

| Path | Cold | Warm | Speedup |
|---|---|---|---|
| `engrex query` (daemon socket — what the app and CLI use) | 12.276s | 0.023s | 534x |
| `engrex ask` (in-process) | 13.571s | 0.051s | 266x |
| `curl /api/generate`, small grounded prompt | 2.64s | 17ms | 155x |
| `curl /api/chat` | 270ms | 13ms | 21x |
| `OLLAMA_HOST=… ollama run` | 0.498s | 0.025s | 20x |

Answers on the warm runs are byte-identical to the cold ones, which is what pinning
`temperature: 0` buys — without it the two runs would differ in wording and length and the
comparison would be meaningless.

Reproduce:

```bash
export ENGREX_OLLAMA_URL=http://127.0.0.1:11435
time ./bin/engrex query "what is cifar"   # twice
```

## The context-tolerant tier: benchmark and reality disagree

**On the synthetic benchmark** it earns 2 extra hits and both are wrong.

**On the real index** it does exactly what it was built for:

```
"tell me what is cifar"   →  13.1s   (cold)
"explain cifar to me"     →  0.655s  (tier: context-tolerant, correct answer reused)
```

Those two questions embed at **0.9355** similarity — far above any threshold in use — but
tier 1 still misses them, because retrieval returned *different passages* for the two
wordings, putting them in different namespaces where they never meet. The lookup log
recorded `near-miss samples: 0`, confirming the cache never even scored them against each
other. That is the tier-1 limitation in one measurement.

The disagreement is the fixture's fault, not the cache's: the benchmark gives every
generated question its own scattered passage set, so two different questions essentially
never retrieve overlapping context and the only things the guard lets through are
accidents. Real retrieval overlaps heavily between two wordings of one question.

It remains off by default — a tolerant hit is the only kind that can be wrong — but on a
real corpus it is worth enabling. `CACHE_FLAGS` in the Makefile controls this.

## Storage

- 768-dimension embedding: ~4KB base64 in the journal, against ~15KB as JSON decimals
- Default cap: 10,000 entries, LRU beyond that
- Graph rebuild triggers once tombstones exceed 25% of nodes

## What has not been measured

- Behaviour above ~10,000 entries. The cap has never been reached in testing, so eviction
  under sustained pressure is exercised by unit tests only.
- Memory under load. The index holds every embedding in memory; at the 10,000-entry cap
  that is roughly 30MB of vectors plus overhead, calculated rather than measured.
- Multi-client concurrency beyond the load test's 8 workers. `go test -race ./cache/`
  passes.
- Any provider other than Ollama.
