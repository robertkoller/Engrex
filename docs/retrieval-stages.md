# Retrieval Stages

Three optional stages sit around the hybrid retrieval core: **query rewriting** before
it, **reranking** after it, and **citation verification** after generation. All three
are off by default and opt-in per query.

They are off by default deliberately. Each costs at least one extra local LLM call, and
none of them has been shown to help on this corpus yet — the golden set is still ten
starter cases over a single document. See [evaluation.md](evaluation.md).

```
question
   │
   ├─▶ rewrite      (optional)  decompose into sub-queries
   │
   ▼
hybrid retrieval               vector KNN + BM25, fused with RRF
   │
   ├─▶ rerank       (optional)  40 candidates → best 10
   │
   ▼
prompt + generate
   │
   └─▶ verify       (optional)  check each claim against the passages
```

## Usage

```bash
engrex ask "your question" --rerank
engrex ask "your question" --rewrite
engrex ask "your question" --verify
engrex ask "your question" --all
```

`ask` runs in-process rather than through the daemon, so stages are a property of the
question rather than of the daemon's configuration.

Scoring them:

```bash
engrex eval                    # baseline hybrid
engrex eval --rerank           # with reranking
engrex eval --rewrite --rerank # both
```

## Reranking — `internal/rerank`

Retrieval optimizes for recall: cast wide, accept noise. Reranking converts that recall
back into precision by scoring each passage against the query directly, rather than
comparing two independently-produced embeddings. That is what makes over-fetching safe
— with a reranker attached, retrieval widens from 20 candidates to 40.

The implementation is **listwise**: every candidate is shown to the model at once and it
returns their order in a single call. Listwise rather than pointwise because one call is
far cheaper than N, and because seeing candidates together lets the model judge relative
relevance — the actual question — instead of assigning absolute scores that then have to
be compared across independent calls.

**This is the weaker of the two standard options.** A cross-encoder
(`bge-reranker-v2-m3`, `Qwen3-Reranker`) scores query and passage in a single forward
pass with full cross-attention between them, and would rank better. It needs an ONNX
runtime or a `llama.cpp` sidecar — a second inference dependency in a project that
currently has exactly one. It would implement the same `Reranker` interface, so
swapping it in is a constructor change.

Failure behavior: a reranker that errors, times out, or returns unparseable output
returns the input order unchanged. A reranking outage costs precision, never
availability. Passages the model omits are appended in their original order, so the
output is always a permutation of the input and nothing silently disappears.

## Query rewriting — `internal/rewrite`

A single embedding of a raw question is a poor search key in two cases: a multi-part
question averages into a vector sitting between both topics and matching neither well,
and a follow-up carries almost no retrievable content on its own.

A **cheap syntactic gate** decides whether to rewrite at all — multiple question marks,
conjunctions like "and" / "versus" / "difference between", or more than 25 words. Most
questions are simple, and asking a model "is this multi-part?" before every search would
double the latency of the common case to help the rare one.

When it does fire, sub-queries are retrieved independently and their rankings fused with
RRF. **The original question is always kept and searched too**, so a bad rewrite can
only add candidates, never remove them. Sub-queries are capped at 4.

Note that reranking always scores against the user's *actual* question, never a
rewritten sub-query — the sub-queries widen recall, but relevance is judged against what
was asked.

## Citation verification — `internal/verify`

The prompt asks for grounded answers; nothing checked that it got one. This project has
already hit the failure directly: a model producing fluent, confident, entirely invented
content while correct passages sat in its context. Asking the model to behave is not a
control. Checking the output is.

After generation, the answer is split into sentence-level claims and each is tested
against the retrieved passages: *does any passage state or directly imply this?* The
result is a groundedness rate plus a list of unsupported claims.

Two deliberate choices:

- **Hedges are excluded from scoring.** "The context does not mention the error rate"
  asserts no fact, and marking it unsupported would penalise exactly the honest behavior
  the answer prompt asks for. Such sentences count toward neither numerator nor
  denominator.
- **Unparseable verdicts count as unsupported.** Treating an ambiguous judgment as
  support would let the failure this package exists to catch slip through silently.

Verification runs *after* the answer has streamed, so it annotates rather than
suppresses. For a local assistant that is the right trade: the user has already read the
answer by the time a 3B model finishes grading it, and flagging which sentences their
notes do not support beats withholding the answer.

### Known limitation

The verifier is only as good as the judge, and `llama3.2:3b` is a weak one. Observed on
this corpus: a claim that *was* supported by the retrieved table was flagged
unsupported — a false positive. Entailment is a narrow judgment, but it is still a
judgment, and a 3B model makes it unreliably. Treat the grounding percentage as a
signal, not a measurement, until it runs on a larger model.
