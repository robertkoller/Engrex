# Testing

54 tests, all offline — no Ollama, no network, a few seconds under `-race`.

```bash
go test ./cache/ -v          # no build tags, no CGo
make test                    # whole repo
```

## Conventions

Follows what the rest of the repo already does: `httptest.NewServer` with a
constructor-injected base URL, no mocking framework, no build tags (the repo has zero
`//go:build` lines). The `fakeOllama` helper mirrors the one copy-pasted into
`internal/rerank`, `internal/verify` and `internal/rewrite`.

Two things this package does differently, both because the alternative was weaker:

**Real prompts, not fixtures.** `internal/rerank`, `internal/rewrite` and `internal/verify`
all build their prompts in unexported functions, so a test cannot call them. Hand-copying
the prompt into a fixture would drift the moment one is edited — and drift silently, since
the splitter degrades to the tail rule rather than failing. Instead `capturingOllama`
records what the real builder sent, and the splitter is tested against that:

```go
server, captured := capturingOllama(t, "3,1,2")
reranker := rerank.NewLLM(server.URL, "test-model")
reranker.Rerank(question, passages, 2)
parts, ok := DefaultSplitter().Split(*captured)   // the prompt the real builder emitted
```

`rag.buildPrompt` is the exception — reaching it needs a live store — so the answer prompt
is a fixture in `answerPrompt`, and it is the one thing here that can drift.

**A controllable embedder.** `fakeEmbedder.relate(a, b, 0.99)` registers a vector at a
chosen cosine distance from another, so a test can say "these two questions are 99%
similar" without a model. Unregistered text gets a deterministic near-orthogonal vector.

## What the tests prove

### The safety properties

These are the reason the design is shaped the way it is. If one of these breaks, the cache
is worse than useless.

| Test | Property |
|---|---|
| `TestAnswerIsNeverServedForDifferentContext` | The identical question over different passages must miss |
| `TestPassageCannotHijackTheSplit` | A saved note containing `QUESTION:` cannot steer how its own prompt is split |
| `TestEmbeddingsAreNeverMatchedApproximately` | An embedding is refused even at 0.9999 similarity |
| `TestSystemPromptSplitsTheChatCache` | Two identical questions under different system prompts do not share an entry |
| `TestDifferentQuestionsOverIdenticalContextStaySeparable` | Identical passages share a namespace; different questions stay distinguishable inside it |
| `TestInterruptedStreamIsNotCached` | A stream that dies halfway is never stored |

### The splitter

`TestSplitsRealRerankPrompt`, `TestSplitsRealVerifyPrompt`,
`TestSplitsRealRewritePromptPastItsFewShotExample` — against prompts the real builders
emit. The rewrite one is the interesting case: that prompt contains a few-shot example
beginning `QUESTION: `, so it pins the last-occurrence search.

`TestSplitsAnswerPrompt`, `TestTailSplitterHandlesUnknownPrompts`,
`TestShortPromptIsAllSemantic` cover the answer format and the two fallbacks.

### Keys

`TestIdenticalContextSharesANamespace`, `TestModelAndSamplingSplitTheNamespace`,
`TestContextWindowDoesNotSplitTheNamespace`, `TestOptionOrderIsIrrelevant`,
`TestExactKeyTracksTheWholeText`.

`TestOptionOrderIsIrrelevant` loops 50 times deliberately: Go randomises map iteration, so
an unsorted rendering of the options would give the same request a different key on most
calls and the cache would essentially never hit. One iteration could pass by luck.

`TestContextWindowDoesNotSplitTheNamespace` pins the decision to leave `num_ctx` out of the
key. Engrex derives it from the prompt's own length, so two wordings of one question can
land on either side of a boundary — keying on it would guarantee a miss in exactly the case
the cache exists for.

### Index and storage

`TestNamespaceSearchIgnoresOtherNamespaces`, `TestExpiredEntriesAreNotReturned`,
`TestRemovedEntriesAreNotReturned`, `TestRebuildKeepsLiveEntriesAndClearsTheDead`,
`TestAddCopiesTheEmbedding`, `TestVectorlessEntryDoesNotBreakTheIndex`,
`TestJournalRoundTrip`, `TestTornFinalLineIsDropped`, `TestCompactDropsHistory`.

### Proxy

`TestStreamingMissIsRelayedThenServedFromCache`, `TestNonStreamingCallsRoundTrip`,
`TestEmbeddingsRoundTripThroughTheProxy`, `TestChatRoundTripsAndCaches`,
`TestUnknownPathsArePassedThrough`, `TestLongCachedAnswerSurvivesADefaultScanner`.

### Policy

`TestExactRepeatIsAHit`, `TestParaphraseHitsWithinANamespace`,
`TestVolatileQuestionsAreNotCached`, `TestVolatilityIsJudgedOnTheQuestionNotThePassages`,
`TestExpiredEntriesAreEvicted`, `TestLeastRecentlyUsedEntriesAreEvictedAtCapacity`,
`TestContextTolerantHitSurvivesOneSwappedPassage`,
`TestContextTolerantHitRefusesUnrelatedPassages`, `TestContextTolerantIsOffByDefault`.

## Checking that a test can still fail

A test that cannot fail proves nothing. The 64KB replay test is the one most worth
verifying, because the failure it guards against is silent for any client that reads the
stream with a default `bufio.Scanner` and skips `scanner.Err()`, as `rag.Query` used to.

```bash
sed -i.bak 's/^const replayChunk = 2048$/const replayChunk = 100000000/' cache/ollama.go
go test ./cache/ -run TestLongCachedAnswerSurvivesADefaultScanner
#   FAIL ... the scanner gave up: bufio.Scanner: token too long
mv cache/ollama.go.bak cache/ollama.go
```

The same exercise works for the others: set `defaultMinContextOverlap` to 0 and the
tolerant guard test fails; return the whole prompt from `Split` and the context-separation
tests fail.

## Bugs these tests were written for

Every one of these was a real failure found during the build, not a hypothetical.

**The panic on vectorless entries.** Embed entries carry no vector; storing one seeded the
HNSW graph with a zero-length node and the next real insert panicked, killing the request
handler mid-stream. It fired on the ordinary query sequence — embed, then generate — so it
would have affected essentially any real use. The test suite missed it because the one test
storing an embed entry stored it *first and only*, and the panic needs an empty-vector node
followed by a non-empty one.

**Silent truncation on long answers.** Replaying a cached answer as a single NDJSON frame
exceeds `bufio.Scanner`'s 64KB line limit, which returns false without an error.

**Splitting on a few-shot example.** The rewrite prompt's own example begins `QUESTION: `.

**Stored content steering its own split.** Identifying a prompt format by searching for
delimiters lets a saved note contain them.

## Benchmark correctness, and two fixtures that lied

The load test measures whether hits are *right*, not just fast. The synthetic provider
stamps every answer with a hash of the question it was generated for, and the driver checks
that stamp against every acceptable wording of the question asked. That turns "false hit
rate" into a measurement rather than an assumption.

Getting that measurement honest took three corrections, all of them to the fixture rather
than the cache — worth recording, because each one produced a confident and wrong number
first:

1. **Ground truth was too strict.** A paraphrase served the original's answer was scored
   *wrong*, because the stamp came from the literal question string. Serving "what is the
   dedup threshold?" for "what dedup threshold does engrex use?" is the cache working.
   Fixed by accepting any wording of the same underlying question. Before the fix the
   namespace tier looked ~50% wrong at every threshold.

2. **Unique questions were not unique.** They were generated as `base + " (variant 12)"`,
   which is ~99% identical text to the base, so the supposedly-unservable share of traffic
   embedded almost on top of it and the hit rate came out flattered. Fixed by composing
   questions from a topic pool; measured pairwise median similarity dropped to 0.51,
   matching the "different questions" distribution.

3. **The corpus was too small and too uniform.** Ten passages meant only ten distinct
   retrieval windows, so hundreds of questions crowded into each namespace and gave
   similarity hundreds of chances to match the wrong one. Consecutive windows also made two
   different questions share four of five passages, which sailed through the overlap guard.
   And every generated passage used one sentence template, so MinHash saw everything as
   overlapping. Fixed by expanding to ~210 varied passages selected by a scattered hash.

Before those fixes the benchmark reported a 52% false-hit rate and a 100%-wrong tolerant
tier. Afterwards: 0.00% false hits, and the tolerant tier's two hits are the only wrong
ones. **The lesson is that a benchmark is code and can be wrong in the direction that makes
your system look broken, not only in the direction that flatters it.**

### Concurrency, request shape and durability

- `TestConcurrentTrafficIsRaceFree` drives hits, misses, evictions and journal writes from
  eight goroutines. Under `-race` it caught `touch` writing `LastUsedAt` while the journal
  serialized the same entry.
- `TestIdenticalConcurrentMissesAreCoalesced` — six identical concurrent requests, one
  generation.
- `TestRequestFieldsOutsideThePromptSplitTheKey`, `TestEmptyFieldsDoNotSplitTheKeyOrBlockCaching`,
  `TestUnreplayableRequestsBypassTheCache`, `TestOptionsAreRenderedUnambiguously`,
  `TestVariantSplitsTheKey` — `format`, `system` and friends are keyed; images, tools and
  `context` bypass.
- `TestReasoningSurvivesTheCache` — a qwen3-style `thinking` field is stored and replayed,
  streamed and not. Found on real Ollama 0.30.8, where a qwen3 reply can have an empty
  `response` and everything in `thinking`.
- `TestUpstreamErrorsAreRelayedAsSent` — a 404 stays a 404 with Ollama's message.
- `TestJournalIsCompactedAndSurvivesRestart`, `TestStoringTheSameRequestTwiceKeepsOneEntry`,
  `TestIngestDropsAnswersOnlyWhenTolerantMatchingIsOn`.
- `TestSketchEstimatesJaccardAccurately` holds the MinHash to the ~0.06 error 64 hashes
  should give. The previous `base*(2p+1)+p` permutation measured about 0.11.
- `internal/rag/cache_split_test.go` runs the real `buildPrompt` and `buildNoContextPrompt`
  through the splitter, so the answer markers cannot drift unnoticed. It needs sqlite, so
  it runs under `make test`, not `go test ./cache/`.

## What is not covered

- The OpenAI and Anthropic providers are stubs and are not tested beyond refusing.
- The dashboard HTML is rendered and checked by hand, not in CI. It has been verified to
  load with live data and no console errors, in dark mode only.
- `go test -race ./cache/` is not part of `make test`; run it after touching locking.
- The tolerant tier's behaviour on a real corpus is measured by hand (see
  [measurements.md](measurements.md)), not asserted in a test — it depends on a populated
  index.
