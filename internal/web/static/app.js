// Pulsebox dashboard — structured hierarchy diagram.
// A deterministic left-to-right tree (Cluster → Namespace → Workload → Pod,
// with Nodes and PV/PVC as related entities) drawn as connected card-nodes.
// Related links (pod→node, pod→PVC, service→pod) light up on hover/select.
// Same /api/topology + /api/stream contract; dependency-free, fully embedded.
"use strict";

(() => {
  const COLORS = { healthy: "#3fb950", degraded: "#d29922", unhealthy: "#f85149", unknown: "#6e7681" };
  const GPU = "#a371f7", ACCENT = "#58a6ff";
  const WORKLOADS = new Set(["Deployment", "StatefulSet", "DaemonSet"]);

  // size + column rank per kind
  const SIZE = {
    Cluster: [156, 52], Namespace: [168, 44], Node: [162, 44],
    Deployment: [170, 40], StatefulSet: [170, 40], DaemonSet: [170, 40],
    Service: [152, 32], PersistentVolumeClaim: [160, 32], PersistentVolume: [150, 32],
    Pod: [152, 32],
  };
  const sizeOf = (k) => SIZE[k] || [150, 32];
  const kindRank = (k) => ({ Deployment: 0, StatefulSet: 0, DaemonSet: 0, Pod: 1, Service: 2, PersistentVolumeClaim: 3, Namespace: 0, Node: 1 }[k] ?? 5);

  const COLGAP = 214, VGAP = 12;

  // ---- state -------------------------------------------------------------
  const nodes = new Map();
  const edges = new Map();
  let pos = new Map();          // id -> {x,y,w,h,depth,node}   (top-left x, center y)
  let layoutDirty = true;
  let needFit = true;
  let nsFilter = "", searchTerm = "";
  let selectedId = null, hoverId = null;
  let scale = 1, panX = 0, panY = 0;

  const canvas = document.getElementById("graph");
  const ctx = canvas.getContext("2d");
  let dpr = 1, W = 0, H = 0;
  function resize() {
    dpr = window.devicePixelRatio || 1;
    const r = canvas.getBoundingClientRect();
    W = r.width; H = r.height;
    canvas.width = Math.floor(W * dpr); canvas.height = Math.floor(H * dpr);
  }
  window.addEventListener("resize", () => { resize(); });
  resize();

  // ---- data merge --------------------------------------------------------
  function applyMessage(msg) {
    if (msg.type === "snapshot") { nodes.clear(); edges.clear(); }
    (msg.upsertNodes || []).forEach((n) => nodes.set(n.id, n));
    (msg.upsertEdges || []).forEach((e) => edges.set(e.id, e));
    (msg.removeNodes || []).forEach((id) => { nodes.delete(id); if (id === selectedId) closeDetail(); });
    (msg.removeEdges || []).forEach((id) => edges.delete(id));
    layoutDirty = true;
    updateSummary(); updateNamespaceFilter();
  }

  // ---- layout ------------------------------------------------------------
  function treeChildrenIndex() {
    // "contains" builds the hierarchy; "backs" hangs a PV under its PVC.
    const idx = new Map();
    for (const e of edges.values()) {
      if (e.kind !== "contains" && e.kind !== "backs") continue;
      if (!idx.has(e.source)) idx.set(e.source, []);
      idx.get(e.source).push(e.target);
    }
    return idx;
  }

  function computeVisible() {
    if (!nsFilter) return null; // null = all
    const contains = treeChildrenIndex();
    const keep = new Set(["Cluster"]);
    for (const n of nodes.values()) if (n.kind === "Node") keep.add(n.id);
    const nsId = "Namespace/" + nsFilter;
    const stack = [nsId];
    keep.add(nsId);
    while (stack.length) {
      const id = stack.pop();
      for (const c of contains.get(id) || []) if (!keep.has(c)) { keep.add(c); stack.push(c); }
    }
    return keep;
  }

  function relayout() {
    layoutDirty = false;
    pos = new Map();
    if (nodes.size === 0) return;
    const visible = computeVisible();
    const vis = (id) => !visible || visible.has(id);
    const contains = treeChildrenIndex();
    const childrenOf = (id) => (contains.get(id) || []).filter((c) => nodes.has(c) && vis(c))
      .sort((a, b) => {
        const na = nodes.get(a), nb = nodes.get(b);
        return kindRank(na.kind) - kindRank(nb.kind) || na.name.localeCompare(nb.name);
      });

    // roots = visible nodes with no visible tree-parent
    const hasParent = new Set();
    for (const e of edges.values()) {
      if ((e.kind === "contains" || e.kind === "backs") && nodes.has(e.target) && vis(e.source) && vis(e.target))
        hasParent.add(e.target);
    }
    const roots = [...nodes.values()].filter((n) => vis(n.id) && !hasParent.has(n.id))
      .sort((a, b) => kindRank(a.kind) - kindRank(b.kind) || a.name.localeCompare(b.name));

    let cursorY = 0;
    const seen = new Set();
    function place(id, depth) {
      if (seen.has(id)) return null;
      seen.add(id);
      const n = nodes.get(id);
      const [w, h] = sizeOf(n.kind);
      const kids = childrenOf(id);
      let y;
      if (!kids.length) { y = cursorY + h / 2; cursorY += h + VGAP; }
      else {
        const ys = kids.map((k) => place(k, depth + 1)).filter((v) => v !== null);
        y = ys.length ? (ys[0] + ys[ys.length - 1]) / 2 : (cursorY + h / 2);
      }
      pos.set(id, { x: depth * COLGAP, y, w, h, depth, node: n });
      return y;
    }
    for (const r of roots) { place(r.id, 0); cursorY += VGAP * 2; }
  }

  function fitView() {
    if (pos.size === 0) return;
    let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity;
    for (const p of pos.values()) {
      minX = Math.min(minX, p.x); maxX = Math.max(maxX, p.x + p.w);
      minY = Math.min(minY, p.y - p.h / 2); maxY = Math.max(maxY, p.y + p.h / 2);
    }
    const pad = 60;
    const gw = (maxX - minX) + pad * 2, gh = (maxY - minY) + pad * 2;
    scale = Math.min(1.1, Math.max(0.2, Math.min(W / gw, H / gh)));
    panX = (W - (maxX + minX) * scale) / 2;
    panY = (H - (maxY + minY) * scale) / 2;
  }

  // ---- render loop -------------------------------------------------------
  function frame() {
    if (layoutDirty) { relayout(); if (needFit) { fitView(); needFit = false; } }
    draw();
    requestAnimationFrame(frame);
  }
  requestAnimationFrame(frame);

  function nodeColor(n) {
    if (n.kind === "Node" && n.meta && n.meta.gpu === "true") return GPU;
    return COLORS[n.health] || COLORS.unknown;
  }
  const matches = (n) => !searchTerm || (n.name && n.name.toLowerCase().includes(searchTerm));

  function draw() {
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, W, H);
    ctx.save();
    ctx.translate(panX, panY);
    ctx.scale(scale, scale);

    // tree connectors
    ctx.lineWidth = 1.4 / scale;
    for (const e of edges.values()) {
      if (e.kind !== "contains" && e.kind !== "backs") continue;
      const a = pos.get(e.source), b = pos.get(e.target);
      if (!a || !b) continue;
      elbow(a.x + a.w, a.y, b.x, b.y, "rgba(139,148,158,0.28)");
    }

    // related (non-tree) links for the focused node
    const focus = hoverId || selectedId;
    if (focus) {
      ctx.lineWidth = 2 / scale;
      for (const e of edges.values()) {
        if (e.kind === "contains" || e.kind === "backs") continue;
        if (e.source !== focus && e.target !== focus) continue;
        const a = pos.get(e.source), b = pos.get(e.target);
        if (!a || !b) continue;
        curve(a.x + a.w / 2, a.y, b.x + b.w / 2, b.y, "rgba(88,166,255,0.7)");
      }
    }

    for (const p of pos.values()) drawCard(p);
    ctx.restore();
  }

  function elbow(x1, y1, x2, y2, color) {
    const mx = (x1 + x2) / 2;
    ctx.strokeStyle = color;
    ctx.beginPath();
    ctx.moveTo(x1, y1);
    ctx.bezierCurveTo(mx, y1, mx, y2, x2, y2);
    ctx.stroke();
  }
  function curve(x1, y1, x2, y2, color) {
    ctx.strokeStyle = color;
    ctx.beginPath();
    ctx.moveTo(x1, y1);
    const mx = (x1 + x2) / 2;
    ctx.bezierCurveTo(mx, y1, mx, y2, x2, y2);
    ctx.stroke();
  }

  function drawCard(p) {
    const n = p.node, x = p.x, y = p.y - p.h / 2, w = p.w, h = p.h;
    const c = nodeColor(n);
    const focused = n.id === selectedId || n.id === hoverId;
    ctx.globalAlpha = matches(n) ? 1 : 0.2;

    roundRect(x, y, w, h, 8);
    ctx.fillStyle = cssVar("--panel");
    ctx.fill();
    // health accent bar (left)
    ctx.save();
    roundRect(x, y, w, h, 8); ctx.clip();
    ctx.fillStyle = c; ctx.fillRect(x, y, 4, h);
    if (n.kind === "Node" && n.meta && n.meta.gpu === "true") { ctx.fillStyle = withAlpha(GPU, 0.10); ctx.fillRect(x, y, w, h); }
    ctx.restore();
    // border
    ctx.lineWidth = (n.id === selectedId ? 2.4 : focused ? 1.8 : 1) / scale;
    ctx.strokeStyle = n.id === selectedId ? "#fff" : focused ? ACCENT : withAlpha(c, 0.55);
    roundRect(x, y, w, h, 8); ctx.stroke();

    // text
    const tx = x + 12;
    ctx.textBaseline = "middle";
    const kindLabel = kindShort(n);
    if (h >= 38) {
      ctx.fillStyle = cssVar("--faint");
      ctx.font = `600 ${10}px sans-serif`;
      ctx.fillText(kindLabel.toUpperCase(), tx, y + 13);
      ctx.fillStyle = cssVar("--text");
      ctx.font = `700 ${13}px sans-serif`;
      ctx.fillText(clip(n.name, w - 24, ctx), tx, y + 29);
    } else {
      // one line: dot + name  (+ small right meta)
      ctx.fillStyle = c;
      ctx.beginPath(); ctx.arc(tx + 3, y + h / 2, 4, 0, Math.PI * 2); ctx.fill();
      ctx.fillStyle = cssVar("--text");
      ctx.font = `600 ${12.5}px sans-serif`;
      ctx.fillText(clip(n.name, w - 40, ctx), tx + 14, y + h / 2 + 0.5);
      const right = rightMeta(n);
      if (right) {
        ctx.fillStyle = right.color; ctx.font = `700 ${10.5}px sans-serif`;
        ctx.textAlign = "right"; ctx.fillText(right.text, x + w - 9, y + h / 2 + 0.5); ctx.textAlign = "left";
      }
    }
    // score / ready on tall cards, right side
    if (h >= 38) {
      const r = tallRight(n);
      if (r) { ctx.fillStyle = r.color; ctx.font = `700 ${12}px sans-serif`; ctx.textAlign = "right";
        ctx.fillText(r.text, x + w - 10, y + h / 2); ctx.textAlign = "left"; }
    }
    ctx.globalAlpha = 1;
  }

  function kindShort(n) {
    if (n.kind === "PersistentVolumeClaim") return "PVC";
    if (n.kind === "PersistentVolume") return "PV";
    return n.kind;
  }
  function rightMeta(n) {
    if (n.kind === "Pod") {
      const r = n.meta && parseInt(n.meta.restarts || "0", 10);
      if (r > 0) return { text: "↻" + r, color: COLORS.degraded };
    }
    if (n.kind === "Service" && n.meta) return { text: n.meta.endpoints + "ep", color: n.health === "unhealthy" ? COLORS.unhealthy : cssVar("--faint") };
    if (n.kind === "PersistentVolumeClaim" && n.meta) return { text: n.meta.phase || "", color: n.health === "unhealthy" ? COLORS.unhealthy : cssVar("--faint") };
    return null;
  }
  function tallRight(n) {
    if (n.kind === "Cluster" || n.kind === "Namespace") return { text: Math.round((n.score || 0) * 100) + "%", color: nodeColor(n) };
    if (WORKLOADS.has(n.kind) && n.meta && n.meta.ready) return { text: n.meta.ready, color: nodeColor(n) };
    if (n.kind === "Node" && n.meta && n.meta.gpu === "true") return { text: "GPU" + (n.meta.gpuCount ? "×" + n.meta.gpuCount : ""), color: GPU };
    return null;
  }

  function roundRect(x, y, w, h, r) {
    ctx.beginPath();
    ctx.moveTo(x + r, y);
    ctx.arcTo(x + w, y, x + w, y + h, r);
    ctx.arcTo(x + w, y + h, x, y + h, r);
    ctx.arcTo(x, y + h, x, y, r);
    ctx.arcTo(x, y, x + w, y, r);
    ctx.closePath();
  }
  function clip(s, maxW, c) {
    if (c.measureText(s).width <= maxW) return s;
    let t = s;
    while (t.length > 1 && c.measureText(t + "…").width > maxW) t = t.slice(0, -1);
    return t + "…";
  }

  // css var + color helpers
  const varCache = {};
  function cssVar(name) {
    if (varCache[name]) return varCache[name];
    return (varCache[name] = getComputedStyle(document.documentElement).getPropertyValue(name).trim() || "#888");
  }
  function withAlpha(hex, a) {
    const h = hex.replace("#", "");
    const n = parseInt(h.length === 3 ? h.split("").map((x) => x + x).join("") : h, 16);
    return `rgba(${(n >> 16) & 255},${(n >> 8) & 255},${n & 255},${a})`;
  }
  window.matchMedia("(prefers-color-scheme: dark)").addEventListener?.("change", () => { for (const k in varCache) delete varCache[k]; });

  // ---- interaction -------------------------------------------------------
  let panning = false, lastX = 0, lastY = 0, downX = 0, downY = 0, moved = false;
  const toWorld = (px, py) => ({ x: (px - panX) / scale, y: (py - panY) / scale });
  function nodeAt(px, py) {
    const w = toWorld(px, py);
    let best = null;
    for (const p of pos.values()) {
      if (w.x >= p.x && w.x <= p.x + p.w && w.y >= p.y - p.h / 2 && w.y <= p.y + p.h / 2) best = p;
    }
    return best;
  }
  const localXY = (e) => { const r = canvas.getBoundingClientRect(); return [e.clientX - r.left, e.clientY - r.top]; };

  canvas.addEventListener("mousedown", (e) => {
    const [px, py] = localXY(e); downX = px; downY = py; moved = false;
    panning = true; lastX = px; lastY = py;
  });
  window.addEventListener("mousemove", (e) => {
    const [px, py] = localXY(e);
    if (Math.abs(px - downX) + Math.abs(py - downY) > 4) moved = true;
    if (panning && moved) { panX += px - lastX; panY += py - lastY; lastX = px; lastY = py; return; }
    const hit = nodeAt(px, py);
    const id = hit ? hit.node.id : null;
    if (id !== hoverId) hoverId = id;
    if (hit) showTooltip(hit.node, px, py); else hideTooltip();
    canvas.style.cursor = hit ? "pointer" : "grab";
  });
  window.addEventListener("mouseup", (e) => {
    if (panning && !moved) {
      const [px, py] = localXY(e); const hit = nodeAt(px, py);
      if (hit) selectNode(hit.node.id); else closeDetail();
    }
    panning = false;
  });
  canvas.addEventListener("wheel", (e) => {
    e.preventDefault();
    const [px, py] = localXY(e);
    const w = toWorld(px, py);
    scale = Math.min(3, Math.max(0.15, scale * (e.deltaY < 0 ? 1.1 : 1 / 1.1)));
    panX = px - w.x * scale; panY = py - w.y * scale;
  }, { passive: false });
  document.getElementById("fit").addEventListener("click", () => fitView());

  // ---- tooltip -----------------------------------------------------------
  const tooltip = document.getElementById("tooltip");
  function showTooltip(n, px, py) {
    let html = `<div class="tt-title">${esc(n.name)}</div>`;
    html += `<div class="tt-sub">${esc(kindShort(n))}${n.namespace ? " · " + esc(n.namespace) : ""} · <b style="color:${nodeColor(n)}">${esc(n.health)}</b> ${(n.score || 0).toFixed(2)}</div>`;
    if (n.meta && n.meta.node) html += `<div class="tt-sub">on ${esc(n.meta.node)}</div>`;
    if (n.reasons && n.reasons.length) html += `<div class="tt-reason">${esc(n.reasons.slice(0, 3).join("; "))}</div>`;
    tooltip.innerHTML = html;
    tooltip.classList.remove("hidden");
    tooltip.style.left = Math.min(px + 14, W - 350) + "px";
    tooltip.style.top = Math.min(py + 14, H - 90) + "px";
  }
  function hideTooltip() { tooltip.classList.add("hidden"); }

  // ---- detail panel ------------------------------------------------------
  const detail = document.getElementById("detail");
  document.getElementById("detailClose").addEventListener("click", closeDetail);
  function closeDetail() { detail.classList.add("hidden"); selectedId = null; }

  function selectNode(id) {
    const n = nodes.get(id); if (!n) return;
    selectedId = id;
    renderDetail(n, null);
    detail.classList.remove("hidden");
    hideTooltip();
    if (n.kind !== "Cluster") fetchResource(n);
  }
  function renderDetail(n, extra) {
    document.getElementById("detailTitle").textContent = n.name;
    const c = nodeColor(n);
    detail.style.setProperty("--c", c);
    document.getElementById("detailAccent").style.setProperty("--c", c);
    let html = `<span class="badge" style="background:${c};color:#0b0e13">${esc(n.health)}</span> <span style="color:var(--muted)">score ${(n.score || 0).toFixed(2)}</span>`;
    html += `<div class="kv">` + kv("Kind", n.kind);
    if (n.namespace) html += kv("Namespace", n.namespace);
    if (n.meta) for (const [k, v] of Object.entries(n.meta)) html += kv(k, v);
    html += `</div>`;
    if (n.reasons && n.reasons.length)
      html += `<div class="section-title">Reasons</div><ul class="reasons">` + n.reasons.map((r) => `<li>${esc(r)}</li>`).join("") + `</ul>`;
    if (extra && extra.warningEvents && extra.warningEvents.length)
      html += `<div class="section-title">Recent warning events</div><ul class="reasons events">` + extra.warningEvents.map((e) => `<li>${esc(e)}</li>`).join("") + `</ul>`;
    else if (extra)
      html += `<div class="section-title">Recent warning events</div><div style="color:var(--muted);margin:0 4px">none</div>`;
    document.getElementById("detailBody").innerHTML = html;
  }
  async function fetchResource(n) {
    const ns = n.namespace || "-";
    try {
      const res = await fetch(`api/resource/${encodeURIComponent(n.kind)}/${encodeURIComponent(ns)}/${encodeURIComponent(n.name)}`);
      if (!res.ok) { if (selectedId === n.id) renderDetail(n, { warningEvents: [] }); return; }
      const data = await res.json();
      if (selectedId === n.id) renderDetail(nodes.get(n.id) || n, data);
    } catch (_) {}
  }
  const kv = (k, v) => `<div class="k">${esc(k)}</div><div class="v">${esc(String(v))}</div>`;
  function esc(s) { return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c])); }

  // ---- summary + filters -------------------------------------------------
  function updateSummary() {
    const elm = document.getElementById("summary");
    let cluster = null;
    const counts = { healthy: 0, degraded: 0, unhealthy: 0, unknown: 0 };
    let podCount = 0, nodeCount = 0, gpuCount = 0, nsCount = 0;
    for (const n of nodes.values()) {
      if (n.kind === "Cluster") cluster = n;
      else if (n.kind === "Pod") { podCount++; counts[n.health] = (counts[n.health] || 0) + 1; }
      else if (n.kind === "Node") { nodeCount++; if (n.meta && n.meta.gpu === "true") gpuCount++; }
      else if (n.kind === "Namespace") nsCount++;
    }
    const s = cluster ? cluster.score : 0;
    const col = s >= 0.99 ? COLORS.healthy : s >= 0.7 ? COLORS.degraded : COLORS.unhealthy;
    elm.innerHTML =
      `<span class="stat"><span class="score-pill" style="color:${col}">cluster ${Math.round(s * 100)}%</span></span>` +
      stat("healthy", counts.healthy, COLORS.healthy) + stat("degraded", counts.degraded, COLORS.degraded) +
      stat("unhealthy", counts.unhealthy, COLORS.unhealthy) +
      `<span class="stat small">${nsCount} ns · ${podCount} pods · ${nodeCount} nodes${gpuCount ? ` · <span style="color:${GPU}">${gpuCount} GPU</span>` : ""}</span>`;
  }
  const stat = (l, v, c) => `<span class="stat"><b style="color:${c}">${v}</b> ${l}</span>`;

  function updateNamespaceFilter() {
    const sel = document.getElementById("nsFilter");
    const current = sel.value;
    const names = [...nodes.values()].filter((n) => n.kind === "Namespace").map((n) => n.name).sort();
    const existing = new Set([...sel.options].map((o) => o.value));
    const wanted = new Set(["", ...names]);
    if (existing.size === wanted.size && [...wanted].every((x) => existing.has(x))) return;
    sel.innerHTML = `<option value="">All namespaces</option>` + names.map((n) => `<option value="${esc(n)}">${esc(n)}</option>`).join("");
    sel.value = wanted.has(current) ? current : "";
  }
  document.getElementById("nsFilter").addEventListener("change", (e) => { nsFilter = e.target.value; layoutDirty = true; needFit = true; });
  document.getElementById("search").addEventListener("input", (e) => { searchTerm = e.target.value.trim().toLowerCase(); });

  // ---- connection --------------------------------------------------------
  const connEl = document.getElementById("connState");
  const setConn = (cls, txt) => { connEl.className = "conn " + cls; connEl.textContent = txt; };
  let ws = null, wsRetry = 0, pollTimer = null;
  function streamURL() { const u = new URL("api/stream", location.href); u.protocol = location.protocol === "https:" ? "wss:" : "ws:"; return u.toString(); }
  function connect() {
    setConn("conn-connecting", "connecting…");
    try { ws = new WebSocket(streamURL()); } catch (_) { return scheduleReconnect(); }
    ws.onopen = () => { wsRetry = 0; stopPolling(); setConn("conn-live", "● live"); };
    ws.onmessage = (ev) => { try { applyMessage(JSON.parse(ev.data)); } catch (_) {} };
    ws.onclose = () => { setConn("conn-down", "reconnecting…"); scheduleReconnect(); };
    ws.onerror = () => { try { ws.close(); } catch (_) {} };
  }
  function scheduleReconnect() { ws = null; wsRetry++; startPolling(); setTimeout(connect, Math.min(15000, 1000 * Math.pow(1.6, Math.min(wsRetry, 8)))); }
  function startPolling() {
    if (pollTimer) return;
    setConn("conn-polling", "polling");
    const poll = async () => {
      try { const res = await fetch("api/topology", { cache: "no-store" }); if (res.ok) { const g = await res.json(); applyMessage({ type: "snapshot", upsertNodes: g.nodes, upsertEdges: g.edges }); } } catch (_) {}
    };
    poll(); pollTimer = setInterval(poll, 5000);
  }
  function stopPolling() { if (pollTimer) { clearInterval(pollTimer); pollTimer = null; } }

  fetch("api/topology", { cache: "no-store" })
    .then((r) => r.ok ? r.json() : null)
    .then((g) => { if (g) applyMessage({ type: "snapshot", upsertNodes: g.nodes, upsertEdges: g.edges }); })
    .catch(() => {})
    .finally(connect);
})();
