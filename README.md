# Pulsebox

**Pulsebox is a Kubernetes-native cluster health exporter with an embedded live
topology dashboard.**

It runs as a Deployment inside your cluster, watches cluster state through the
Kubernetes API (via `client-go` informers — no polling), and:

- exposes **Prometheus metrics** (`pulsebox_*`) for Grafana/alerting, and
- serves an **interactive topology dashboard** — namespaces → workloads → pods →
  containers, plus nodes and PV/PVC — as a force-directed graph color-coded by
  health, updating live over a WebSocket.

The dashboard is compiled into the single Go binary (`go:embed`): no CDN, no
separate frontend deployment, works in air-gapped / private-registry
environments.

> ### Why we dropped the Docker socket
> Earlier versions of Pulsebox (≤ v0.0.x) were a Docker health exporter: they
> mounted `/var/run/docker.sock` and polled container health on one host. That
> model can't see a *cluster* — it has no notion of nodes, scheduling, replicas,
> PVs/PVCs, Services/Endpoints, or namespaces, and it only reports on whatever
> containers happen to run on the single node the socket belongs to. The
> collection layer has been **completely rewritten** on `client-go`. There is no
> Docker code path anymore, no socket mount, and no `docker` import. If you used
> the old version, replace the socket-mount Deployment/compose with the
> manifests in `k8s/` or the Helm chart in `helm/pulsebox/`.

---

## Contents

- [Architecture](#architecture)
- [Health model](#health-model)
- [Deploying](#deploying)
- [RBAC](#rbac)
- [Reaching the dashboard (Traefik IngressRoute)](#reaching-the-dashboard)
- [Prometheus discovery (ServiceMonitor)](#prometheus-discovery)
- [Metrics reference](#metrics-reference)
- [Topology API + WebSocket](#topology-api--websocket)
- [Configuration](#configuration)
- [Local development](#local-development)
- [Follow-ups](#follow-ups)

---

## Architecture

```
            ┌─────────────────────── Pulsebox pod ───────────────────────┐
 Kube API   │  client-go SharedInformerFactory (watch, not poll)         │
  events ──▶│      │                                                     │
            │      ▼   in-memory caches (listers) + restart-rate tracker │
            │  internal/collector ── debounced rebuild ──▶ topology graph│
            │      │                          │                          │
            │      ▼                          ▼                          │
            │  internal/health (single source of scoring)               │
            │      │                          │                          │
            │      ▼                          ▼                          │
            │  :8037 /metrics           :8080 / (dashboard)              │
            │  (Prometheus)             /api/topology, /api/stream (WS)  │
            └────────────────────────────────────────────────────────────┘
```

Two HTTP listeners:

| Port   | Purpose                              | Exposed via        |
|--------|--------------------------------------|--------------------|
| `8037` | Prometheus `/metrics`, `/healthz`, `/readyz` | ServiceMonitor (internal) |
| `8080` | Dashboard `/`, topology `/api/*`, WebSocket `/api/stream` | Traefik IngressRoute |

Health scoring lives in one package (`internal/health`) that backs **both** the
metrics and the topology API, so the numbers on the dashboard and in Prometheus
can never disagree.

Packages:

```
cmd/pulsebox         entrypoint: config, kube client, two HTTP servers
internal/collector   informers, caches, graph build, WS diff broker, restart tracker
internal/health      status → {state, score, reasons}  (well-tested, shared)
internal/metrics     pull-based prometheus.Collector (reads caches each scrape)
internal/api         REST + WebSocket handlers
internal/web         go:embed'd dashboard (internal/web/static)
internal/topology    compact graph model + diff logic
```

---

## Health model

Every resource maps to `{state, score (0.0–1.0), reasons[]}` where state is one of
`healthy` · `degraded` · `unhealthy` · `unknown`.

| Resource | Rules |
|----------|-------|
| **Node** | `Ready` condition; `MemoryPressure`/`DiskPressure`/`PIDPressure`/`NetworkUnavailable` → degraded; cordoned surfaced; `Ready=Unknown` → unknown. Taints & roles in meta. |
| **Pod** | `Running` + all containers ready → healthy. Waiting reason in `CrashLoopBackOff\|ImagePullBackOff\|ErrImagePull\|CreateContainerConfigError` → unhealthy. `Pending`/`ContainerCreating` → degraded (**starting**, distinct from unhealthy). `Succeeded` → healthy-terminal (Jobs). `Failed` → unhealthy. A **rising restart rate** degrades/​fails an otherwise-Running pod. |
| **Deployment / StatefulSet / DaemonSet** | desired vs ready/available; partially rolled out → degraded, none available → unhealthy. |
| **PersistentVolume** | phase; `Failed` → unhealthy, `Pending` → degraded, `Released` → degraded. |
| **PersistentVolumeClaim** | `Bound` → healthy; **`Pending` beyond the grace period (default 2m) → unhealthy** (the common Longhorn RWO failure mode); `Lost` → unhealthy. |
| **Service** | a Service *with a selector* and **zero ready endpoint addresses** → unhealthy (silent outage), even if pods look fine. Headless/selector-less → unknown (not flagged). |
| **Namespace / Cluster** | aggregate roll-up: mean child score, with a present hard failure pinning the state. `unknown` children are ignored so unscored objects don't drag the score down. |

`Warning`-type Events are tailed for tooltips/detail only — never used as a
primary health source (they're noisy and ephemeral).

---

## Deploying

### 1. Build & push the image

```bash
# Pin a real tag — never :latest.
docker build -t docker.datatejarat.ir/pulsebox:v0.1.0 --build-arg VERSION=v0.1.0 .
docker push docker.datatejarat.ir/pulsebox:v0.1.0
```

The frontend is embedded at build time, so the resulting image is a single
static distroless binary — no assets to ship separately.

### 2a. Deploy with Helm (recommended)

```bash
helm upgrade --install pulsebox helm/pulsebox \
  --namespace pulsebox --create-namespace \
  --set image.repository=docker.datatejarat.ir/pulsebox \
  --set image.tag=v0.1.0 \
  --set ingressRoute.host=pulsebox.your-domain.tld \
  --set ingressRoute.tls.secretName=wildcard-datatejarat-tls
```

Key `values.yaml` knobs: `image.repository/tag`, `resources`, `ports.metrics`/
`ports.http`, `ingressRoute.host`/`tls.secretName`, `serviceMonitor.enabled`/
`releaseLabel`, and `rbac.clusterWide` (+ `rbac.watchNamespace`) to switch
between cluster-wide and namespaced watch.

### 2b. Or deploy the plain manifests

```bash
kubectl apply -f k8s/
```

Edit the image, `Host(...)`, and TLS secret name in `k8s/deployment.yaml` and
`k8s/ingressroute.yaml` first.

### 3. Verify

```bash
kubectl -n pulsebox get pods
kubectl -n pulsebox port-forward svc/pulsebox 8037:8037
curl -s localhost:8037/metrics | grep pulsebox_ | head
```

Within ~30 s of the pod running you should see live node/pod/pvc metrics.

---

## RBAC

Pulsebox is **strictly read-only**. The `ClusterRole` grants only
`get`/`list`/`watch` — no `create`/`update`/`patch`/`delete`/`deletecollection`
and no wildcards, anywhere.

| API group | Resources |
|-----------|-----------|
| `""` (core) | `nodes`, `namespaces`, `pods`, `persistentvolumes`, `persistentvolumeclaims`, `services`, `endpoints`, `events` |
| `apps` | `deployments`, `statefulsets`, `daemonsets`, `replicasets` |

A `ServiceAccount` (`pulsebox`) is bound to this role via a `ClusterRoleBinding`.

**Namespaced mode** (`rbac.clusterWide=false`, or `--namespace=<ns>`): the chart
emits a namespaced `Role`/`RoleBinding` instead, dropping the cluster-scoped
resources (`nodes`, `persistentvolumes`, `namespaces`) that a `Role` cannot
grant. Pulsebox detects the single-namespace scope and skips watching those
resources accordingly.

---

## Reaching the dashboard

This cluster uses **Traefik v3 `IngressRoute` CRDs**, not the standard
`networking.k8s.io/Ingress`. `k8s/ingressroute.yaml` (and the chart's
`ingressroute.yaml`) exposes the dashboard on its own subdomain over the
`websecure` entrypoint, with TLS from the existing wildcard secret:

```yaml
match: Host(`pulsebox.your-domain.tld`)
services:
  - name: pulsebox
    port: 8080          # dashboard/API only; metrics (:8037) is NOT exposed
tls:
  secretName: wildcard-datatejarat-tls
```

The WebSocket (`/api/stream`) rides the same host/route — Traefik proxies it
transparently. Metrics stay internal (scraped in-cluster via the ServiceMonitor).

> **TLS secret note:** Traefik resolves `secretName` from the IngressRoute's own
> namespace. The wildcard secret lives in the `traefik` namespace, so either
> replicate it into the `pulsebox` namespace or configure it as Traefik's
> default certificate via a `TLSStore`.

No auth in v1 (matches the cluster's internal-service pattern). See
[Follow-ups](#follow-ups).

---

## Prometheus discovery

`k8s/servicemonitor.yaml` / the chart's ServiceMonitor is scraped by
kube-prometheus-stack.

> ⚠️ **Known cluster gotcha:** the Prometheus instance here only selects
> ServiceMonitors labeled **`release: monitoring`** — because the Helm release is
> named `monitoring`, *not* `kube-prometheus-stack`. If this label is missing or
> "corrected" to `release: kube-prometheus-stack`, Prometheus **silently ignores
> the target** and no Pulsebox metrics are scraped. The label is set (with a
> warning comment) in both the manifest and the chart; override it via
> `serviceMonitor.releaseLabel` if your Prometheus release differs.

The monitor scrapes the `metrics` port at `/metrics` every 30 s.

---

## Metrics reference

All metrics are numeric gauges/counters with the `pulsebox_` prefix (loosely
following kube-state-metrics conventions).

| Metric | Type | Labels | Meaning |
|--------|------|--------|---------|
| `pulsebox_node_ready` | gauge | `node` | `1` if the node's `Ready` condition is True, else `0`. |
| `pulsebox_node_condition` | gauge | `node`, `condition` | `1` if the named condition (`MemoryPressure`/`DiskPressure`/`PIDPressure`/`NetworkUnavailable`) is active. |
| `pulsebox_pod_health_status` | gauge | `namespace`, `pod`, `node`, `phase` | `0`=unhealthy, `1`=healthy, `2`=starting/degraded, `3`=unknown. |
| `pulsebox_pod_container_restarts_total` | counter | `namespace`, `pod`, `container` | Cumulative container restart count. |
| `pulsebox_pod_restart_rate_per_minute` | gauge | `namespace`, `pod` | Recently observed restart rate (feeds pod health). |
| `pulsebox_workload_replicas_ready` | gauge | `namespace`, `kind`, `name` | Ready/available replicas. |
| `pulsebox_workload_replicas_desired` | gauge | `namespace`, `kind`, `name` | Desired replicas. |
| `pulsebox_pv_status` | gauge | `pv`, `phase` | `1` for the PV's current phase (see `phase` label). |
| `pulsebox_pvc_status` | gauge | `namespace`, `pvc`, `phase` | `1` for the PVC's current phase. |
| `pulsebox_pvc_pending_seconds` | gauge | `namespace`, `pvc` | Seconds a PVC has been `Pending` (`0` if bound). |
| `pulsebox_service_endpoints_ready` | gauge | `namespace`, `service` | Ready endpoint addresses backing a Service. |
| `pulsebox_namespace_health_score` | gauge | `namespace` | Aggregate namespace health, `0.0`–`1.0`. |
| `pulsebox_cluster_health_score` | gauge | — | Aggregate cluster health, `0.0`–`1.0`. |
| `pulsebox_collector_synced` | gauge | — | `1` once informer caches finished their initial sync. |

Standard Go runtime and process metrics are also exported.

> **Deprecation note:** the old Docker-era metrics `container_health_status` and
> `container_health_check_duration_seconds` are **removed** — they described a
> single Docker host and have no cluster equivalent. Use
> `pulsebox_pod_health_status` and `pulsebox_pod_container_restarts_total`
> instead.

Example Grafana/PromQL:

```promql
# unhealthy pods right now
pulsebox_pod_health_status == 0
# PVCs stuck pending longer than 2 minutes
pulsebox_pvc_pending_seconds > 120
# services with no ready endpoints (silent outages)
pulsebox_service_endpoints_ready == 0
# restart storms
rate(pulsebox_pod_container_restarts_total[5m]) > 0
```

---

## Topology API + WebSocket

Served on the `http` port (`:8080`).

| Endpoint | Description |
|----------|-------------|
| `GET /api/topology` | Full cluster graph snapshot. |
| `GET /api/topology/{namespace}` | Graph scoped to one namespace (+ referenced nodes/PVs). |
| `GET /api/resource/{kind}/{namespace}/{name}` | Full cached object + recent warning events for the detail panel. Use `-` for the namespace of cluster-scoped kinds (e.g. `/api/resource/node/-/worker1`). |
| `GET /api/stream` | WebSocket: one `snapshot` message on connect, then incremental `diff` messages. |
| `GET /api/health` | Pulsebox's **own** liveness (`?ready=1` for readiness) — distinct from cluster health. |

### Graph schema

```jsonc
{
  "nodes": [
    {
      "id": "Pod/app/web-abc",          // stable: Kind/[namespace/]name
      "kind": "Pod",                     // Cluster|Namespace|Node|Deployment|
                                         // StatefulSet|DaemonSet|Pod|Service|
                                         // PersistentVolumeClaim|PersistentVolume
      "name": "web-abc",
      "namespace": "app",                // omitted for cluster-scoped kinds
      "health": "healthy",               // healthy|degraded|unhealthy|unknown
      "score": 1.0,                      // 0.0–1.0
      "reasons": ["..."],                // present when not healthy
      "meta": {                          // small render hints (strings)
        "node": "worker1", "restarts": "0", "phase": "Running", "gpu": "true"
      }
    }
  ],
  "edges": [
    { "id": "runs-on:Pod/app/web-abc->Node/worker1",
      "source": "Pod/app/web-abc", "target": "Node/worker1", "kind": "runs-on" }
    // edge kinds: contains | runs-on | mounts | backs | routes
  ],
  "updatedAt": "2026-07-08T12:00:00Z"
}
```

### WebSocket message format

```jsonc
// first frame after connect — full state
{ "type": "snapshot", "upsertNodes": [], "upsertEdges": [], "timestamp": "..." }

// subsequent frames — incremental diffs as informer caches change
{ "type": "diff",
  "upsertNodes": [],            // added or changed nodes
  "upsertEdges": [],
  "removeNodes": ["Pod/app/old"],   // ids only
  "removeEdges": ["runs-on:..."],
  "timestamp": "..." }
```

The dashboard applies these incrementally, preserving node positions. If the
socket drops it auto-reconnects with backoff and, meanwhile, **degrades to
polling** `GET /api/topology` every 5 s.

### Dashboard features

- Force-directed graph, color-coded by health (green/amber/red/gray).
- **GPU nodes rendered distinctly** (purple hexagon) — GPU capacity is scarce here.
- Top summary bar: cluster health %, healthy/degraded/unhealthy pod counts,
  namespace/pod/node/GPU tallies.
- Namespace filter, name search, pan/zoom, drag-to-pin.
- Click a node for detail: labels, restart count, node placement, mounted PVCs,
  reasons, and recent warning events.

---

## Configuration

Flags (each has an env equivalent):

| Flag | Env | Default | Purpose |
|------|-----|---------|---------|
| `--metrics-addr` | `PULSEBOX_METRICS_ADDR` | `:8037` | Prometheus `/metrics` listener. |
| `--http-addr` | `PULSEBOX_HTTP_ADDR` | `:8080` | Dashboard + API listener. |
| `--namespace` | `PULSEBOX_NAMESPACE` | `""` | Restrict watches to one namespace (empty = cluster-wide). |
| `--pvc-pending-grace` | `PULSEBOX_PVC_PENDING_GRACE` | `2m` | PVC pending longer than this → unhealthy. |
| `--kubeconfig` | `PULSEBOX_KUBECONFIG` | `""` | Out-of-cluster kubeconfig (dev). In-cluster config is preferred automatically. |

---

## Local development

Requires Go 1.24+ and access to a cluster (kubeconfig). Pulsebox uses
`rest.InClusterConfig()` first, then falls back to `--kubeconfig` /
`$KUBECONFIG` / `~/.kube/config`.

```bash
go test ./...                 # unit + fake-cluster integration tests
go run ./cmd/pulsebox         # runs against your current kube-context
# dashboard: http://localhost:8080/   metrics: http://localhost:8037/metrics
```

Or via the dev-only compose file (see the banner in `docker-compose.yml`):

```bash
docker compose up
```

Frontend source lives in `internal/web/static/` (plain HTML/CSS/JS, no build
step) and is embedded via `go:embed` at compile time.

---

## Follow-ups

- **Auth:** v1 has none, matching this cluster's internal services (vLLM/Milvus).
  A natural next step is a Traefik middleware in front of the IngressRoute —
  Keycloak or oauth2-proxy — without touching Pulsebox itself.
- **Large clusters:** the layout simulates in-browser; use the namespace filter
  (or `/api/topology/{namespace}`) to focus. Barnes-Hut / server-side layout
  could come later.

---

## License

Apache License 2.0. Developed by **Ramtin Boreili** —
[GitHub](https://github.com/Ramtinboreili/Pulsebox) ·
[LinkedIn](https://www.linkedin.com/in/ramtin-boreili/).
