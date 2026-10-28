# Fix the HNSW benchmark's query generation and rerun it

Handoff written 2026-10-28. Delete this file once the work is done.

## The problem

The published HNSW recall numbers in `docs/hnsw.md` (0.984 → 0.756 → 0.480 at 1K / 5K / 20K,
`efSearch=64`) are almost certainly an artifact of how the benchmark generates its **queries**,
not a property of the index.

`internal/hnsw/bench.go`, in both `Run` (line ~82) and `RunEfSweep` (line ~145):

```go
vectors := randomUnitVectorsBench(size, config.Dimensions, int64(size))
queries := randomUnitVectorsBench(config.Queries, config.Dimensions, int64(size)+1)
```

`randomUnitVectorsBench` draws its 60 cluster centroids from the seed it is given. Because the
queries use a different seed (`size+1`), they are generated around **60 different centroids that
contain no corpus vectors**. At 768 dimensions, random centroids are nearly orthogonal, so each
query sits roughly equidistant from every corpus vector. The exact top 10 is then a near
arbitrary pick among near ties, which is exactly the failure the comment above
`randomUnitVectorsBench` describes for uniform random vectors ("ANY approximate index scores
terrible recall against a ground truth that is itself noise"). The clustering fix was applied to
the corpus but not to the queries.

Supporting evidence: the documented uniform random run fell 0.94 → 0.56 → 0.24 with size; the
current run falls 0.984 → 0.756 → 0.480. Same shape, same cause, and it worsens with size because
more ties accumulate.

The index itself looks sound: `selectNeighbors` implements the paper's heuristic with backfill,
and `TestGraphIsConnectedAtBottomLayer` exists. Do not change `hnsw.go` unless the fixed benchmark
shows a real problem.

## What to do

1. **Fix query generation** so queries come from the same distribution as the corpus. Preferred:
   generate `size + config.Queries` vectors with one seed and hold out the last `config.Queries`
   as queries, so queries are never also in the index (an exact self match would inflate recall).
   Apply the same fix in both `Run` and `RunEfSweep`; factor it into one helper so the two cannot
   drift again.
2. **Add a guard against this class of bug** in `internal/hnsw/bench.go` or a test: for a sample
   of queries, check that the distance to the exact nearest neighbor is clearly smaller than the
   median distance to the corpus (for example nearest < 0.8 × median). If the ground truth is a
   pile of near ties, fail loudly instead of reporting a recall number. Write a test that proves
   the guard trips on the old generator (different seed) and passes on the new one.
3. **Rerun the benchmark** with the same settings the published table used, so the numbers are
   comparable: 768 dimensions, `M=16`, `efConstruction=100`, `efSearch=64`, k=10, 25 queries,
   sizes 1,000 / 5,000 / 20,000. Then the efSearch sweep at 5,000 vectors (`--ef-sweep-size 5000`).
   Check `cmd/engrex/bench.go` for the exact flags. The 20K build took ~1m37s last time; run it in
   the background if needed.
4. **If recall at 20K is still well below ~0.9 at `efSearch=64`**, report it before tuning
   anything, then try in this order and record each result: `efSearch` 128/200, `M=32`,
   `efConstruction=200`. Do not tune silently; every number that ends up in the doc needs the
   parameters next to it.
5. **Update `docs/hnsw.md`:**
   - Replace the Results and efSearch tables with the new numbers.
   - In "Benchmark methodology, and a trap worth documenting", add the query seed bug as a second
     trap, with the old numbers kept and labelled as invalid and why. Do not delete the history.
   - Rewrite "What the numbers actually say" to match the new data. The current text argues that
     recall degrading with size is "the central property of the structure"; that claim may not
     survive the rerun.
   - Re-evaluate "Why it is here anyway" honestly. Exact search is still fine for a 21 chunk live
     corpus (15ms at 20K), so the default probably stays exact, but the reason has to be the
     corpus size and simplicity, not a 0.48 recall that came from a flawed benchmark.
6. Run the full test suite (`go test ./...`) and make sure it passes.

## Constraints

- Follow the repo's existing style and my global CLAUDE.md: braces on every control block, full
  descriptive names, plain short comments, no banner dividers, no `/* */`.
- Record the session in `docs/tests/YYYY-MM-DD-hnsw-benchmark-fix.md` per my test report rule,
  including machine, Go version, exact commands, and median of several runs for latency.
- **Do not commit or push.** Leave the changes staged or unstaged and tell me.

## What to report back

A short summary plus a table I can copy into my resume repo's Engrex dossier:

| Vectors | Exact p50 | HNSW p50 | Speedup | recall@10 | Params |
|---|---|---|---|---|---|

plus the efSearch sweep at 5,000 vectors, and one sentence on whether the "keep exact search as
the default" decision still holds and why.
