# kshows — cluster capacity visualizer

A browser-based spatial map of Kubernetes node capacity and how workloads fill
it. Every node is a rectangle; pods are packed inside proportionally to their
resource footprint. Three dimensions: **CPU** and **RAM** as per-pod treemaps,
**disk** as a node-level fill.

The signature view is the **requests-vs-actual dual encoding**: each pod block
draws its *reservation* (request) as a dashed outline and its *actual usage*
as the solid fill inside it. The gap between the two is your slack — visible
at a glance, per pod, per node, across the cluster.

- **Read-only, forever.** The ClusterRole has no write verbs of any kind.
- **Cloud-agnostic.** Works on any conformant cluster: on-prem, bare-metal,
  GKE, EKS, AKS.
- **Degrades gracefully.** No Metrics Server? You get a requests/limits view
  with a banner. `nodes/proxy` restricted? Disk shows capacity only.
- **Single binary.** Go backend + embedded UI, ~zero-dependency frontend.

## Quickstart (in-cluster, primary)

```sh
kubectl apply -f deploy/rbac.yaml -f deploy/deployment.yaml
kubectl -n kshows port-forward svc/kshows 8080:80
# open http://localhost:8080
```

Exposure beyond port-forward (Ingress, LoadBalancer) is the operator's call —
kshows has no built-in auth in v1, so treat the Service as you would any
internal dashboard.

## Local mode (evaluation)

The same binary runs outside the cluster against your kubeconfig — credential
source is the only difference:

```sh
go build -o bin/kshows ./cmd/kshows
./bin/kshows                       # uses ~/.kube/config
./bin/kshows --kubeconfig /path    # explicit path
```

In local mode you inherit your own RBAC; with namespace-scoped access you'll
see only what you can list.

## Demo mode (no cluster at all)

```sh
./bin/kshows --mock
```

Serves a simulated cluster with drifting usage — useful for trying the UI or
developing the frontend.

## Views

| Control | Choices | Meaning |
|---|---|---|
| Dimension | CPU / RAM / DISK | which resource the map shows |
| Metric | REQUESTS / LIMITS / ACTUAL | how pod blocks are sized |

- **REQUESTS / LIMITS** — solid blocks sized by the declared value. Nodes
  overcommitted on the chosen metric get an `over ×N` badge (normal for
  limits) and the map scales to the committed total.
- **ACTUAL** — the dual encoding. Block area = max(request, usage); dashed
  outline = request; solid fill = live usage. Pods bursting past their
  request are flagged with an amber outline.
- **DISK** — node-level used-vs-free bar (v1 scope: no per-pod disk, no
  PV/PVC attribution). Warns at 75%, critical at 90%.

Hover any block for details; click a node card for the single-node view with
a full pod table. Views are shareable via URL params:
`/?dim=mem&metric=requests&node=node-02`.

## Data sources

| Signal | Source | Required RBAC | If missing |
|---|---|---|---|
| Node capacity | core API `Node.status.allocatable` | `nodes: get,list,watch` | — (required) |
| Pod requests/limits | core API pod spec | `pods: get,list,watch` | — (required) |
| CPU/RAM usage | Metrics Server (`metrics.k8s.io`) | `nodes,pods: get,list` | requests/limits view + banner |
| Node disk | kubelet Summary API via apiserver proxy | `nodes/proxy: get` | capacity-only + note |

Nodes and pods come from shared informer caches (no re-listing); metrics are
polled every 15s (Metrics Server's own resolution), disk every 60s with
bounded per-node concurrency.

## API

- `GET /api/v1/snapshot` — full current model (JSON)
- `GET /api/v1/stream` — SSE; pushes a snapshot per poll, blocks keyed by pod UID
- `GET /api/v1/capabilities` — which signals are live
- `GET /healthz` / `GET /readyz` — probes

## Development

```sh
make build      # binary at bin/kshows
make test       # Go + JS (layout math) tests
make run-mock   # local UI against simulated data
make image      # container image
```

The frontend is plain ES modules + SVG, served from `web/static/` and embedded
at build time — rebuild the binary to pick up UI changes. Treemap layout math
(`treemap.js`) is renderer-agnostic; a Canvas renderer for very large clusters
can replace the SVG layer without touching layout or data code.

## Status

v1 per the PRD: live point-in-time view, no history, no cost allocation, no
multi-cluster, no write operations (ever). License: TBD (PRD open question).
