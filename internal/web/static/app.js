// Pulsebox dashboard — structured hierarchy diagram (per design handoff).
// A deterministic tree of card-nodes drawn on canvas: from the Cluster root,
// Namespaces → Workloads → Pods (and Services/PVC/PV) branch RIGHT, while Nodes
// branch LEFT and are collapsed by default (toggle on the Cluster card).
// Related links (runs-on/mounts/routes) light up on hover/select.
// Same /api/topology + /api/stream contract; dependency-free, fully embedded.
"use strict";

(() => {
  const COLORS = { healthy: "#3fb950", degraded: "#d29922", unhealthy: "#f85149", unknown: "#6e7681" };
  const GPU = "#a371f7", ACCENT = "#58a6ff";
  const WORKLOADS = new Set(["Deployment", "StatefulSet", "DaemonSet"]);
  const SIZE = {
    Cluster: [156, 52], Namespace: [168, 44], Node: [162, 44],
    Deployment: [170, 40], StatefulSet: [170, 40], DaemonSet: [170, 40],
    Service: [152, 32], PersistentVolumeClaim: [160, 32], PersistentVolume: [150, 32], Pod: [152, 32],
  };
  const sizeOf = (k) => SIZE[k] || [150, 32];
  const kindRank = (k) => ({ Deployment: 0, StatefulSet: 0, DaemonSet: 0, Pod: 1, Service: 2, PersistentVolumeClaim: 3, Namespace: 0, Node: 1 }[k] ?? 5);
  const COLGAP = 214, VGAP = 12;

  // ---- state -------------------------------------------------------------
  const nodes = new Map(), edges = new Map();
  let pos = new Map();
  let layoutDirty = true, needFit = true;
  let searchTerm = "";
  const nsFilter = new Set(); // selected namespace names; empty = show all
  let selectedId = null, hoverId = null;
  let nodesExpanded = false;
  let scale = 1, panX = 0, panY = 0;
  let everLoaded = false, lastError = false;
  let detailExtra = null; // last fetched /api/resource payload for the selected node

  const canvas = document.getElementById("graph");
  const ctx = canvas.getContext("2d");
  let dpr = 1, W = 0, H = 0;
  function resize() {
    dpr = window.devicePixelRatio || 1;
    const r = canvas.getBoundingClientRect();
    W = r.width; H = r.height;
    canvas.width = Math.floor(W * dpr); canvas.height = Math.floor(H * dpr);
  }
  window.addEventListener("resize", resize);
  resize();

  // ---- data merge --------------------------------------------------------
  function applyMessage(msg) {
    if (msg.type === "snapshot") { nodes.clear(); edges.clear(); }
    (msg.upsertNodes || []).forEach((n) => nodes.set(n.id, n));
    (msg.upsertEdges || []).forEach((e) => edges.set(e.id, e));
    (msg.removeNodes || []).forEach((id) => { nodes.delete(id); if (id === selectedId) closeDetail(); });
    (msg.removeEdges || []).forEach((id) => edges.delete(id));
    if (nodes.size > 0) { everLoaded = true; lastError = false; }
    layoutDirty = true;
    updateSummary(); updateNamespaceFilter(); updateOverlay();
  }

  // ---- layout ------------------------------------------------------------
  function treeChildrenIndex() {
    const idx = new Map();
    for (const e of edges.values()) {
      if (e.kind !== "contains" && e.kind !== "backs") continue;
      if (!idx.has(e.source)) idx.set(e.source, []);
      idx.get(e.source).push(e.target);
    }
    return idx;
  }
  function computeVisible() {
    if (!nsFilter.size) return null; // no selection = show everything
    const contains = treeChildrenIndex();
    const keep = new Set(["Cluster"]);
    for (const n of nodes.values()) if (n.kind === "Node") keep.add(n.id);
    for (const ns of nsFilter) {
      const nsId = "Namespace/" + ns;
      if (!nodes.has(nsId)) continue;
      const stack = [nsId];
      keep.add(nsId);
      while (stack.length) {
        const id = stack.pop();
        for (const c of contains.get(id) || []) if (!keep.has(c)) { keep.add(c); stack.push(c); }
      }
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
    const childrenOf = (id) => (contains.get(id) || []).filter((c) => {
      if (!nodes.has(c) || !vis(c)) return false;
      if (nodes.get(c).kind === "Node" && !nodesExpanded) return false; // Nodes collapsed by default
      return true;
    }).sort((a, b) => {
      const na = nodes.get(a), nb = nodes.get(b);
      return kindRank(na.kind) - kindRank(nb.kind) || na.name.localeCompare(nb.name);
    });

    const hasParent = new Set();
    for (const e of edges.values()) {
      if ((e.kind === "contains" || e.kind === "backs") && nodes.has(e.target) && vis(e.source) && vis(e.target))
        hasParent.add(e.target);
    }
    const roots = [...nodes.values()].filter((n) => vis(n.id) && !hasParent.has(n.id))
      .sort((a, b) => kindRank(a.kind) - kindRank(b.kind) || a.name.localeCompare(b.name));

    let cursorY = 0;
    const seen = new Set();
    function place(id, depth, dir, cur) {
      if (seen.has(id)) return null;
      seen.add(id);
      const n = nodes.get(id);
      const [w, h] = sizeOf(n.kind);
      const kids = childrenOf(id);
      let y;
      if (!kids.length) { y = cur.y + h / 2; cur.y += h + VGAP; }
      else {
        const ys = kids.map((k) => place(k, depth + 1, dir, cur)).filter((v) => v !== null);
        y = ys.length ? (ys[0] + ys[ys.length - 1]) / 2 : (cur.y + h / 2);
      }
      // dir 1 = right of root, dir -1 = left of root (x is the card's left edge)
      const x = depth === 0 ? 0 : (dir === 1 ? depth * COLGAP : -(depth * COLGAP) - w);
      pos.set(id, { x, y, w, h, depth, dir, node: n });
      return y;
    }
    // Subtree weight = number of visible descendant leaves (pods/services/PVCs/
    // PVs/nodes). Used to balance namespaces across the two sides so the tree
    // stays roughly square instead of one runaway-tall column.
    const weightMemo = new Map();
    function weight(id) {
      if (weightMemo.has(id)) return weightMemo.get(id);
      weightMemo.set(id, 1); // cycle guard / provisional
      const kids = childrenOf(id);
      let c = kids.length ? 0 : 1;
      for (const k of kids) c += weight(k);
      weightMemo.set(id, c);
      return c;
    }
    const kindNameOrder = (a, b) =>
      kindRank(nodes.get(a).kind) - kindRank(nodes.get(b).kind) || nodes.get(a).name.localeCompare(nodes.get(b).name);

    for (const r of roots) {
      const kids = childrenOf(r.id);
      // Node children (only present when expanded) anchor to the left and seed
      // its weight; namespaces are greedily balanced onto the lighter side.
      const nodeKids = kids.filter((k) => nodes.get(k).kind === "Node");
      const nsKids = kids.filter((k) => nodes.get(k).kind !== "Node");
      const leftKids = nodeKids.slice(), rightKids = [];
      let leftW = nodeKids.reduce((s, k) => s + weight(k), 0), rightW = 0;
      for (const k of nsKids.slice().sort((a, b) => weight(b) - weight(a) || nodes.get(a).name.localeCompare(nodes.get(b).name))) {
        if (leftW <= rightW) { leftKids.push(k); leftW += weight(k); }
        else { rightKids.push(k); rightW += weight(k); }
      }
      leftKids.sort(kindNameOrder); rightKids.sort(kindNameOrder);

      const curR = { y: cursorY }, curL = { y: cursorY };
      const ysR = rightKids.map((k) => place(k, 1, 1, curR)).filter((v) => v !== null);
      const ysL = leftKids.map((k) => place(k, 1, -1, curL)).filter((v) => v !== null);
      const allY = ysR.concat(ysL);
      const [w, h] = sizeOf(r.kind);
      const y = allY.length ? (Math.min(...allY) + Math.max(...allY)) / 2 : cursorY + h / 2;
      pos.set(r.id, { x: 0, y, w, h, depth: 0, dir: 1, node: r });
      cursorY = Math.max(curR.y, curL.y) + VGAP * 2;
    }
  }
  function fitView() {
    if (pos.size === 0) return;
    let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity;
    for (const p of pos.values()) {
      minX = Math.min(minX, p.x); maxX = Math.max(maxX, p.x + p.w);
      minY = Math.min(minY, p.y - p.h / 2); maxY = Math.max(maxY, p.y + p.h / 2);
    }
    const pad = 60, gw = (maxX - minX) + pad * 2, gh = (maxY - minY) + pad * 2;
    scale = Math.min(1.1, Math.max(0.2, Math.min(W / gw, H / gh)));
    panX = (W - (maxX + minX) * scale) / 2;
    panY = (H - (maxY + minY) * scale) / 2;
  }

  // ---- render loop -------------------------------------------------------
  function frame() {
    if (layoutDirty) { relayout(); if (needFit && pos.size) { fitView(); needFit = false; } }
    draw();
    requestAnimationFrame(frame);
  }
  requestAnimationFrame(frame);

  const nodeColor = (n) => (n.kind === "Node" && n.meta && n.meta.gpu === "true") ? GPU : (COLORS[n.health] || COLORS.unknown);
  const matches = (n) => !searchTerm || (n.name && n.name.toLowerCase().includes(searchTerm));

  function draw() {
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, W, H);
    ctx.save();
    ctx.translate(panX, panY);
    ctx.scale(scale, scale);

    ctx.lineWidth = 1.4 / scale;
    for (const e of edges.values()) {
      if (e.kind !== "contains" && e.kind !== "backs") continue;
      const a = pos.get(e.source), b = pos.get(e.target);
      if (!a || !b) continue;
      if (b.dir === -1) elbow(a.x, a.y, b.x + b.w, b.y);
      else elbow(a.x + a.w, a.y, b.x, b.y);
    }
    const focus = hoverId || selectedId;
    if (focus) {
      ctx.lineWidth = 2 / scale;
      for (const e of edges.values()) {
        if (e.kind === "contains" || e.kind === "backs") continue;
        if (e.source !== focus && e.target !== focus) continue;
        const a = pos.get(e.source), b = pos.get(e.target);
        if (!a || !b) continue;
        bezier(a.x + a.w / 2, a.y, b.x + b.w / 2, b.y, "rgba(88,166,255,0.7)");
      }
    }
    for (const p of pos.values()) drawCard(p);
    ctx.restore();
  }

  function elbow(x1, y1, x2, y2) { bezier(x1, y1, x2, y2, "rgba(139,148,158,0.28)"); }
  function bezier(x1, y1, x2, y2, color) {
    const mx = (x1 + x2) / 2;
    ctx.strokeStyle = color;
    ctx.beginPath(); ctx.moveTo(x1, y1); ctx.bezierCurveTo(mx, y1, mx, y2, x2, y2); ctx.stroke();
  }

  function drawCard(p) {
    const n = p.node, x = p.x, y = p.y - p.h / 2, w = p.w, h = p.h;
    const c = nodeColor(n);
    const focused = n.id === selectedId || n.id === hoverId;
    ctx.globalAlpha = matches(n) ? 1 : 0.2;

    roundRect(x, y, w, h, 8); ctx.fillStyle = cssVar("--panel"); ctx.fill();
    ctx.save(); roundRect(x, y, w, h, 8); ctx.clip();
    ctx.fillStyle = c; ctx.fillRect(x, y, 4, h);
    if (n.kind === "Node" && n.meta && n.meta.gpu === "true") { ctx.fillStyle = withAlpha(GPU, 0.10); ctx.fillRect(x, y, w, h); }
    ctx.restore();
    ctx.lineWidth = (n.id === selectedId ? 2.4 : focused ? 1.8 : 1) / scale;
    ctx.strokeStyle = n.id === selectedId ? "#fff" : focused ? ACCENT : withAlpha(c, 0.55);
    roundRect(x, y, w, h, 8); ctx.stroke();

    const tx = x + 12;
    ctx.textBaseline = "middle"; ctx.textAlign = "left";
    if (h >= 38) {
      ctx.fillStyle = cssVar("--faint"); ctx.font = "600 10px sans-serif";
      ctx.fillText(kindShort(n).toUpperCase(), tx, y + 13);
      ctx.fillStyle = cssVar("--text"); ctx.font = "700 13px sans-serif";
      ctx.fillText(clip(n.name, w - 24), tx, y + 29);
      const r = tallRight(n);
      if (r) { ctx.fillStyle = r.color; ctx.font = "700 12px sans-serif"; ctx.textAlign = "right";
        ctx.fillText(r.text, x + w - 10, y + h / 2); ctx.textAlign = "left"; }
    } else {
      ctx.fillStyle = c; ctx.beginPath(); ctx.arc(tx + 3, y + h / 2, 4, 0, Math.PI * 2); ctx.fill();
      ctx.fillStyle = cssVar("--text"); ctx.font = "600 12.5px sans-serif";
      ctx.fillText(clip(n.name, w - 40), tx + 14, y + h / 2 + 0.5);
      const right = rightMeta(n);
      if (right) { ctx.fillStyle = right.color; ctx.font = "700 10.5px sans-serif"; ctx.textAlign = "right";
        ctx.fillText(right.text, x + w - 9, y + h / 2 + 0.5); ctx.textAlign = "left"; }
    }
    if (n.kind === "Cluster") {
      let nc = 0; for (const nn of nodes.values()) if (nn.kind === "Node") nc++;
      ctx.fillStyle = cssVar("--accent"); ctx.font = "700 10.5px sans-serif"; ctx.textAlign = "left";
      ctx.fillText((nodesExpanded ? "▾ " : "▸ ") + nc + " nodes", tx, y + h - 9);
    }
    ctx.globalAlpha = 1;
  }

  function kindShort(n) { return n.kind === "PersistentVolumeClaim" ? "PVC" : n.kind === "PersistentVolume" ? "PV" : n.kind; }
  function rightMeta(n) {
    if (n.kind === "Pod") { const r = n.meta && parseInt(n.meta.restarts || "0", 10); if (r > 0) return { text: "↻" + r, color: COLORS.degraded }; }
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
    ctx.beginPath(); ctx.moveTo(x + r, y);
    ctx.arcTo(x + w, y, x + w, y + h, r); ctx.arcTo(x + w, y + h, x, y + h, r);
    ctx.arcTo(x, y + h, x, y, r); ctx.arcTo(x, y, x + w, y, r); ctx.closePath();
  }
  function clip(s, maxW) { if (ctx.measureText(s).width <= maxW) return s; let t = s; while (t.length > 1 && ctx.measureText(t + "…").width > maxW) t = t.slice(0, -1); return t + "…"; }
  const varCache = {};
  function cssVar(name) { return varCache[name] || (varCache[name] = getComputedStyle(document.documentElement).getPropertyValue(name).trim() || "#888"); }
  function withAlpha(hex, a) { const h = hex.replace("#", ""); const n = parseInt(h.length === 3 ? h.split("").map((x) => x + x).join("") : h, 16); return `rgba(${(n >> 16) & 255},${(n >> 8) & 255},${n & 255},${a})`; }
  window.matchMedia("(prefers-color-scheme: dark)").addEventListener?.("change", () => { for (const k in varCache) delete varCache[k]; });

  // ---- interaction -------------------------------------------------------
  let panning = false, lastX = 0, lastY = 0, downX = 0, downY = 0, moved = false;
  const toWorld = (px, py) => ({ x: (px - panX) / scale, y: (py - panY) / scale });
  function nodeAt(px, py) {
    const w = toWorld(px, py); let best = null;
    for (const p of pos.values()) if (w.x >= p.x && w.x <= p.x + p.w && w.y >= p.y - p.h / 2 && w.y <= p.y + p.h / 2) best = p;
    return best;
  }
  const localXY = (e) => { const r = canvas.getBoundingClientRect(); return [e.clientX - r.left, e.clientY - r.top]; };

  canvas.addEventListener("mousedown", (e) => { const [px, py] = localXY(e); downX = px; downY = py; moved = false; panning = true; lastX = px; lastY = py; });
  window.addEventListener("mousemove", (e) => {
    const [px, py] = localXY(e);
    if (Math.abs(px - downX) + Math.abs(py - downY) > 4) moved = true;
    if (panning && moved) { panX += px - lastX; panY += py - lastY; lastX = px; lastY = py; return; }
    const hit = nodeAt(px, py);
    hoverId = hit ? hit.node.id : null;
    if (hit) showTooltip(hit.node, px, py); else hideTooltip();
    canvas.style.cursor = hit ? "pointer" : "grab";
  });
  window.addEventListener("mouseup", (e) => {
    if (panning && !moved) {
      const [px, py] = localXY(e); const hit = nodeAt(px, py);
      if (hit && hit.node.kind === "Cluster") { nodesExpanded = !nodesExpanded; layoutDirty = true; }
      else if (hit) selectNode(hit.node.id);
      else closeDetail();
    }
    panning = false;
  });
  canvas.addEventListener("wheel", (e) => {
    e.preventDefault();
    const [px, py] = localXY(e); const w = toWorld(px, py);
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
  const detailBody = document.getElementById("detailBody");
  document.getElementById("detailClose").addEventListener("click", closeDetail);
  detailBody.addEventListener("click", (e) => {
    const b = e.target.closest("[data-goto]");
    if (b) selectNode(b.getAttribute("data-goto"));
  });
  function closeDetail() { detail.classList.add("hidden"); selectedId = null; detailExtra = null; }

  function selectNode(id) {
    const n = nodes.get(id); if (!n) return;
    selectedId = id; detailExtra = null;
    renderDetail(n);
    detail.classList.remove("hidden");
    hideTooltip();
    if (n.kind !== "Cluster") fetchResource(n);
  }

  function breadcrumbFor(n) {
    const parent = new Map();
    for (const e of edges.values()) if (e.kind === "contains" || e.kind === "backs") parent.set(e.target, e.source);
    const chain = []; let cur = n.id; const guard = new Set();
    while (cur && !guard.has(cur)) { guard.add(cur); const node = nodes.get(cur); if (node) chain.unshift(kindShort(node) + " " + node.name); cur = parent.get(cur); }
    return chain.join(" / ");
  }

  function relatedFor(n) {
    const out = [];
    for (const e of edges.values()) {
      if (e.kind === "contains" || e.kind === "backs") continue;
      let oid = null; if (e.source === n.id) oid = e.target; else if (e.target === n.id) oid = e.source;
      if (!oid) continue;
      const other = nodes.get(oid); if (!other) continue;
      const label = e.kind === "runs-on" ? "Runs on " + kindShort(other) + " " + other.name
        : e.kind === "mounts" ? "Mounts " + kindShort(other) + " " + other.name
        : e.kind === "routes" ? "Routes to " + kindShort(other) + " " + other.name
        : kindShort(other) + " " + other.name;
      out.push({ id: oid, label });
    }
    for (const e of edges.values()) {
      if ((e.kind === "contains" || e.kind === "backs") && e.target === n.id) {
        const parent = nodes.get(e.source);
        if (parent) out.unshift({ id: parent.id, label: "Parent: " + kindShort(parent) + " " + parent.name });
      }
    }
    return out;
  }

  function containersFrom(extra) {
    const r = extra && extra.resource;
    if (!r || !r.spec || !Array.isArray(r.spec.containers)) return [];
    const st = {}; ((r.status && r.status.containerStatuses) || []).forEach((cs) => { st[cs.name] = cs; });
    return r.spec.containers.map((c) => {
      const s = st[c.name] || {};
      return { name: c.name, image: c.image, ready: !!s.ready, restarts: s.restartCount || 0 };
    });
  }

  function renderDetail(n, extra) {
    if (extra !== undefined) detailExtra = extra;
    const c = nodeColor(n);
    detail.style.setProperty("--c", c);
    document.getElementById("detailAccent").style.setProperty("--c", c);

    let html = `<div class="breadcrumb">${esc(breadcrumbFor(n))}</div>`;
    html += `<h2>${esc(n.name)}</h2>`;
    html += `<div style="margin:0 4px 4px"><span class="badge" style="background:${c};color:#0b0e13">${esc(n.health)}</span> <span style="color:var(--muted)"> score ${(n.score || 0).toFixed(2)}</span></div>`;

    html += `<div class="kv">` + kv("Kind", n.kind) + (n.namespace ? kv("Namespace", n.namespace) : "");
    if (n.meta) for (const [k, v] of Object.entries(n.meta)) html += kv(k, v);
    html += `</div>`;

    if (n.reasons && n.reasons.length)
      html += sectionTitle("Reasons") + `<ul class="reasons">` + n.reasons.map((r) => `<li>${esc(r)}</li>`).join("") + `</ul>`;

    const ctrs = containersFrom(detailExtra);
    if (ctrs.length) {
      html += sectionTitle("Containers");
      for (const cc of ctrs) {
        const col = cc.ready ? COLORS.healthy : COLORS.degraded;
        html += `<div class="ctr"><div class="ctr-head"><b>${esc(cc.name)}</b><span style="font-size:11px;color:${col}">${cc.ready ? "ready" : "starting"}</span></div>` +
          `<div class="ctr-img">${esc(cc.image)}</div><div class="ctr-re">restarts ${esc(cc.restarts)}</div></div>`;
      }
    }

    const related = relatedFor(n);
    if (related.length) {
      html += sectionTitle("Related") + `<div class="related">` +
        related.map((r) => `<button class="rel-btn" data-goto="${esc(r.id)}">${esc(r.label)} →</button>`).join("") + `</div>`;
    }

    html += sectionTitle("Recent warning events");
    const evs = detailExtra && detailExtra.warningEvents;
    if (evs && evs.length) html += `<ul class="reasons events">` + evs.map((e) => `<li>${esc(e)}</li>`).join("") + `</ul>`;
    else if (detailExtra) html += `<div class="muted">none</div>`;
    else html += `<div class="muted">loading…</div>`;

    detailBody.innerHTML = html;
  }

  async function fetchResource(n) {
    const ns = n.namespace || "-";
    try {
      const res = await fetch(`api/resource/${encodeURIComponent(n.kind)}/${encodeURIComponent(ns)}/${encodeURIComponent(n.name)}`);
      const data = res.ok ? await res.json() : { warningEvents: [] };
      if (selectedId === n.id) renderDetail(nodes.get(n.id) || n, data);
    } catch (_) { if (selectedId === n.id) renderDetail(nodes.get(n.id) || n, { warningEvents: [] }); }
  }

  const kv = (k, v) => `<div class="k">${esc(k)}</div><div class="v">${esc(String(v))}</div>`;
  const sectionTitle = (t) => `<div class="section-title">${esc(t)}</div>`;
  function esc(s) { return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c])); }

  // ---- overlay (loading / empty / error) ---------------------------------
  const overlay = document.getElementById("overlay");
  const overlayText = document.getElementById("overlayText");
  function updateOverlay() {
    if (nodes.size > 0) { overlay.classList.add("hidden"); return; }
    overlay.classList.remove("hidden");
    if (lastError) { overlay.classList.add("error"); overlayText.textContent = "Couldn't reach the Pulsebox API — retrying…"; }
    else { overlay.classList.remove("error"); overlayText.textContent = everLoaded ? "No resources to display." : "Loading cluster topology…"; }
  }

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
    const col = s >= 0.95 ? COLORS.healthy : s >= 0.7 ? COLORS.degraded : COLORS.unhealthy;
    elm.innerHTML =
      `<span class="stat"><span class="score-pill" style="color:${col}">cluster ${Math.round(s * 100)}%</span></span>` +
      stat("healthy", counts.healthy, COLORS.healthy) + stat("degraded", counts.degraded, COLORS.degraded) +
      stat("unhealthy", counts.unhealthy, COLORS.unhealthy) +
      `<span class="stat small">${nsCount} ns · ${podCount} pods · ${nodeCount} nodes · <span style="color:${GPU}">${gpuCount} GPU</span></span>`;
  }
  const stat = (l, v, c) => `<span class="stat"><b style="color:${c}">${v}</b> ${l}</span>`;

  const nsBtn = document.getElementById("nsBtn");
  const nsMenu = document.getElementById("nsMenu");
  const nsList = document.getElementById("nsList");

  function nsButtonLabel() {
    const lbl = document.getElementById("nsBtnLabel");
    lbl.textContent = nsFilter.size === 0 ? "All namespaces" : nsFilter.size === 1 ? [...nsFilter][0] : nsFilter.size + " namespaces";
  }

  function updateNamespaceFilter() {
    const names = [...nodes.values()].filter((n) => n.kind === "Namespace").map((n) => n.name).sort();
    for (const s of [...nsFilter]) if (!names.includes(s)) nsFilter.delete(s); // prune stale selections
    const existing = [...nsList.querySelectorAll("input")].map((i) => i.value);
    const changed = existing.length !== names.length || names.some((n, i) => existing[i] !== n);
    if (changed) {
      const nsColor = (name) => { const nn = nodes.get("Namespace/" + name); return nn ? (COLORS[nn.health] || COLORS.unknown) : COLORS.unknown; };
      nsList.innerHTML = names.map((n) =>
        `<label data-name="${esc(n)}"><input type="checkbox" value="${esc(n)}"${nsFilter.has(n) ? " checked" : ""}>` +
        `<span class="ns-dot" style="background:${nsColor(n)}"></span>${esc(n)}</label>`).join("");
    } else {
      nsList.querySelectorAll("input").forEach((i) => { i.checked = nsFilter.has(i.value); });
    }
    nsButtonLabel();
  }

  nsBtn.addEventListener("click", () => {
    nsMenu.classList.toggle("hidden");
    nsBtn.setAttribute("aria-expanded", String(!nsMenu.classList.contains("hidden")));
  });
  document.addEventListener("click", (e) => {
    if (!document.getElementById("nsMulti").contains(e.target)) {
      nsMenu.classList.add("hidden"); nsBtn.setAttribute("aria-expanded", "false");
    }
  });
  nsList.addEventListener("change", (e) => {
    const i = e.target.closest("input"); if (!i) return;
    if (i.checked) nsFilter.add(i.value); else nsFilter.delete(i.value);
    nsButtonLabel(); layoutDirty = true; needFit = true;
  });
  document.getElementById("nsClear").addEventListener("click", () => {
    nsFilter.clear();
    nsList.querySelectorAll("input").forEach((i) => { i.checked = false; });
    nsButtonLabel(); layoutDirty = true; needFit = true;
  });
  document.getElementById("nsMenuSearch").addEventListener("input", (e) => {
    const q = e.target.value.trim().toLowerCase();
    nsList.querySelectorAll("label").forEach((l) => l.classList.toggle("hiddenByFilter", !!q && !l.dataset.name.toLowerCase().includes(q)));
  });
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
      try { const res = await fetch("api/topology", { cache: "no-store" }); if (!res.ok) throw 0; const g = await res.json(); applyMessage({ type: "snapshot", upsertNodes: g.nodes, upsertEdges: g.edges }); }
      catch (_) { lastError = true; updateOverlay(); }
    };
    poll(); pollTimer = setInterval(poll, 5000);
  }
  function stopPolling() { if (pollTimer) { clearInterval(pollTimer); pollTimer = null; } }

  updateOverlay();
  fetch("api/topology", { cache: "no-store" })
    .then((r) => r.ok ? r.json() : Promise.reject())
    .then((g) => applyMessage({ type: "snapshot", upsertNodes: g.nodes, upsertEdges: g.edges }))
    .catch(() => { lastError = true; updateOverlay(); })
    .finally(connect);
})();
