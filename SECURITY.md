# Security

## Security posture

kshows is designed to be boring to a security reviewer. The properties below
are structural, not policy — they hold because of what the code can and
cannot do.

**Read-only, with no write code path.** The shipped ClusterRole
(`deploy/rbac.yaml`) grants only `get`, `list`, and `watch`. There is no code
in this repository that issues a write to the Kubernetes API, so even a
ServiceAccount over-provisioned by an operator would not be used to mutate
anything.

**Minimal blast radius.** The container runs as non-root (UID 65532) with a
read-only root filesystem, no privilege escalation, and all capabilities
dropped, on a distroless base with no shell or package manager.

**No persistence, no egress.** The entire model is in memory and rebuilt from
the API each poll. kshows opens no outbound connections other than to the
Kubernetes API server it was pointed at, writes no files, and ships no
telemetry.

**What it can read.** Node objects, pod specs and metadata cluster-wide, pod
and node resource metrics, and node-level filesystem stats via the kubelet
Summary API. Pod names and namespaces are the most sensitive data it handles,
and they are rendered in the UI. It does not read Secrets, ConfigMaps,
container logs, or pod contents.

## Exposure is the operator's responsibility

**kshows has no authentication or authorization for end users.** Anyone who
can reach the HTTP port sees the whole cluster's node and pod inventory.

The default quickstart uses `kubectl port-forward`, which inherits your own
cluster credentials and exposes nothing to the network. If you put kshows
behind an Ingress or a LoadBalancer, you are responsible for putting
authentication in front of it. Treat the endpoint as equivalent to read
access to pod and node metadata cluster-wide.

## Reporting a vulnerability

Please report security issues privately via GitHub's
[private vulnerability reporting](https://github.com/tekikaito/kshows/security/advisories/new)
rather than opening a public issue.

Include the version (`kshows --version`), your Kubernetes distribution, and
enough detail to reproduce. Expect an initial response within a week. As an
unfunded open-source project there is no formal SLA, but anything affecting
the read-only guarantee or enabling cross-tenant information disclosure is
treated as urgent.

## Supported versions

The latest released version is supported. Fixes ship in a new release rather
than as patches to older tags.
