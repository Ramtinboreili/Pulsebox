// Pulsebox dashboard — a dependency-free force-directed cluster topology map.
// No frameworks, no CDN, no build step: this file is embedded in the binary and
// talks only to the same-origin Pulsebox API.
"use strict";

(() => {
  const HEALTH_COLORS = {
    healthy: "#3fb950",
    degraded: "#d29922",
    unhealthy: "#f85149",
    unknown: "#6e7681",
  };
  const GPU_COLOR = "#a371f7";

  // Per-kind visual sizing and the ring radius the layout pulls each kind to,
  // giving a loose cluster→namespace→workload→pod hierarchy.
  const KIND = {
    Cluster:               { r: 26, ring: 0 },
    Node:                  { r: 14, ring: 170 },
    Namespace:             { r: 16, ring: 250 },
    Deployment:            { r: 10, ring: 360 },
    StatefulSet:           { r: 10, ring: 360 },
    DaemonSet:             { r: 10, ring: 360 },
    Service:               { r: 8,  ring: 410 },
    PersistentVolumeClaim: { r: 7,  ring: 450 },
    PersistentVolume:      { r: 8,  ring: 520 },
    Pod:                   { r: 6,  ring: 480 },
  };
  const kindMeta = (k) => KIND[k] || { r: 7, ring: 400 };

  // ---- state -------------------------------------------------------------
  const nodes = new Map(); // id -> node (with sim fields x,y,vx,vy)
  const edges = new Map(); // id -> edge
  let visibleNodes = [];
  let visibleEdges = [];
  let adjacency = new Map();

  let nsFilter = "";
  let searchTerm = "";
  let selectedId = null;
  let hoverId = null;
  let alpha = 1;

  // camera
  let scale = 1, panX = 0, panY = 0;

  const canvas = document.getElementById("graph");
  const ctx = canvas.getContext("2d");
  let dpr = window.devicePixelRatio || 1;
  let W = 0, H = 0;

  function resize() {
    dpr = window.devicePixelRatio || 1;
    W = canvas.clientWidth;
    H = canvas.clientHeight;
    canvas.width = Math.floor(W * dpr);
    canvas.height = Math.floor(H * dpr);
  }
  window.addEventListener("resize", () => { resize(); });
  resize();
  panX = W / 2; panY = H / 2;

  // ---- data merge --------------------------------------------------------
  function upsertNode(n) {
    const existing = nodes.get(n.id);
    if (existing) {
      // preserve simulation position
      Object.assign(existing, n);
    } else {
      const ring = kindMeta(n.kind).ring;
      const a = Math.random() * Math.PI * 2;
      n.x = Math.cos(a) * (ring + (Math.random() - 0.5) * 40);
      n.y = Math.sin(a) * (ring + (Math.random() - 0.5) * 40);
      n.vx = 0; n.vy = 0;
      nodes.set(n.id, n);
    }
  }

  function applyMessage(msg) {
    if (msg.type === "snapshot") {
      nodes.clear(); edges.clear();
    }
    (msg.upsertNodes || []).forEach(upsertNode);
    (msg.upsertEdges || []).forEach((e) => edges.set(e.id, e));
    (msg.removeNodes || []).forEach((id) => { nodes.delete(id); if (selectedId === id) closeDetail(); });
    (msg.removeEdges || []).forEach((id) => edges.delete(id));
    rebuildDerived();
    alpha = Math.max(alpha, 0.6); // reheat layout on change
  }

  function rebuildDerived() {
    // adjacency
    adjacency = new Map();
    for (const e of edges.values()) {
      if (!adjacency.has(e.source)) adjacency.set(e.source, new Set());
      if (!adjacency.has(e.target)) adjacency.set(e.target, new Set());
      adjacency.get(e.source).add(e.target);
      adjacency.get(e.target).add(e.source);
    }
    computeVisible();
    updateNamespaceFilter();
    updateSummary();
  }

  function computeVisible() {
    if (!nsFilter) {
      visibleNodes = [...nodes.values()];
    } else {
      const keep = new Set();
      for (const n of nodes.values()) {
        if (n.namespace === nsFilter || (n.kind === "Namespace" && n.name === nsFilter) || n.kind === "Cluster") {
          keep.add(n.id);
        }
      }
      // pull in one-hop neighbours (nodes, PVs) of kept ns-scoped nodes
      for (const id of [...keep]) {
        const nb = adjacency.get(id);
        if (nb) nb.forEach((x) => keep.add(x));
      }
      visibleNodes = [...keep].map((id) => nodes.get(id)).filter(Boolean);
    }
    const vis = new Set(visibleNodes.map((n) => n.id));
    visibleEdges = [...edges.values()].filter((e) => vis.has(e.source) && vis.has(e.target));
  }

  // ---- force simulation --------------------------------------------------
  function tick() {
    const n = visibleNodes.length;
    if (n === 0) return;
    const REPULSION = 1400;
    const SPRING = 0.02;
    const SPRING_LEN = 70;
    const RING_PULL = 0.008;
    const CENTER_PULL = 0.002;

    // repulsion (O(n^2); acceptable for typical namespace-scoped views)
    for (let i = 0; i < n; i++) {
      const a = visibleNodes[i];
      for (let j = i + 1; j < n; j++) {
        const b = visibleNodes[j];
        let dx = a.x - b.x, dy = a.y - b.y;
        let d2 = dx * dx + dy * dy;
        if (d2 < 0.01) { d2 = 0.01; dx = Math.random(); dy = Math.random(); }
        const d = Math.sqrt(d2);
        const f = (REPULSION / d2) * alpha;
        const fx = (dx / d) * f, fy = (dy / d) * f;
        a.vx += fx; a.vy += fy;
        b.vx -= fx; b.vy -= fy;
      }
    }
    // springs along edges
    for (const e of visibleEdges) {
      const a = nodes.get(e.source), b = nodes.get(e.target);
      if (!a || !b) continue;
      const dx = b.x - a.x, dy = b.y - a.y;
      const d = Math.sqrt(dx * dx + dy * dy) || 0.01;
      const f = SPRING * (d - SPRING_LEN) * alpha;
      const fx = (dx / d) * f, fy = (dy / d) * f;
      a.vx += fx; a.vy += fy;
      b.vx -= fx; b.vy -= fy;
    }
    // ring + center gravity for hierarchy
    for (const a of visibleNodes) {
      const ring = kindMeta(a.kind).ring;
      const d = Math.sqrt(a.x * a.x + a.y * a.y) || 0.01;
      const targetF = (ring - d) * RING_PULL * alpha;
      a.vx += (a.x / d) * targetF;
      a.vy += (a.y / d) * targetF;
      a.vx -= a.x * CENTER_PULL * alpha;
      a.vy -= a.y * CENTER_PULL * alpha;
    }
    // integrate
    for (const a of visibleNodes) {
      if (a === draggingNode) continue;
      a.x += a.vx; a.y += a.vy;
      a.vx *= 0.85; a.vy *= 0.85;
    }
    alpha *= 0.985;
    if (alpha < 0.02) alpha = 0.02;
  }

  // ---- rendering ---------------------------------------------------------
  function nodeColor(n) {
    if (n.kind === "Node" && n.meta && n.meta.gpu === "true") return GPU_COLOR;
    return HEALTH_COLORS[n.health] || HEALTH_COLORS.unknown;
  }

  function draw() {
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, W, H);
    ctx.save();
    ctx.translate(panX, panY);
    ctx.scale(scale, scale);

    const dim = searchTerm.length > 0;
    const matches = (n) => !dim || (n.name && n.name.toLowerCase().includes(searchTerm));

    // edges
    ctx.lineWidth = 1 / scale;
    for (const e of visibleEdges) {
      const a = nodes.get(e.source), b = nodes.get(e.target);
      if (!a || !b) continue;
      const hot = e.source === hoverId || e.target === hoverId || e.source === selectedId || e.target === selectedId;
      ctx.strokeStyle = hot ? "rgba(88,166,255,0.7)" : "rgba(139,148,158,0.18)";
      ctx.beginPath();
      ctx.moveTo(a.x, a.y);
      ctx.lineTo(b.x, b.y);
      ctx.stroke();
    }

    // nodes
    for (const n of visibleNodes) {
      const km = kindMeta(n.kind);
      const r = km.r;
      const isGpu = n.kind === "Node" && n.meta && n.meta.gpu === "true";
      ctx.globalAlpha = matches(n) ? 1 : 0.15;
      ctx.beginPath();
      if (isGpu) {
        drawHex(n.x, n.y, r + 2);
      } else {
        ctx.arc(n.x, n.y, r, 0, Math.PI * 2);
      }
      ctx.fillStyle = nodeColor(n);
      ctx.fill();

      if (n.id === selectedId) {
        ctx.lineWidth = 3 / scale; ctx.strokeStyle = "#fff"; ctx.stroke();
      } else if (n.id === hoverId) {
        ctx.lineWidth = 2 / scale; ctx.strokeStyle = "#58a6ff"; ctx.stroke();
      } else if (isGpu) {
        ctx.lineWidth = 2 / scale; ctx.strokeStyle = "#d2b7ff"; ctx.stroke();
      } else {
        ctx.lineWidth = 1 / scale; ctx.strokeStyle = "rgba(0,0,0,0.5)"; ctx.stroke();
      }

      // labels for the larger structural nodes, or when zoomed in
      if (KIND[n.kind] && (km.r >= 10 || scale > 1.6)) {
        ctx.globalAlpha = matches(n) ? 0.9 : 0.15;
        ctx.fillStyle = "#c9d1d9";
        ctx.font = `${Math.max(9, 11 / scale)}px sans-serif`;
        ctx.textAlign = "center";
        ctx.fillText(n.name, n.x, n.y - r - 4 / scale);
      }
    }
    ctx.globalAlpha = 1;
    ctx.restore();
  }

  function drawHex(cx, cy, r) {
    for (let i = 0; i < 6; i++) {
      const a = (Math.PI / 3) * i - Math.PI / 6;
      const x = cx + r * Math.cos(a), y = cy + r * Math.sin(a);
      if (i === 0) ctx.moveTo(x, y); else ctx.lineTo(x, y);
    }
    ctx.closePath();
  }

  function frame() {
    tick();
    draw();
    requestAnimationFrame(frame);
  }
  requestAnimationFrame(frame);

  // ---- interaction -------------------------------------------------------
  let draggingNode = null;
  let panning = false;
  let lastX = 0, lastY = 0;
  let downX = 0, downY = 0, moved = false;

  function toWorld(px, py) {
    return { x: (px - panX) / scale, y: (py - panY) / scale };
  }
  function nodeAt(px, py) {
    const w = toWorld(px, py);
    let best = null, bestD = Infinity;
    for (const n of visibleNodes) {
      const r = kindMeta(n.kind).r + 3;
      const dx = n.x - w.x, dy = n.y - w.y;
      const d = dx * dx + dy * dy;
      if (d <= r * r && d < bestD) { best = n; bestD = d; }
    }
    return best;
  }

  canvas.addEventListener("mousedown", (e) => {
    const rect = canvas.getBoundingClientRect();
    const px = e.clientX - rect.left, py = e.clientY - rect.top;
    downX = px; downY = py; moved = false;
    const hit = nodeAt(px, py);
    if (hit) { draggingNode = hit; }
    else { panning = true; lastX = px; lastY = py; }
  });
  window.addEventListener("mousemove", (e) => {
    const rect = canvas.getBoundingClientRect();
    const px = e.clientX - rect.left, py = e.clientY - rect.top;
    if (Math.abs(px - downX) + Math.abs(py - downY) > 4) moved = true;
    if (draggingNode) {
      const w = toWorld(px, py);
      draggingNode.x = w.x; draggingNode.y = w.y;
      draggingNode.vx = 0; draggingNode.vy = 0;
      alpha = Math.max(alpha, 0.3);
    } else if (panning) {
      panX += px - lastX; panY += py - lastY;
      lastX = px; lastY = py;
    } else {
      const hit = nodeAt(px, py);
      hoverId = hit ? hit.id : null;
      if (hit) showTooltip(hit, e.clientX, e.clientY); else hideTooltip();
      canvas.style.cursor = hit ? "pointer" : "grab";
    }
  });
  window.addEventListener("mouseup", () => {
    if (draggingNode && !moved) selectNode(draggingNode.id);
    else if (panning && !moved) { /* background click: keep selection */ }
    draggingNode = null; panning = false;
  });
  canvas.addEventListener("wheel", (e) => {
    e.preventDefault();
    const rect = canvas.getBoundingClientRect();
    const px = e.clientX - rect.left, py = e.clientY - rect.top;
    const factor = e.deltaY < 0 ? 1.1 : 1 / 1.1;
    const w = toWorld(px, py);
    scale = Math.min(4, Math.max(0.15, scale * factor));
    // keep cursor anchored
    panX = px - w.x * scale;
    panY = py - w.y * scale;
  }, { passive: false });

  // ---- tooltip -----------------------------------------------------------
  const tooltip = document.getElementById("tooltip");
  function showTooltip(n, clientX, clientY) {
    let html = `<div class="tt-title">${esc(n.name)}</div><div class="tt-kind">${esc(n.kind)}${n.namespace ? " · " + esc(n.namespace) : ""} · <b style="color:${nodeColor(n)}">${esc(n.health)}</b> ${n.score.toFixed(2)}</div>`;
    if (n.reasons && n.reasons.length) html += `<div class="tt-reason">${esc(n.reasons.slice(0, 3).join("; "))}</div>`;
    tooltip.innerHTML = html;
    tooltip.classList.remove("hidden");
    tooltip.style.left = Math.min(clientX + 14, window.innerWidth - 330) + "px";
    tooltip.style.top = (clientY + 14) + "px";
  }
  function hideTooltip() { tooltip.classList.add("hidden"); }

  // ---- detail panel ------------------------------------------------------
  const detail = document.getElementById("detail");
  document.getElementById("detailClose").addEventListener("click", closeDetail);
  function closeDetail() { detail.classList.add("hidden"); selectedId = null; }

  function selectNode(id) {
    selectedId = id;
    const n = nodes.get(id);
    if (!n) return;
    renderDetail(n, null);
    detail.classList.remove("hidden");
    if (n.kind !== "Cluster") fetchResource(n);
  }

  function renderDetail(n, extra) {
    document.getElementById("detailTitle").textContent = n.name;
    const color = nodeColor(n);
    let html = `<span class="badge" style="background:${color};color:#0d1117">${esc(n.health)}</span> <span style="color:var(--muted)">score ${n.score.toFixed(2)}</span>`;
    html += `<div class="kv">`;
    html += kv("Kind", n.kind);
    if (n.namespace) html += kv("Namespace", n.namespace);
    if (n.meta) for (const [k, v] of Object.entries(n.meta)) html += kv(k, v);
    html += `</div>`;
    if (n.reasons && n.reasons.length) {
      html += `<div class="section-title">Reasons</div><ul class="reasons">` +
        n.reasons.map((r) => `<li>${esc(r)}</li>`).join("") + `</ul>`;
    }
    if (extra && extra.warningEvents && extra.warningEvents.length) {
      html += `<div class="section-title">Recent warning events</div><ul class="reasons events">` +
        extra.warningEvents.map((e) => `<li>${esc(e)}</li>`).join("") + `</ul>`;
    } else if (extra) {
      html += `<div class="section-title">Recent warning events</div><div style="color:var(--muted)">none</div>`;
    }
    document.getElementById("detailBody").innerHTML = html;
  }

  async function fetchResource(n) {
    const ns = n.namespace || "-";
    const kind = n.kind;
    try {
      const res = await fetch(`api/resource/${encodeURIComponent(kind)}/${encodeURIComponent(ns)}/${encodeURIComponent(n.name)}`);
      if (!res.ok) return;
      const data = await res.json();
      if (selectedId === n.id) renderDetail(nodes.get(n.id) || n, data);
    } catch (_) { /* ignore */ }
  }

  const kv = (k, v) => `<div class="k">${esc(k)}</div><div class="v">${esc(String(v))}</div>`;
  function esc(s) {
    return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  }

  // ---- summary + filters -------------------------------------------------
  function updateSummary() {
    const el = document.getElementById("summary");
    let cluster = null;
    const counts = { healthy: 0, degraded: 0, unhealthy: 0, unknown: 0 };
    let podCount = 0, nodeCount = 0, gpuCount = 0, nsCount = 0;
    for (const n of nodes.values()) {
      if (n.kind === "Cluster") cluster = n;
      if (n.kind === "Pod") { podCount++; counts[n.health] = (counts[n.health] || 0) + 1; }
      if (n.kind === "Node") { nodeCount++; if (n.meta && n.meta.gpu === "true") gpuCount++; }
      if (n.kind === "Namespace") nsCount++;
    }
    const score = cluster ? cluster.score : 0;
    const scoreColor = score >= 0.99 ? HEALTH_COLORS.healthy : score >= 0.7 ? HEALTH_COLORS.degraded : HEALTH_COLORS.unhealthy;
    el.innerHTML =
      `<span class="stat"><span class="score-pill" style="color:${scoreColor};border-color:${scoreColor}">cluster ${(score * 100).toFixed(0)}%</span></span>` +
      stat("healthy", counts.healthy, HEALTH_COLORS.healthy) +
      stat("degraded", counts.degraded, HEALTH_COLORS.degraded) +
      stat("unhealthy", counts.unhealthy, HEALTH_COLORS.unhealthy) +
      `<span class="stat small">${nsCount} ns · ${podCount} pods · ${nodeCount} nodes${gpuCount ? ` · <span style="color:${GPU_COLOR}">${gpuCount} GPU</span>` : ""}</span>`;
  }
  const stat = (label, v, color) => `<span class="stat"><b style="color:${color}">${v}</b> ${label}</span>`;

  function updateNamespaceFilter() {
    const sel = document.getElementById("nsFilter");
    const current = sel.value;
    const nsNames = [...nodes.values()].filter((n) => n.kind === "Namespace").map((n) => n.name).sort();
    // rebuild only if the set changed
    const existing = new Set([...sel.options].map((o) => o.value));
    const wanted = new Set(["", ...nsNames]);
    if (existing.size === wanted.size && [...wanted].every((x) => existing.has(x))) return;
    sel.innerHTML = `<option value="">All namespaces</option>` +
      nsNames.map((n) => `<option value="${esc(n)}">${esc(n)}</option>`).join("");
    sel.value = wanted.has(current) ? current : "";
  }

  document.getElementById("nsFilter").addEventListener("change", (e) => {
    nsFilter = e.target.value;
    computeVisible();
    alpha = 0.7;
  });
  document.getElementById("search").addEventListener("input", (e) => {
    searchTerm = e.target.value.trim().toLowerCase();
  });

  // ---- connection: WebSocket with polling fallback -----------------------
  const connEl = document.getElementById("connState");
  function setConn(cls, text) {
    connEl.className = "conn " + cls;
    connEl.textContent = text;
  }

  let ws = null;
  let wsRetry = 0;
  let pollTimer = null;

  function connect() {
    setConn("conn-connecting", "connecting…");
    try {
      ws = new WebSocket(streamURL());
    } catch (_) { scheduleReconnect(); return; }

    ws.onopen = () => { wsRetry = 0; stopPolling(); setConn("conn-live", "● live"); };
    ws.onmessage = (ev) => {
      try { applyMessage(JSON.parse(ev.data)); } catch (_) {}
    };
    ws.onclose = () => { setConn("conn-down", "reconnecting…"); scheduleReconnect(); };
    ws.onerror = () => { try { ws.close(); } catch (_) {} };
  }

  function streamURL() {
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    // resolve relative to current path so it works behind a sub-path ingress
    const base = new URL("api/stream", location.href);
    base.protocol = proto;
    return base.toString();
  }

  function scheduleReconnect() {
    ws = null;
    wsRetry++;
    startPolling(); // degrade gracefully while the socket is down
    const delay = Math.min(15000, 1000 * Math.pow(1.6, Math.min(wsRetry, 8)));
    setTimeout(connect, delay);
  }

  function startPolling() {
    if (pollTimer) return;
    setConn("conn-polling", "polling");
    const poll = async () => {
      try {
        const res = await fetch("api/topology", { cache: "no-store" });
        if (res.ok) {
          const g = await res.json();
          applyMessage({ type: "snapshot", upsertNodes: g.nodes, upsertEdges: g.edges });
        }
      } catch (_) {}
    };
    poll();
    pollTimer = setInterval(poll, 5000);
  }
  function stopPolling() {
    if (pollTimer) { clearInterval(pollTimer); pollTimer = null; }
  }

  // Initial load via REST so the graph appears even before the socket opens.
  fetch("api/topology", { cache: "no-store" })
    .then((r) => r.ok ? r.json() : null)
    .then((g) => { if (g) applyMessage({ type: "snapshot", upsertNodes: g.nodes, upsertEdges: g.edges }); })
    .catch(() => {})
    .finally(connect);
})();
