# HTTP surface

Two listeners, on separate ports so scrape and dashboard traffic never share a handler
with the hot path.

| Port | Purpose |
|---|---|
| `11435` | The proxy. Point Ollama clients here instead of `11434` |
| `11436` | Metrics and dashboard |

The proxy port is deliberately adjacent to Ollama's: it *is* an Ollama endpoint, so it
reads as one. 7777 and 7778 are already taken by Engrex's extension and graph servers.

## Proxy — `:11435`

### Cached endpoints

| Path | Cached as | Semantic part |
|---|---|---|
| `/api/generate` | `answer` / `rerank` / `rewrite` / `verify` / `other` | The question, per the marker table |
| `/api/chat` | by flattened conversation | The last message; system prompt and history are context |
| `/api/embed` | `embed`, exact match only | The whole input |

Everything else — `/api/tags`, model management, anything added upstream — is
reverse-proxied untouched. Putting this in front of Ollama never removes a capability.

A batch embedding request (`input` as an array rather than a string) is passed straight
through rather than half-handled: caching it would need a key per element.

Every request field besides the prompt that can change the output — `system`, `format`,
`template`, `raw`, `suffix`, `think`, and for embeddings `truncate`, `dimensions` and
`options` — is folded into the key, so a request asking for JSON never gets a prose
answer cached for the same prompt. `keep_alive` is ignored, and empty values count as
absent. Requests carrying `images`, `tools`, or Ollama's `context` token array are passed
through uncached, since none of those are in the text the cache keys on. A reasoning
model's separate `thinking` output is stored and replayed ahead of the answer.

A non-200 from Ollama is relayed with its own status and body, so a missing model is still
a 404 with Ollama's message. Identical requests arriving while the first is still
generating wait for it and are then served as exact hits, so a burst costs one generation.

### Response headers

| Header | Values |
|---|---|
| `X-Engrex-Cache` | `hit` \| `miss` \| `bypass` |
| `X-Engrex-Cache-Tier` | `exact` \| `namespace` \| `context-tolerant` (hits only) |
| `X-Engrex-Cache-Similarity` | Cosine score of the match, 4dp |
| `X-Engrex-Cache-Saved-Ms` | How long the stored answer originally took to generate |

`bypass` means the request was understood but deliberately not cached — a time-referencing
question, a batch embed, or a request carrying images, tools or `context`.

```bash
curl -sD - -X POST localhost:11435/api/generate --data-binary @body.json -o /dev/null \
  | grep -i x-engrex
```

### Admin

```
POST /cache/invalidate    {"all":true} | {"model":"llama3.2"} | {"class":"answer"} | {"tag":"..."}
GET  /cache/stats         same JSON as the dashboard's /api/stats
```

An empty criteria matches nothing, so a malformed body cannot empty the cache.

## Metrics and dashboard — `:11436`

```
GET /              the dashboard, a single embedded HTML page, no CDN
GET /api/stats     JSON snapshot
GET /metrics       Prometheus text exposition
```

### Series

| Metric | Type | Labels |
|---|---|---|
| `engrex_cache_lookups_total` | counter | `outcome` = hit/miss/bypass/error |
| `engrex_cache_hits_by_tier_total` | counter | `tier` |
| `engrex_cache_lookups_by_class_total` | counter | `class`, `outcome` |
| `engrex_cache_evictions_total` | counter | `reason` |
| `engrex_cache_hit_rate` | gauge | — |
| `engrex_cache_entries` | gauge | — |
| `engrex_cache_time_saved_seconds` | gauge | — |
| `engrex_cache_hit_latency_microseconds` | summary | quantiles 0.5 / 0.95 / 0.99 |
| `engrex_cache_miss_latency_microseconds` | summary | quantiles 0.5 / 0.95 / 0.99 |
| `engrex_cache_near_miss_similarity` | summary | quantiles 0.5 / 0.95 / 0.99 |

`near_miss_similarity` is the distribution the threshold tuner works from: a miss at 0.69
against a 0.70 threshold is a hit a slightly looser setting would have caught.

The dashboard's "Speedup, in-proxy" tile is miss-p50 over hit-p50, both measured inside the
handler. It runs to five figures because a hit does no I/O beyond writing the response —
the end-to-end figure a client sees is much lower, and is the honest one to quote.

## CLI

```
engrex-cache serve       [--proxy --dashboard --upstream --data --tolerant
                          --threshold --max-entries --synthetic-upstream]
engrex-cache enable      [--proxy]     write ollama_url into ~/.engrex/config.json
engrex-cache disable                   clear it
engrex-cache stats       [--dashboard]
engrex-cache calibrate   [--upstream]  measure paraphrase separation
engrex-cache tune        [--class --data]
engrex-cache loadtest    [--proxy --model --requests --concurrency
                          --unique --paraphrase --seed --synthetic]
engrex-cache invalidate  [--all --model --class --tag --proxy]
```

`enable` writes config rather than being auto-detected. Whether the cache is in the path
should never be a guess, least of all while measuring it. The daemon resolves that value
**once at construction**, so a change needs a daemon restart — `make stack-start` handles
the ordering.
