# Pulsebox — Deployment & Operations Guide

This guide walks you through what to do with the Pulsebox codebase now that it is a Kubernetes-native cluster health exporter with a live topology dashboard: how to build and publish the image, how to deploy it (Helm or plain manifests), how to work with the Helm chart day to day, how to wire up Prometheus and the Traefik dashboard route, and how to verify and troubleshoot it.


## 0. Quick start (TL;DR)

```
# 1) Build & push the image to Docker Hub
docker build -t ramtinboreili/pulsebox:v1.3.0 --build-arg VERSION=v1.3.0 .
docker push ramtinboreili/pulsebox:v1.3.0

# 2) Install with Helm into its own namespace
helm upgrade --install pulsebox helm/pulsebox \
  --namespace pulsebox --create-namespace \
  --set ingressRoute.host=pulsebox.your-domain.tld \
  --set ingressRoute.tls.secretName=wildcard-datatejarat-tls

# 3) Check it
kubectl -n pulsebox get pods
kubectl -n pulsebox port-forward svc/pulsebox 8037:8037 &
curl -s localhost:8037/metrics | grep pulsebox_ | head
```

That is the whole flow. The rest of this document explains each step and your options.


## 1. What you have now

Pulsebox runs as a single Deployment inside your cluster. It watches cluster state through the Kubernetes API (using client-go informers — it does **not** poll and it does **not** use the Docker socket anymore) and serves two things:

- **Prometheus metrics** on port **8037** (`/metrics`) — for Grafana and alerting.
- **A live topology dashboard** on port **8080** (`/`) plus a JSON/WebSocket API (`/api/*`) — an interactive graph of namespaces, workloads, pods, nodes and PV/PVC, colour-coded by health.

The dashboard frontend is compiled into the binary, so the image is a single static file with no CDN or asset dependencies. The published image is public on Docker Hub: `docker pull ramtinboreili/pulsebox:v1.3.0`.


## 2. Prerequisites

- A Kubernetes cluster (this was built against v1.33) and `kubectl` pointed at it.
- `helm` v3 (recommended path) — or just `kubectl` if you prefer the plain manifests.
- `docker` (or another OCI builder) and push access to the `ramtinboreili` Docker Hub namespace.
- For metrics: kube-prometheus-stack already installed (it is, in the target cluster).
- For the dashboard URL: Traefik v3 with IngressRoute CRDs and a TLS certificate.


## 3. Build & publish the image

The frontend is embedded at build time, so a normal Docker build is all you need. Always pin a real tag — never `:latest`.

```
docker build -t ramtinboreili/pulsebox:v1.3.0 --build-arg VERSION=v1.3.0 .
docker login            # if not already logged in to Docker Hub
docker push ramtinboreili/pulsebox:v1.3.0
```

> **Note:** The image is public, so clusters need no pull secret. If you later make it private, add a pull secret and set `imagePullSecrets` in values.yaml (Helm) or uncomment the block in k8s/deployment.yaml.

To cut a new version later: bump the tag (e.g. `v1.0.1`), rebuild/push, then bump `image.tag` in values.yaml (and ideally `Chart.yaml` appVersion/version) to match.


## 4. Deploy — pick one method


### 4a. Helm (recommended)

The chart lives in `helm/pulsebox/`. Install or upgrade with:

```
helm upgrade --install pulsebox helm/pulsebox \
  --namespace pulsebox --create-namespace \
  --set image.tag=v1.3.0 \
  --set ingressRoute.host=pulsebox.your-domain.tld \
  --set ingressRoute.tls.secretName=wildcard-datatejarat-tls
```

`upgrade --install` is idempotent — the same command installs the first time and upgrades every time after. Prefer a values file over long `--set` chains for real environments:

```
# my-values.yaml
image:
  tag: v1.3.0
ingressRoute:
  host: pulsebox.your-domain.tld
  tls:
    secretName: wildcard-datatejarat-tls
serviceMonitor:
  releaseLabel: monitoring
```

```
helm upgrade --install pulsebox helm/pulsebox -n pulsebox --create-namespace -f my-values.yaml
```

Other useful commands:

```
helm template pulsebox helm/pulsebox -f my-values.yaml   # render YAML without applying
helm lint helm/pulsebox                                  # sanity-check the chart
helm -n pulsebox status pulsebox                         # release status + notes
helm -n pulsebox uninstall pulsebox                      # remove everything
helm package helm/pulsebox                               # produce pulsebox-1.3.0.tgz to share
```


### 4b. Plain manifests (no Helm)

Everything is also available as static YAML in `k8s/`. Before applying, edit:

- `k8s/deployment.yaml` — the `image:` line if your tag differs.
- `k8s/ingressroute.yaml` — the `Host(...)` and `tls.secretName`.

```
kubectl apply -f k8s/
```

The files are: namespace, serviceaccount, clusterrole, clusterrolebinding, deployment, service, ingressroute, servicemonitor.


## 5. Helm chart — what the values mean

Everything you would normally change is exposed in `helm/pulsebox/values.yaml`:

- `image.repository` / `image.tag` — the Docker Hub image and tag.
- `imagePullSecrets` — empty by default (public image); add a name for a private registry.
- `resources.requests` — CPU/memory requests. Per cluster convention there are **no limits**.
- `ports.metrics` (8037) / `ports.http` (8080) — the two listener ports.
- `serviceMonitor.enabled` / `serviceMonitor.releaseLabel` — Prometheus discovery (see §7).
- `ingressRoute.enabled` / `host` / `tls.secretName` — the dashboard URL and cert (see §6).
- `rbac.clusterWide` — `true` watches the whole cluster (ClusterRole); `false` scopes to one namespace with a Role (`rbac.watchNamespace`).
- `pvcPendingGrace` — how long a PVC may stay Pending before it is scored unhealthy (default 2m).

> **Note:** If you fork the chart or publish it, `helm package helm/pulsebox` creates a versioned `.tgz`. You can host these in any HTTP chart repo (or an OCI registry via `helm push`) so others can `helm install` by name instead of from the folder.


## 6. Expose the dashboard (Traefik IngressRoute)

This cluster uses Traefik v3 IngressRoute CRDs, not the standard Ingress. The chart creates an IngressRoute that serves **only** the dashboard/API port (8080); metrics (8037) stay internal. Set the host and TLS secret to match your domain:

```
ingressRoute:
  host: pulsebox.your-domain.tld
  tls:
    secretName: wildcard-datatejarat-tls
```

> **Note:** Traefik reads `secretName` from the IngressRoute's OWN namespace. Your wildcard secret lives in the `traefik` namespace, so either copy it into the `pulsebox` namespace or set it as Traefik's default certificate via a TLSStore. The WebSocket (/api/stream) works over the same host automatically.

No login is required in v1 (matching your other internal services). To add auth later, put a Traefik middleware — Keycloak or oauth2-proxy — in front of the IngressRoute; Pulsebox itself does not change.


## 7. Prometheus scraping (important gotcha)

The chart ships a ServiceMonitor labelled `release: monitoring`. In this cluster the kube-prometheus-stack Prometheus only selects ServiceMonitors with that exact label, because the Helm release is named **monitoring** (not kube-prometheus-stack).

> **Note:** If this label is missing or 'corrected' to release: kube-prometheus-stack, Prometheus SILENTLY ignores Pulsebox and you get no metrics. If your Prometheus release name is different, set `serviceMonitor.releaseLabel` to match it.

Confirm the target is picked up: in the Prometheus UI, Status → Targets, look for a `pulsebox` job in state UP.


## 8. Access & verify

Reach the dashboard at `https://pulsebox.your-domain.tld/`, or without ingress via port-forward:

```
kubectl -n pulsebox port-forward svc/pulsebox 8080:8080
# open http://localhost:8080/
```

Sanity checks (all should pass within ~30s of the pod running):

1. `kubectl -n pulsebox get pods` shows the pod Ready.
2. `curl -s localhost:8037/metrics | grep pulsebox_cluster_health_score` returns a value 0.0–1.0.
3. `curl -s localhost:8080/api/topology` returns JSON with a non-empty `nodes` array.
4. The dashboard renders a colour-coded graph; GPU nodes show as purple hexagons.
5. In Prometheus, the `pulsebox` target is UP.


## 9. Day-to-day git workflow

The rewrite was developed on a feature branch and merged into `dev`. A sensible flow going forward:

- `main` — stable / released.
- `dev` — integration branch (this work lives here now).
- feature branches off `dev`, merged back via PR.

Typical commands:

```
git checkout dev && git pull
git checkout -b my-change
# ...edit, then:
go test ./...            # run the test suite before committing
git commit -am "describe change"
git push -u origin my-change   # open a PR into dev

# cut a release
git checkout main && git merge dev
git tag v1.3.0 && git push --tags
```


## 10. Troubleshooting


#### No Pulsebox metrics in Prometheus

- Check the ServiceMonitor label is `release: monitoring` (§7).
- Confirm `/metrics` works directly: port-forward 8037 and curl it.
- Check the pod is Ready — readiness gates on the informer caches syncing.


#### Dashboard loads but the graph is empty

- Look at pod logs: `kubectl -n pulsebox logs deploy/pulsebox`.
- A `forbidden` error means RBAC did not apply — reinstall so the ClusterRole/binding exist.
- If you set `rbac.clusterWide=false`, nodes/PVs/namespaces are intentionally not shown.


#### Pod won't start / ImagePullBackOff

- Verify the tag exists: `docker pull ramtinboreili/pulsebox:v1.3.0`.
- If the repo is private, add a pull secret and set `imagePullSecrets`.


#### Dashboard 404 / cert warning

- The IngressRoute host must match your DNS, and the TLS secret must exist in the pulsebox namespace (§6).


## 11. Next steps

- Add authentication in front of the dashboard (Traefik + Keycloak/oauth2-proxy).
- Build Grafana dashboards on the pulsebox_ metrics (see the README metrics table).
- Wire alerts, e.g. `pulsebox_pvc_pending_seconds > 120` or `pulsebox_service_endpoints_ready == 0`.
- Publish the Helm chart to a chart repo or OCI registry for one-command installs.

Full reference (every metric, the topology JSON schema, and the WebSocket message format) is in the project README.

