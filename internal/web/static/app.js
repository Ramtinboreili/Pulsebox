// Pulsebox dashboard — schema layout.
// Renders the topology graph as nested, deterministic cards
// (namespace → workload → pod, with nodes/services/PVCs grouped) instead of a
// force-directed graph. Consumes the same /api/topology + /api/stream contract.
"use strict";

(() => {
  const HEALTH = ["healthy", "degraded", "unhealthy", "unknown"];
  const WORKLOAD_KINDS = new Set(["Deployment", "StatefulSet", "DaemonSet"]);

  // ---- state -------------------------------------------------------------
  const nodes = new Map(); // id -> node
  const edges = new Map(); // id -> edge
  const collapsed = new Set(); // namespace names that are collapsed
  let nsFilter = "";
  let searchTerm = "";
  let selectedId = null;
  let renderQueued = false;

  const board = document.getElementById("board");

  // ---- data merge --------------------------------------------------------
  function applyMessage(msg) {
    if (msg.type === "snapshot") { nodes.clear(); edges.clear(); }
    (msg.upsertNodes || []).forEach((n) => nodes.set(n.id, n));
    (msg.upsertEdges || []).forEach((e) => edges.set(e.id, e));
    (msg.removeNodes || []).forEach((id) => { nodes.delete(id); if (id === selectedId) closeDetail(); });
    (msg.removeEdges || []).forEach((id) => edges.delete(id));
    scheduleRender();
  }

  function scheduleRender() {
    if (renderQueued) return;
    renderQueued = true;
    requestAnimationFrame(() => { renderQueued = false; render(); });
  }

  // ---- model -------------------------------------------------------------
  // children via "contains" edges: parentId -> [childId]
  function childrenIndex() {
    const idx = new Map();
    for (const e of edges.values()) {
      if (e.kind !== "contains") continue;
      if (!idx.has(e.source)) idx.set(e.source, []);
      idx.get(e.source).push(e.target);
    }
    return idx;
  }

  function buildModel() {
    const contains = childrenIndex();
    const byId = (id) => nodes.get(id);
    const kNodes = [...nodes.values()].filter((n) => n.kind === "Node")
      .sort((a, b) => a.name.localeCompare(b.name));
    const namespaces = [...nodes.values()].filter((n) => n.kind === "Namespace")
      .sort((a, b) => a.name.localeCompare(b.name));

    const nsModels = namespaces.map((ns) => {
      const childIds = contains.get(ns.id) || [];
      const workloads = [], loosePods = [], services = [], pvcs = [];
      for (const cid of childIds) {
        const c = byId(cid);
        if (!c) continue;
        if (WORKLOAD_KINDS.has(c.kind)) {
          const podIds = contains.get(c.id) || [];
          workloads.push({ node: c, pods: podIds.map(byId).filter(Boolean).sort(byName) });
        } else if (c.kind === "Pod") loosePods.push(c);
        else if (c.kind === "Service") services.push(c);
        else if (c.kind === "PersistentVolumeClaim") pvcs.push(c);
      }
      workloads.sort((a, b) => byName(a.node, b.node));
      loosePods.sort(byName); services.sort(byName); pvcs.sort(byName);
      const allPods = [...loosePods, ...workloads.flatMap((w) => w.pods)];
      const issues = allPods.filter((p) => p.health === "unhealthy" || p.health === "degraded").length;
      return { node: ns, workloads, loosePods, services, pvcs, podCount: allPods.length, issues };
    });

    return { nodes: kNodes, namespaces: nsModels };
  }
  const byName = (a, b) => a.name.localeCompare(b.name);

  // ---- render ------------------------------------------------------------
  function render() {
    const scrollY = window.scrollY;
    const model = buildModel();
    updateSummary();
    updateNamespaceFilter();

    const frag = document.createDocumentFragment();

    if (nodes.size === 0) {
      board.innerHTML = `<div class="empty">Waiting for cluster data…</div>`;
      return;
    }

    // Nodes strip
    if (model.nodes.length) {
      const strip = el("section", "nodes-strip");
      for (const n of model.nodes) strip.appendChild(nodeCard(n));
      frag.appendChild(strip);
    }

    // Namespace sections
    for (const ns of model.namespaces) frag.appendChild(namespaceSection(ns));

    board.replaceChildren(frag);
    applyFilter();
    window.scrollTo(0, scrollY);
  }

  function nodeCard(n) {
    const c = el("div", `node-card ${hc(n)}`);
    if (n.meta && n.meta.gpu === "true") c.classList.add("gpu");
    c.dataset.id = n.id; c.dataset.name = n.name;
    const head = el("div", "nc-head");
    head.appendChild(dot(n));
    head.appendChild(text("span", "", n.name));
    if (n.meta && n.meta.gpu === "true") {
      const g = el("span", "chip-gpu"); g.textContent = "GPU" + (n.meta.gpuCount ? " ×" + n.meta.gpuCount : "");
      head.appendChild(g);
    }
    c.appendChild(head);
    const sub = [];
    if (n.meta && n.meta.roles) sub.push(n.meta.roles);
    if (n.meta && n.meta.kubelet) sub.push(n.meta.kubelet);
    c.appendChild(text("div", "nc-sub", sub.join(" · ") || n.health));
    wire(c, n);
    return c;
  }

  function namespaceSection(m) {
    const ns = m.node;
    const sec = el("section", `ns ${hc(ns)}`);
    if (collapsed.has(ns.name)) sec.classList.add("collapsed");
    sec.dataset.ns = ns.name;

    const head = el("div", "ns-head");
    head.appendChild(text("span", "ns-caret", "▾"));
    head.appendChild(dot(ns));
    head.appendChild(text("span", "ns-name", ns.name));
    head.appendChild(text("span", "ns-kindtag", "namespace"));
    const meta = el("div", "ns-meta");
    const bits = [`${m.podCount} pods`];
    if (m.issues) bits.push(`${m.issues} issue${m.issues > 1 ? "s" : ""}`);
    meta.appendChild(text("span", "", bits.join(" · ")));
    meta.appendChild(text("span", "ns-score", pct(ns.score)));
    head.appendChild(meta);
    head.addEventListener("click", () => {
      if (collapsed.has(ns.name)) collapsed.delete(ns.name); else collapsed.add(ns.name);
      sec.classList.toggle("collapsed");
    });
    sec.appendChild(head);

    const body = el("div", "ns-body");
    for (const w of m.workloads) body.appendChild(workloadGroup(w));
    if (m.loosePods.length) body.appendChild(simpleGroup("Pods", "pods", m.loosePods));
    if (m.services.length) body.appendChild(simpleGroup("Services", "services", m.services, true));
    if (m.pvcs.length) body.appendChild(simpleGroup("Storage", "PVCs", m.pvcs, true));
    if (!body.children.length) body.appendChild(text("div", "empty", "no workloads"));
    sec.appendChild(body);
    return sec;
  }

  function workloadGroup(w) {
    const g = el("div", `group ${hc(w.node)}`);
    const head = el("div", "group-head");
    head.appendChild(dot(w.node));
    head.appendChild(text("span", "gh-kind", w.node.kind));
    head.appendChild(text("span", "gh-name", w.node.name));
    head.appendChild(text("span", "gh-meta", (w.node.meta && w.node.meta.ready) || ""));
    wire(head, w.node);
    head.dataset.name = w.node.name;
    g.appendChild(head);
    const bodyEl = el("div", "group-body");
    if (w.pods.length) for (const p of w.pods) bodyEl.appendChild(podTile(p));
    else bodyEl.appendChild(text("span", "empty", "no pods"));
    g.appendChild(bodyEl);
    return g;
  }

  function simpleGroup(label, kindLabel, items, satellite) {
    const g = el("div", "group");
    const head = el("div", "group-head");
    head.style.setProperty("--c", "var(--border)");
    head.appendChild(text("span", "gh-kind", kindLabel));
    head.appendChild(text("span", "gh-name", label));
    head.appendChild(text("span", "gh-meta", String(items.length)));
    g.appendChild(head);
    const bodyEl = el("div", "group-body");
    for (const it of items) bodyEl.appendChild(podTile(it, satellite));
    g.appendChild(bodyEl);
    return g;
  }

  function podTile(p, satellite) {
    const t = el("div", `tile ${hc(p)}${satellite ? " satellite" : ""}`);
    if (p.id === selectedId) t.classList.add("sel");
    t.dataset.id = p.id; t.dataset.name = p.name;
    t.appendChild(span("t-dot"));
    t.appendChild(text("span", "t-name", p.name));
    const restarts = p.meta && parseInt(p.meta.restarts || "0", 10);
    if (restarts > 0) t.appendChild(text("span", "t-badge", "↻" + restarts));
    wire(t, p);
    return t;
  }

  // ---- element helpers ---------------------------------------------------
  function el(tag, cls) { const e = document.createElement(tag); if (cls) e.className = cls; return e; }
  function span(cls) { return el("span", cls); }
  function text(tag, cls, txt) { const e = el(tag, cls); e.textContent = txt; return e; }
  function dot(n) { return el("span", `dot ${n.health}`); }
  function hc(n) { return HEALTH.includes(n.health) ? n.health : "unknown"; }
  function pct(s) { return Math.round((s || 0) * 100) + "%"; }

  function wire(elm, n) {
    elm.addEventListener("click", (e) => { e.stopPropagation(); selectNode(n.id); });
    elm.addEventListener("mouseenter", (e) => showTooltip(n, e));
    elm.addEventListener("mousemove", (e) => moveTooltip(e));
    elm.addEventListener("mouseleave", hideTooltip);
  }

  // ---- filtering ---------------------------------------------------------
  function applyFilter() {
    const q = searchTerm;
    document.querySelectorAll(".ns").forEach((sec) => {
      const nsName = sec.dataset.ns;
      const nsMatch = !nsFilter || nsName === nsFilter;
      if (!nsMatch) { sec.classList.add("hiddenByFilter"); return; }
      sec.classList.remove("hiddenByFilter");
      let anyVisible = false;
      sec.querySelectorAll(".group").forEach((g) => {
        let gVisible = false;
        g.querySelectorAll(".tile").forEach((t) => {
          const show = !q || (t.dataset.name || "").toLowerCase().includes(q);
          t.classList.toggle("hiddenByFilter", !show);
          if (show) gVisible = true;
        });
        const headName = (g.querySelector(".group-head") || {}).dataset?.name || "";
        if (q && headName.toLowerCase().includes(q)) gVisible = true;
        g.classList.toggle("hiddenByFilter", !gVisible);
        if (gVisible) anyVisible = true;
      });
      const nsNameMatch = q && nsName.toLowerCase().includes(q);
      if (nsNameMatch) anyVisible = true;
      sec.classList.toggle("hiddenByFilter", !!q && !anyVisible);
    });
    // nodes strip: filter by search only
    document.querySelectorAll(".node-card").forEach((c) => {
      const show = !q || (c.dataset.name || "").toLowerCase().includes(q);
      c.classList.toggle("hiddenByFilter", !show);
    });
  }

  // ---- tooltip -----------------------------------------------------------
  const tooltip = document.getElementById("tooltip");
  function showTooltip(n, e) {
    let html = `<div class="tt-title">${esc(n.name)}</div>`;
    html += `<div class="tt-sub">${esc(n.kind)}${n.namespace ? " · " + esc(n.namespace) : ""} · <b style="color:var(--c)">${esc(n.health)}</b> ${(n.score || 0).toFixed(2)}</div>`;
    if (n.meta && n.meta.node) html += `<div class="tt-sub">on ${esc(n.meta.node)}</div>`;
    if (n.reasons && n.reasons.length) html += `<div class="tt-reason">${esc(n.reasons.slice(0, 3).join("; "))}</div>`;
    tooltip.innerHTML = html;
    tooltip.style.setProperty("--c", `var(--${hc(n)})`);
    tooltip.classList.remove("hidden");
    moveTooltip(e);
  }
  function moveTooltip(e) {
    tooltip.style.left = Math.min(e.clientX + 14, window.innerWidth - 350) + "px";
    tooltip.style.top = Math.min(e.clientY + 16, window.innerHeight - 90) + "px";
  }
  function hideTooltip() { tooltip.classList.add("hidden"); }

  // ---- detail panel ------------------------------------------------------
  const detail = document.getElementById("detail");
  document.getElementById("detailClose").addEventListener("click", closeDetail);
  function closeDetail() {
    detail.classList.add("hidden");
    const prev = selectedId; selectedId = null;
    if (prev) document.querySelectorAll(".tile.sel").forEach((t) => t.classList.remove("sel"));
  }

  function selectNode(id) {
    const n = nodes.get(id);
    if (!n) return;
    selectedId = id;
    document.querySelectorAll(".tile.sel").forEach((t) => t.classList.remove("sel"));
    const t = document.querySelector(`.tile[data-id="${cssEsc(id)}"]`);
    if (t) t.classList.add("sel");
    renderDetail(n, null);
    detail.classList.remove("hidden");
    hideTooltip();
    if (n.kind !== "Cluster") fetchResource(n);
  }

  function renderDetail(n, extra) {
    document.getElementById("detailTitle").textContent = n.name;
    detail.style.setProperty("--c", `var(--${hc(n)})`);
    document.getElementById("detailAccent").style.setProperty("--c", `var(--${hc(n)})`);
    let html = `<span class="badge" style="background:var(--${hc(n)});color:#0b0e13">${esc(n.health)}</span> <span style="color:var(--muted)">score ${(n.score || 0).toFixed(2)}</span>`;
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
      html += `<div class="section-title">Recent warning events</div><div style="color:var(--muted);margin:0 4px">none</div>`;
    }
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
  function cssEsc(s) { return (window.CSS && CSS.escape) ? CSS.escape(s) : String(s).replace(/["\\]/g, "\\$&"); }

  // ---- summary + controls ------------------------------------------------
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
    const score = cluster ? cluster.score : 0;
    const col = score >= 0.99 ? "var(--healthy)" : score >= 0.7 ? "var(--degraded)" : "var(--unhealthy)";
    elm.innerHTML =
      `<span class="stat"><span class="score-pill" style="color:${col}">cluster ${Math.round(score * 100)}%</span></span>` +
      stat("healthy", counts.healthy, "var(--healthy)") +
      stat("degraded", counts.degraded, "var(--degraded)") +
      stat("unhealthy", counts.unhealthy, "var(--unhealthy)") +
      `<span class="stat small">${nsCount} ns · ${podCount} pods · ${nodeCount} nodes${gpuCount ? ` · <span style="color:var(--gpu)">${gpuCount} GPU</span>` : ""}</span>`;
  }
  const stat = (label, v, col) => `<span class="stat"><b style="color:${col}">${v}</b> ${label}</span>`;

  function updateNamespaceFilter() {
    const sel = document.getElementById("nsFilter");
    const current = sel.value;
    const names = [...nodes.values()].filter((n) => n.kind === "Namespace").map((n) => n.name).sort();
    const existing = new Set([...sel.options].map((o) => o.value));
    const wanted = new Set(["", ...names]);
    if (existing.size === wanted.size && [...wanted].every((x) => existing.has(x))) return;
    sel.innerHTML = `<option value="">All namespaces</option>` +
      names.map((n) => `<option value="${esc(n)}">${esc(n)}</option>`).join("");
    sel.value = wanted.has(current) ? current : "";
  }

  document.getElementById("nsFilter").addEventListener("change", (e) => { nsFilter = e.target.value; applyFilter(); });
  document.getElementById("search").addEventListener("input", (e) => { searchTerm = e.target.value.trim().toLowerCase(); applyFilter(); });
  document.getElementById("collapseAll").addEventListener("click", () => {
    const names = [...nodes.values()].filter((n) => n.kind === "Namespace").map((n) => n.name);
    const anyOpen = names.some((n) => !collapsed.has(n));
    collapsed.clear();
    if (anyOpen) names.forEach((n) => collapsed.add(n));
    render();
  });

  // ---- connection: WebSocket with polling fallback -----------------------
  const connEl = document.getElementById("connState");
  const setConn = (cls, txt) => { connEl.className = "conn " + cls; connEl.textContent = txt; };

  let ws = null, wsRetry = 0, pollTimer = null;

  function streamURL() {
    const base = new URL("api/stream", location.href);
    base.protocol = location.protocol === "https:" ? "wss:" : "ws:";
    return base.toString();
  }
  function connect() {
    setConn("conn-connecting", "connecting…");
    try { ws = new WebSocket(streamURL()); } catch (_) { return scheduleReconnect(); }
    ws.onopen = () => { wsRetry = 0; stopPolling(); setConn("conn-live", "● live"); };
    ws.onmessage = (ev) => { try { applyMessage(JSON.parse(ev.data)); } catch (_) {} };
    ws.onclose = () => { setConn("conn-down", "reconnecting…"); scheduleReconnect(); };
    ws.onerror = () => { try { ws.close(); } catch (_) {} };
  }
  function scheduleReconnect() {
    ws = null; wsRetry++;
    startPolling();
    setTimeout(connect, Math.min(15000, 1000 * Math.pow(1.6, Math.min(wsRetry, 8))));
  }
  function startPolling() {
    if (pollTimer) return;
    setConn("conn-polling", "polling");
    const poll = async () => {
      try {
        const res = await fetch("api/topology", { cache: "no-store" });
        if (res.ok) { const g = await res.json(); applyMessage({ type: "snapshot", upsertNodes: g.nodes, upsertEdges: g.edges }); }
      } catch (_) {}
    };
    poll();
    pollTimer = setInterval(poll, 5000);
  }
  function stopPolling() { if (pollTimer) { clearInterval(pollTimer); pollTimer = null; } }

  // Initial load via REST so content appears before the socket opens.
  fetch("api/topology", { cache: "no-store" })
    .then((r) => r.ok ? r.json() : null)
    .then((g) => { if (g) applyMessage({ type: "snapshot", upsertNodes: g.nodes, upsertEdges: g.edges }); })
    .catch(() => {})
    .finally(connect);
})();
