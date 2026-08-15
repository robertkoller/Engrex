# Evaluation

Engrex scores its own retrieval. `engrex eval` runs a golden set of questions through
the real retrieval path and reports recall@k, MRR, and latency against a committed
baseline, so a change to chunking, embedding, or ranking can be judged by its effect
rather than by feel.

This exists because every threshold in the system was originally set by intuition. It
is the prerequisite for every later retrieval change — see
[rag-upgrade-plan.md](rag-upgrade-plan.md).

## Running it

```
engrex eval                          # score against the committed baseline
engrex eval --save --label baseline  # freeze this run's numbers as the new baseline
engrex eval --set eval/mine.json     # use a different golden set
```

Requires Ollama running, since every question is really embedded and really retrieved.

## The golden set

`eval/golden.json`. One entry per question:

```json
{
  "question": "how does the daemon avoid double-ingesting a file?",
  "relevant": ["daemon.md", "ingestion.md"],
  "hops": 2,
  "note": "why this case is here"
}
```

- **`question`** is asked verbatim, exactly as you would type it.
- **`relevant`** are matched as **substrings** of a retrieved chunk's source path or
  origin URL, so `"daemon.md"` matches that file wherever it lives. Substrings rather
  than absolute paths so the set survives files being moved.
- **`hops`** of 2 or more marks a question needing evidence from several documents.
- **`note`** is for you; it never affects scoring.

### Writing a good set

Aim for ~40 cases. The set is only useful if it can fail:

- **Weight it toward questions you actually ask.** A set of questions invented to be
  answerable measures nothing.
- **Include the failures.** Questions current retrieval gets wrong are the most
  valuable entries — they are what shows a change worked.
- **Vary the shape.** Natural questions, bare keyword queries, exact identifiers, and
  multi-part questions stress different halves of hybrid retrieval.
- **Mark multi-hop cases honestly.** Overstating `hops` inflates the slice that later
  justifies agentic retrieval.

## Metrics

| Metric | What it answers |
|---|---|
| **recall@1, @3** | Is the right document at the very top? What matters with no reranker. |
| **recall@10, @20** | Is it anywhere in the pool a reranker could pull up? Sets the ceiling for Phase 4. |
| **MRR** | Mean reciprocal rank of the first correct hit — one number for "how high does the right answer land". |
| **latency p50 / p95** | What the quality cost. Every later phase trades latency for quality; this is the other side of that trade. |

Recall counts **distinct** expected sources, so a case listing three relevant documents
is not scored as solved by retrieving one of them three times.

### The hop split

When the set contains multi-hop cases, the report breaks single-hop and multi-hop
performance out separately. That gap is the entire argument for or against multi-hop
retrieval: if multi-hop questions score as well as single-hop ones, an agentic loop is
latency with no payoff.

### The miss list

Below the metrics, every case that retrieved **nothing** relevant is listed with what
was wanted and what came back. Aggregate scores say whether a change helped; the miss
list says which questions are still broken and why.

## Baselines

`eval/baseline.json` holds a previous run's numbers. When present, `engrex eval` prints
a delta column. Both files are committed — the point of a baseline is comparing against
what was there before a change, which requires it to be in version control.

Save a new baseline only when you have decided a change is an improvement. Overwriting
it after every run defeats the purpose.

## Health check

`engrex doctor` reports the things retrieval quality silently depends on:

- **Raw vs. stored vector magnitude** — whether the embedding model returns unit-length
  vectors. It does not, which is why Engrex normalizes; cosine distance is only well
  behaved on unit vectors.
- **Schema version** — which migrations have run.
- **Chunk count vs. vector count** — a mismatch means the vector index is stale and
  `engrex reindex` is needed.

## Reindexing

`engrex reindex` re-embeds every stored chunk from the text already in the database and
rebuilds the graph edges. Run it after any upgrade that changes how text is embedded —
vectors written by an older scheme sit in a different space and cannot be meaningfully
compared against new query vectors. Chunk ids, sources, and metadata are preserved, and
nothing is re-read from disk.
