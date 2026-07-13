# Engrex — Knowledge Graph (web front-end)

A local-first, offline force-directed graph view of your Engrex notes, built with
vanilla JS + D3 v7. No build step, no npm, no framework, no CDN.

## Files

| File                | Purpose                                                        |
| ------------------- | ------------------------------------------------------------- |
| `index.html`        | Page shell: canvas, toolbar, legend, detail panel, states.    |
| `style.css`         | Dark, glassy visual style.                                    |
| `app.js`            | All logic: fetch, layout, interactions. Config at the top.    |
| `d3.v7.min.js`      | Vendored D3 v7.9.0 (local — works fully offline).             |
| `sample-graph.json` | ~10 fake nodes + edges in the exact `/graph` contract shape.  |

## Run against the daemon

The Go daemon serves this folder at `http://localhost:7778/` and the graph JSON at
`/graph` (same origin — no CORS). Just start the daemon and open:

```
http://localhost:7778/
```

`DATA_URL` in `app.js` already defaults to `/graph`.

## Develop / test standalone (no Go endpoint needed)

1. In `app.js`, flip the config constant at the top:
   ```js
   const DATA_URL = "./sample-graph.json"; // was "/graph"
   ```
2. Serve the folder and open it:
   ```sh
   cd web
   python3 -m http.server
   # open http://localhost:8000/
   ```

Switch `DATA_URL` back to `"/graph"` before shipping.

## What you get

- **Color = recency** (`createdAt`): older notes cool/dim, newer notes warm/bright.
  A legend in the bottom-left shows the scale with the oldest/newest dates.
- **Radius = degree**: more-connected notes are larger.
- **Link width/opacity = similarity** (`1 − distance`): stronger links are bolder.
- **Zoom + pan** anywhere on the canvas; **drag** a node to reposition it.
- **Hover** a node → highlights it and its neighbors, dims everything else.
- **Click** a node → detail panel with source, formatted date, full text, and a
  clickable list of connected notes (click one to select + center it).
- **Ask about this** → `POST /query` with the node text, renders the answer +
  sources. If `/query` isn't available yet, it copies a prefilled question to your
  clipboard and shows a small "Copied ✓" note instead.
- **Search box** → highlights nodes matching text or source, dims the rest.
- **Time filter** (24h / 7d / 30d / All) → filters nodes by `createdAt`, drops
  orphaned links, and re-runs the layout.
- **Empty state** when there are no nodes; **error banner** if the daemon is
  unreachable.

## Notes

- Edges are treated as **undirected**.
- Everything is same-origin with the daemon, so no CORS handling is needed.
- Fully offline: the only external-looking asset, D3, is vendored locally.
