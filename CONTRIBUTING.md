# Contributing to kshows

Thanks for taking the time. This is a small project with a deliberately small
scope — the fastest way to get a change merged is to check it against that
scope first.

## The one rule

**kshows is read-only, permanently.** No pull request that adds a write verb
to the ClusterRole, or a code path that mutates cluster state, will be
accepted. This is the property admins install the tool on; it is not up for
negotiation. Everything else is open to discussion.

## Getting set up

You need Go (the version in `go.mod`) and Node (for the layout-math tests).
No frontend toolchain, no bundler — the UI is plain ES modules embedded into
the binary.

```sh
git clone https://github.com/tekikaito/kshows
cd kshows
make build
./bin/kshows --mock     # simulated cluster, no Kubernetes needed
```

`--mock` is the fastest development loop: it serves a synthetic cluster with
drifting usage, so you can work on the UI without a cluster. Note the UI is
embedded at build time — rerun `make build` to pick up changes under
`web/static/`.

To run against a real cluster, just drop the flag; kshows uses your
kubeconfig with your own RBAC.

## Before you open a PR

```sh
make test     # Go tests (race-enabled) + treemap layout tests
make lint     # golangci-lint if installed, go vet otherwise
```

CI runs the same checks plus manifest validation and a container build.

## What good changes look like

- **Match the surrounding style.** Comments explain constraints and *why*,
  not what the next line does.
- **Test the failure path, not just the happy one.** The degradation
  behavior (missing Metrics Server, restricted `nodes/proxy`) is the product,
  so it has the most test coverage. Keep it that way — see
  `internal/collector/collector_test.go` for the fake-clientset patterns.
- **Don't add frontend dependencies.** The zero-dependency, no-build-step UI
  is a deliberate constraint. Layout math lives in `treemap.js` behind a
  renderer-agnostic interface; keep rendering concerns out of it.
- **Keep the API wire shape stable** unless the change is the point. The UI
  and any third-party consumers read `/api/v1/snapshot`.

## Scope

Currently out of scope (v1): historical trends, per-pod disk and PV/PVC
attribution, cost allocation, rightsizing advice, multi-cluster. These aren't
"no forever" — they're "not until the core is proven." Open an issue to make
the case rather than opening a large PR cold.

## Reporting bugs

Include your Kubernetes distribution and version, whether Metrics Server is
installed, and what kshows logged at startup. Capability-detection issues are
usually visible in the first few log lines.

Security issues: see [SECURITY.md](SECURITY.md) — please don't open a public
issue for those.
