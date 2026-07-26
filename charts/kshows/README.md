# kshows Helm chart

A spatial map of Kubernetes node capacity — pods packed into node rectangles
by actual usage vs. requests, across CPU, RAM and disk. Read-only.

## Install

```sh
helm install kshows oci://ghcr.io/tekikaito/charts/kshows \
  --namespace kshows --create-namespace

kubectl -n kshows port-forward svc/kshows 8080:80
```

## Permissions and graceful degradation

kshows is read-only: the ClusterRole holds only `get`, `list`, and `watch`,
and there is no code path that writes to the Kubernetes API.

Two of its permissions are optional. If your platform team won't grant them,
turn them off — kshows detects the missing signal and narrows the UI with an
explanatory banner rather than failing or, worse, rendering zeros that look
like real data.

| Value | Grants | Withheld |
|---|---|---|
| `rbac.metrics` | `metrics.k8s.io` on nodes and pods | No actual-usage view; requests/limits only |
| `rbac.nodesProxy` | `nodes/proxy` (kubelet Summary API) | Disk shows capacity only, no live usage |

```sh
# For a cluster that restricts the kubelet proxy, as some managed ones do
helm install kshows oci://ghcr.io/tekikaito/charts/kshows \
  --namespace kshows --create-namespace \
  --set rbac.nodesProxy=false
```

To bind an existing ServiceAccount instead, set `rbac.create=false`,
`serviceAccount.create=false`, and `serviceAccount.name=<yours>`.

## Values

| Key | Default | Description |
|---|---|---|
| `replicaCount` | `1` | kshows holds no shared state; more than one replica just means more API load |
| `image.repository` | `ghcr.io/tekikaito/kshows` | |
| `image.tag` | `""` | Defaults to the chart's `appVersion` |
| `rbac.create` | `true` | Create the read-only ClusterRole and binding |
| `rbac.metrics` | `true` | Grant `metrics.k8s.io` for live CPU/RAM |
| `rbac.nodesProxy` | `true` | Grant `nodes/proxy` for live node disk |
| `serviceAccount.create` | `true` | |
| `serviceAccount.name` | `""` | Generated from the release name when empty |
| `collector.pollInterval` | `15s` | Metrics Server's own resolution; faster gains nothing |
| `extraArgs` | `[]` | Additional container flags |
| `service.type` | `ClusterIP` | |
| `service.port` | `80` | |
| `ingress.enabled` | `false` | **Read the warning below before enabling** |
| `serviceMonitor.enabled` | `false` | Requires the Prometheus Operator CRDs |
| `serviceMonitor.interval` | `30s` | |
| `podAnnotations` | `prometheus.io/*` | Annotation-based scraping for a plain Prometheus |
| `resources.limits.memory` | `256Mi` | Raise it on large clusters — informers cache node and pod objects |
| `nodeSelector`, `tolerations`, `affinity`, `topologySpreadConstraints` | `{}` / `[]` | Standard scheduling controls |

## Exposure

**kshows has no built-in authentication.** Anyone who can reach it sees every
node and pod name in the cluster. The default `ClusterIP` plus
`kubectl port-forward` exposes nothing to the network; enabling `ingress` or a
`LoadBalancer` Service publishes the whole inventory, so put authentication in
front of it.

## Metrics

`/metrics` reports kshows' own operating state — poll timing, signal health,
connected SSE clients, snapshot staleness — not cluster capacity, which is
kube-state-metrics' job. See the
[project README](https://github.com/tekikaito/kshows#observability).
