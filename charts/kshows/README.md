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
turn them off. The chart then also starts kshows with that signal switched off,
so it never sends a request that would only be denied, and the UI narrows with
an explanatory banner rather than failing or, worse, rendering zeros that look
like real data.

| Value | Grants | Withheld |
|---|---|---|
| `rbac.metrics` | `metrics.k8s.io` on nodes and pods | No actual-usage view; requests/limits only |
| `rbac.nodeStats` | `nodes/stats`, to read each kubelet's Summary API directly | Disk falls back to `nodesProxy`, or capacity only |
| `rbac.nodesProxy` | `nodes/proxy`, the same data through the API server; also reaches the kubelet's exec and run | Disk uses `nodeStats` alone, or capacity only |

`rbac.nodesProxy` is off by default. Turn it on only when pods cannot reach
the kubelet port (10250) and you would rather grant the broader permission
than lose live disk usage:

```sh
helm install kshows oci://ghcr.io/tekikaito/charts/kshows \
  --namespace kshows --create-namespace \
  --set rbac.nodesProxy=true
```

For kubelets with self-signed serving certificates, prefer
`--set collector.kubeletInsecureTLS=true` over the proxy.

To bind an existing ServiceAccount instead, set `rbac.create=false`,
`serviceAccount.create=false`, and `serviceAccount.name=<yours>`. You then own
the RBAC, so switch off whatever you don't grant yourself, e.g.
`--set extraArgs='{--node-disk=kubelet}'` when you grant only `nodes/stats`.

## Values

| Key | Default | Description |
|---|---|---|
| `replicaCount` | `1` | kshows holds no shared state; more than one replica just means more API load |
| `image.repository` | `ghcr.io/tekikaito/kshows` | |
| `image.tag` | `""` | Defaults to the chart's `appVersion` |
| `rbac.create` | `true` | Create the read-only ClusterRole and binding |
| `rbac.metrics` | `true` | Grant `metrics.k8s.io` for live CPU/RAM |
| `rbac.nodeStats` | `true` | Grant `nodes/stats` to read live node disk from each kubelet |
| `rbac.nodesProxy` | `false` | Grant `nodes/proxy` as the fallback route for live node disk |
| `serviceAccount.create` | `true` | |
| `serviceAccount.name` | `""` | Generated from the release name when empty |
| `collector.pollInterval` | `15s` | Metrics Server's own resolution; faster gains nothing |
| `collector.kubeletInsecureTLS` | `false` | Skip verifying kubelet certificates on the direct disk route (self-signed kubelets) |
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
