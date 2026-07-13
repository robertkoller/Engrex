"use strict";

// ---------------------------------------------------------------------------
// Configuration
//
// The daemon serves the live graph at "/graph" on http://localhost:7778.
// To develop this front-end standalone (before the Go endpoint exists), flip
// DATA_URL to "./sample-graph.json" and serve the folder with:
//     python3 -m http.server
// then open http://localhost:8000/
// ---------------------------------------------------------------------------
const DATA_URL = "/graph"; // <-- switch to "./sample-graph.json" for standalone dev
const QUERY_URL = "/query"; // POST { text } -> { answer, sources } (may not exist yet)

// Theme — mirrors the macOS app's selected theme, passed via URL params
// (?c1=..&c2=..&c3=..) so the graph matches the app. Defaults to "Aurora".
const THEME = (function readTheme() {
  const params = new URLSearchParams(location.search);
  const norm = (value, fallback) => {
    if (!value) return fallback;
    return value.charAt(0) === "#" ? value : "#" + value;
  };
  return {
    c1: norm(params.get("c1"), "#8c5cf5"),
    c2: norm(params.get("c2"), "#4d99fa"),
    c3: norm(params.get("c3"), "#47ccc7"),
  };
})();

// Push the theme into the CSS custom properties so the whole UI recolors.
(function applyTheme() {
  const root = document.documentElement.style;
  root.setProperty("--c1", THEME.c1);
  root.setProperty("--c2", THEME.c2);
  root.setProperty("--c3", THEME.c3);
})();

// Nodes are colored across the theme gradient by a stable hash of their id — no
// date/recency meaning, just an on-brand spread of color.
const themeInterpolator = d3.interpolateRgbBasis([THEME.c1, THEME.c2, THEME.c3]);
const NODE_RADIUS = 7; // fixed — nodes are NOT scaled by connectivity

function colorForNode(node) {
  const id = Number(node.id) || 0;
  let fraction = (Math.sin(id * 12.9898) * 43758.5453) % 1;
  if (fraction < 0) fraction += 1;
  return themeInterpolator(fraction);
}

// ---------------------------------------------------------------------------
// Module state
// ---------------------------------------------------------------------------
let allNodes = []; // raw nodes from the contract (never mutated by the layout)
let allEdges = []; // raw edges: { source: id, target: id, distance }
let currentNodes = []; // simulation node objects
let currentLinks = []; // link objects (source/target become node refs after first tick)
let neighborMap = new Map(); // node id -> Set of neighbor node ids
let selectedNodeId = null;
let hoveredNodeId = null;
let searchMatchIds = null; // Set of ids matching the search, or null when search is empty

// D3 selections / objects created once in setupCanvas().
let svg = null;
let viewport = null; // <g> that zoom transforms
let linkGroup = null;
let nodeGroup = null;
let zoomBehavior = null;
let simulation = null;

// ---------------------------------------------------------------------------
// Bootstrap
// ---------------------------------------------------------------------------
document.addEventListener("DOMContentLoaded", function () {
  setupCanvas();
  wireControls();
  loadGraph();
});

function setupCanvas() {
  svg = d3.select("#graph");
  viewport = svg.append("g").attr("class", "viewport");
  linkGroup = viewport.append("g").attr("class", "links");
  nodeGroup = viewport.append("g").attr("class", "nodes");

  zoomBehavior = d3
    .zoom()
    .scaleExtent([0.15, 6])
    .on("zoom", function (event) {
      viewport.attr("transform", event.transform);
    });
  svg.call(zoomBehavior);

  // Clicking empty canvas clears any selection/highlight.
  svg.on("click", function (event) {
    if (event.target === svg.node()) {
      clearSelection();
    }
  });

  window.addEventListener("resize", function () {
    if (simulation) {
      const size = viewportSize();
      simulation.force("center", d3.forceCenter(size.width / 2, size.height / 2));
      simulation.alpha(0.2).restart();
    }
  });
}

function viewportSize() {
  return { width: window.innerWidth, height: window.innerHeight };
}

// ---------------------------------------------------------------------------
// Data loading
// ---------------------------------------------------------------------------
async function loadGraph() {
  show("#loading");
  hide("#banner");
  try {
    const response = await fetch(DATA_URL, { cache: "no-store" });
    if (!response.ok) {
      throw new Error("HTTP " + response.status);
    }
    const data = await response.json();
    hide("#loading");
    ingest(data);
  } catch (error) {
    hide("#loading");
    showBanner(
      "Can't reach the Engrex daemon at " +
        DATA_URL +
        " — is it running on localhost:7778? (" +
        error.message +
        ")"
    );
  }
}

function ingest(data) {
  allNodes = Array.isArray(data.nodes) ? data.nodes : [];
  allEdges = Array.isArray(data.edges) ? data.edges : [];

  if (allNodes.length === 0) {
    show("#empty-state");
    return;
  }
  hide("#empty-state");

  // No filtering — build the graph once from everything.
  render(allNodes, allEdges);
}

// ---------------------------------------------------------------------------
// Rendering + simulation
// ---------------------------------------------------------------------------
function render(nodes, edges) {
  // Preserve positions across re-filters so the layout doesn't teleport.
  const previousById = new Map(
    currentNodes.map(function (node) {
      return [node.id, node];
    })
  );

  currentNodes = nodes.map(function (node) {
    const previous = previousById.get(node.id);
    const copy = Object.assign({}, node);
    if (previous) {
      copy.x = previous.x;
      copy.y = previous.y;
      copy.vx = previous.vx;
      copy.vy = previous.vy;
    }
    return copy;
  });

  // Fresh link objects each render (forceLink rewrites source/target to refs).
  currentLinks = edges.map(function (edge) {
    return { source: edge.source, target: edge.target, distance: edge.distance };
  });

  computeNeighbors();

  const linkSimilarityWidth = d3.scaleLinear().domain([0, 1]).range([1, 4]).clamp(true);
  const linkSimilarityOpacity = d3.scaleLinear().domain([0, 1]).range([0.12, 0.7]).clamp(true);

  // Links
  const linkSelection = linkGroup
    .selectAll("line.link")
    .data(currentLinks, linkKey)
    .join("line")
    .attr("class", "link")
    .attr("stroke-width", function (link) {
      return linkSimilarityWidth(similarity(link.distance));
    })
    .attr("stroke-opacity", function (link) {
      return linkSimilarityOpacity(similarity(link.distance));
    });

  // Nodes (a group holding a circle + a label)
  const nodeSelection = nodeGroup
    .selectAll("g.node")
    .data(currentNodes, function (node) {
      return node.id;
    })
    .join(
      function (enter) {
        const group = enter
          .append("g")
          .attr("class", "node")
          .on("mouseover", function (event, node) {
            hoveredNodeId = node.id;
            refreshDimming();
          })
          .on("mouseout", function () {
            hoveredNodeId = null;
            refreshDimming();
          })
          .on("click", function (event, node) {
            event.stopPropagation();
            selectNode(node.id, true);
          })
          .call(dragBehavior());
        group.append("circle");
        group
          .append("text")
          .attr("class", "node-label")
          .attr("dy", NODE_RADIUS + 11);
        return group;
      },
      function (update) {
        return update;
      },
      function (exit) {
        return exit.remove();
      }
    );

  nodeSelection
    .select("circle")
    .attr("r", NODE_RADIUS)
    .attr("fill", function (node) {
      return colorForNode(node);
    });

  nodeSelection
    .select("text.node-label")
    .attr("dy", NODE_RADIUS + 11)
    .text(function (node) {
      return shorten(node.label || node.text || "", 28);
    });

  // Simulation
  const size = viewportSize();
  if (simulation) {
    simulation.stop();
  }
  simulation = d3
    .forceSimulation(currentNodes)
    .force(
      "link",
      d3
        .forceLink(currentLinks)
        .id(function (node) {
          return node.id;
        })
        .distance(function (link) {
          // More similar (smaller distance) => shorter, tighter link.
          return 55 + link.distance * 190;
        })
        .strength(function (link) {
          return 0.05 + similarity(link.distance) * 0.5;
        })
    )
    .force("charge", d3.forceManyBody().strength(-280))
    .force("center", d3.forceCenter(size.width / 2, size.height / 2))
    .force(
      "collide",
      d3.forceCollide().radius(NODE_RADIUS + 6)
    )
    .on("tick", function () {
      linkSelection
        .attr("x1", function (link) {
          return link.source.x;
        })
        .attr("y1", function (link) {
          return link.source.y;
        })
        .attr("x2", function (link) {
          return link.target.x;
        })
        .attr("y2", function (link) {
          return link.target.y;
        });
      nodeSelection.attr("transform", function (node) {
        return "translate(" + node.x + "," + node.y + ")";
      });
    });

  simulation.alpha(1).restart();

  // Reapply any active selection/search/hover styling to the new selections.
  refreshSelectionClass();
  refreshDimming();
}

function computeNeighbors() {
  neighborMap = new Map();
  currentNodes.forEach(function (node) {
    neighborMap.set(node.id, new Set());
  });
  currentLinks.forEach(function (link) {
    const sourceId = idOf(link.source);
    const targetId = idOf(link.target);
    const sourceSet = neighborMap.get(sourceId);
    const targetSet = neighborMap.get(targetId);
    if (sourceSet) {
      sourceSet.add(targetId);
    }
    if (targetSet) {
      targetSet.add(sourceId);
    }
  });
}

function dragBehavior() {
  return d3
    .drag()
    .on("start", function (event, node) {
      if (!event.active) {
        simulation.alphaTarget(0.25).restart();
      }
      node.fx = node.x;
      node.fy = node.y;
    })
    .on("drag", function (event, node) {
      node.fx = event.x;
      node.fy = event.y;
    })
    .on("end", function (event, node) {
      if (!event.active) {
        simulation.alphaTarget(0);
      }
      node.fx = null;
      node.fy = null;
    });
}

// ---------------------------------------------------------------------------
// Highlight / dimming (hover + search)
// ---------------------------------------------------------------------------
function refreshDimming() {
  let activeIds = null; // null => everything at full strength

  if (hoveredNodeId !== null) {
    activeIds = new Set([hoveredNodeId]);
    const neighbors = neighborMap.get(hoveredNodeId);
    if (neighbors) {
      neighbors.forEach(function (id) {
        activeIds.add(id);
      });
    }
  } else if (searchMatchIds !== null) {
    activeIds = searchMatchIds;
  }

  nodeGroup.selectAll("g.node").style("opacity", function (node) {
    if (!activeIds) {
      return 1;
    }
    return activeIds.has(node.id) ? 1 : 0.14;
  });

  linkGroup.selectAll("line.link").style("stroke-opacity", function (link) {
    const sourceId = idOf(link.source);
    const targetId = idOf(link.target);
    const baseOpacity = 0.12 + similarity(link.distance) * 0.58;
    if (!activeIds) {
      return baseOpacity;
    }
    // When hovering, only show links incident to the hovered node.
    if (hoveredNodeId !== null) {
      const incident = sourceId === hoveredNodeId || targetId === hoveredNodeId;
      return incident ? Math.max(baseOpacity, 0.6) : 0.04;
    }
    // Search mode: show links between two matched nodes.
    const bothMatch = activeIds.has(sourceId) && activeIds.has(targetId);
    return bothMatch ? baseOpacity : 0.04;
  });
}

// ---------------------------------------------------------------------------
// Selection + detail panel
// ---------------------------------------------------------------------------
function selectNode(nodeId, recenter) {
  selectedNodeId = nodeId;
  refreshSelectionClass();
  openPanel(nodeId);
  if (recenter) {
    centerOnNode(nodeId);
  }
}

function refreshSelectionClass() {
  nodeGroup.selectAll("g.node").classed("is-selected", function (node) {
    return node.id === selectedNodeId;
  });
}

function clearSelection() {
  selectedNodeId = null;
  refreshSelectionClass();
  hide("#panel");
}

function centerOnNode(nodeId) {
  const node = currentNodes.find(function (candidate) {
    return candidate.id === nodeId;
  });
  if (!node || !Number.isFinite(node.x)) {
    return;
  }
  const size = viewportSize();
  const current = d3.zoomTransform(svg.node());
  const scale = Math.max(current.k, 1.1);
  const transform = d3.zoomIdentity
    .translate(size.width / 2, size.height / 2)
    .scale(scale)
    .translate(-node.x, -node.y);
  svg.transition().duration(600).call(zoomBehavior.transform, transform);
}

function openPanel(nodeId) {
  const node = allNodes.find(function (candidate) {
    return candidate.id === nodeId;
  });
  if (!node) {
    return;
  }
  const sourceElement = document.getElementById("panel-source");
  sourceElement.textContent = node.source || "—";
  // If there's an openable path/URL, make the source clickable — it asks the native
  // app (via the WKWebView message bridge) to open the file or page.
  if (node.open) {
    sourceElement.classList.add("clickable");
    sourceElement.title = "Open " + node.open;
    sourceElement.onclick = function () {
      openSource(node.open);
    };
  } else {
    sourceElement.classList.remove("clickable");
    sourceElement.onclick = null;
    sourceElement.removeAttribute("title");
  }

  document.getElementById("panel-text").textContent = node.text || node.label || "";

  // Reset the "Ask about this" result each time the panel opens.
  const askResult = document.getElementById("ask-result");
  askResult.classList.add("hidden");
  askResult.innerHTML = "";
  const askButton = document.getElementById("ask-btn");
  askButton.disabled = false;
  askButton.textContent = "Ask about this";
  askButton.onclick = function () {
    askAboutNode(node);
  };

  renderNeighbors(nodeId);
  show("#panel");
}

function renderNeighbors(nodeId) {
  const container = document.getElementById("panel-neighbors");
  container.innerHTML = "";

  const neighbors = neighborMap.get(nodeId);
  if (!neighbors || neighbors.size === 0) {
    const empty = document.createElement("div");
    empty.className = "panel-neighbors-title";
    empty.textContent = "No connected notes in this view";
    container.appendChild(empty);
    return;
  }

  const title = document.createElement("div");
  title.className = "panel-neighbors-title";
  title.textContent = "Connected notes (" + neighbors.size + ")";
  container.appendChild(title);

  // Order neighbors by similarity (strongest link first).
  const rows = [];
  neighbors.forEach(function (neighborId) {
    const link = currentLinks.find(function (candidate) {
      const sourceId = idOf(candidate.source);
      const targetId = idOf(candidate.target);
      return (
        (sourceId === nodeId && targetId === neighborId) ||
        (targetId === nodeId && sourceId === neighborId)
      );
    });
    const neighborNode = allNodes.find(function (candidate) {
      return candidate.id === neighborId;
    });
    if (neighborNode) {
      rows.push({ node: neighborNode, similarity: link ? similarity(link.distance) : 0 });
    }
  });
  rows.sort(function (a, b) {
    return b.similarity - a.similarity;
  });

  rows.forEach(function (row) {
    const element = document.createElement("div");
    element.className = "neighbor";

    const swatch = document.createElement("span");
    swatch.className = "neighbor-swatch";
    swatch.style.background = colorForNode(row.node);

    const label = document.createElement("span");
    label.className = "neighbor-label";
    label.textContent = row.node.label || shorten(row.node.text || "", 40);

    const sim = document.createElement("span");
    sim.className = "neighbor-sim";
    sim.textContent = row.similarity.toFixed(2);

    element.appendChild(swatch);
    element.appendChild(label);
    element.appendChild(sim);
    element.addEventListener("click", function () {
      selectNode(row.node.id, true);
    });
    container.appendChild(element);
  });
}

// ---------------------------------------------------------------------------
// "Ask about this" -> POST /query, with graceful clipboard fallback
// ---------------------------------------------------------------------------
async function askAboutNode(node) {
  const askButton = document.getElementById("ask-btn");
  const askResult = document.getElementById("ask-result");
  askButton.disabled = true;
  askButton.textContent = "Asking…";

  const question = "What is this about, and how does it connect to my other notes?\n\n" + node.text;

  try {
    const response = await fetch(QUERY_URL, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ text: question }),
    });
    if (!response.ok) {
      throw new Error("HTTP " + response.status);
    }
    const data = await response.json();
    renderAskResult(data);
    askButton.disabled = false;
    askButton.textContent = "Ask about this";
  } catch (error) {
    // /query isn't available (or failed) — fall back to the clipboard.
    await fallbackCopyQuestion(question);
    askButton.disabled = false;
    askButton.textContent = "Ask about this";
  }
}

function renderAskResult(data) {
  const askResult = document.getElementById("ask-result");
  askResult.innerHTML = "";

  const answer = document.createElement("div");
  answer.className = "ask-answer";
  answer.textContent = data && data.answer ? data.answer : "(no answer returned)";
  askResult.appendChild(answer);

  const sources = data && Array.isArray(data.sources) ? data.sources : [];
  if (sources.length > 0) {
    const sourcesTitle = document.createElement("div");
    sourcesTitle.className = "ask-sources-title";
    sourcesTitle.textContent = "Sources";
    askResult.appendChild(sourcesTitle);

    const list = document.createElement("ul");
    sources.forEach(function (source) {
      const item = document.createElement("li");
      item.textContent = source;
      list.appendChild(item);
    });
    askResult.appendChild(list);
  }

  askResult.classList.remove("hidden");
}

async function fallbackCopyQuestion(question) {
  const askResult = document.getElementById("ask-result");
  askResult.innerHTML = "";

  let copied = false;
  try {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      await navigator.clipboard.writeText(question);
      copied = true;
    }
  } catch (error) {
    copied = false;
  }

  const message = document.createElement("div");
  if (copied) {
    message.innerHTML =
      "<span class=\"ask-note\">Copied ✓</span> Couldn't reach the daemon to answer — is it running? The question was copied to your clipboard.";
  } else {
    message.innerHTML =
      "<span class=\"ask-note\">Note</span> Couldn't reach the daemon and the clipboard couldn't be used. Here's the prefilled question:";
    const pre = document.createElement("div");
    pre.className = "ask-answer";
    pre.style.marginTop = "8px";
    pre.textContent = question;
    message.appendChild(pre);
  }
  askResult.appendChild(message);
  askResult.classList.remove("hidden");
}

// ---------------------------------------------------------------------------
// Controls: search, panel close
// ---------------------------------------------------------------------------
function wireControls() {
  const searchInput = document.getElementById("search");
  searchInput.addEventListener("input", function () {
    applySearch(searchInput.value);
  });

  document.getElementById("panel-close").addEventListener("click", function () {
    clearSelection();
  });

  document.addEventListener("keydown", function (event) {
    if (event.key === "Escape") {
      clearSelection();
    }
  });
}

function applySearch(rawQuery) {
  const query = rawQuery.trim().toLowerCase();
  if (query === "") {
    searchMatchIds = null;
    refreshDimming();
    return;
  }
  searchMatchIds = new Set();
  currentNodes.forEach(function (node) {
    const haystack = (
      (node.text || "") +
      " " +
      (node.source || "") +
      " " +
      (node.label || "")
    ).toLowerCase();
    if (haystack.indexOf(query) !== -1) {
      searchMatchIds.add(node.id);
    }
  });
  refreshDimming();
}

// ---------------------------------------------------------------------------
// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------
function similarity(distance) {
  // Contract: smaller distance = more similar. Clamp to a clean 0..1 strength.
  const value = 1 - (Number.isFinite(distance) ? distance : 1);
  return Math.max(0, Math.min(1, value));
}

function idOf(endpoint) {
  // forceLink turns source/target into node objects after the first tick.
  return endpoint && typeof endpoint === "object" ? endpoint.id : endpoint;
}

function linkKey(link) {
  return idOf(link.source) + "→" + idOf(link.target);
}

function shorten(text, maxLength) {
  if (text.length <= maxLength) {
    return text;
  }
  return text.slice(0, maxLength - 1).trimEnd() + "…";
}

// Opens a file/URL. Inside the app's WKWebView it hands off to the native side; in a
// plain browser (standalone dev) it falls back to window.open for web URLs.
function openSource(pathOrURL) {
  if (window.webkit && window.webkit.messageHandlers && window.webkit.messageHandlers.openSource) {
    window.webkit.messageHandlers.openSource.postMessage(pathOrURL);
  } else if (/^https?:\/\//.test(pathOrURL)) {
    window.open(pathOrURL, "_blank");
  }
}

function showBanner(message) {
  const banner = document.getElementById("banner");
  banner.textContent = message;
  banner.classList.remove("hidden");
}

function show(selector) {
  document.querySelector(selector).classList.remove("hidden");
}

function hide(selector) {
  document.querySelector(selector).classList.add("hidden");
}
