# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] - 2026-07-26

First release. Live, read-only, point-in-time capacity visualization.

### Added

- **Spatial node map.** Every node renders as a rectangle with its pods packed
  inside as a squarified treemap, proportional to allocatable capacity, with
  the unallocated remainder drawn as an explicit hatched region.
- **Requests-vs-actual dual encoding.** In the ACTUAL view each pod block
  draws its request as a dashed reservation outline and live usage as the
  solid fill, so the slack between them is visible per pod. Pods bursting past
  their request are flagged.
- **Three dimensions.** CPU and RAM as per-pod treemaps; disk as a node-level
  used-vs-free fill with warning (75%) and critical (90%) thresholds.
- **Explicit metric semantics.** REQUESTS / LIMITS / ACTUAL toggle, with
  overcommit badges when committed totals exceed allocatable.
- **Live updates over SSE**, with blocks keyed by pod UID so changes animate
  rather than flicker.
- **Drill-in node view** with a full pod table (request, limit, actual,
  percentage of request).
- **Graceful degradation.** Capability probing detects a missing Metrics
  Server or restricted `nodes/proxy` and grays out what it cannot populate,
  with an explanatory banner, instead of rendering zeros as if they were data.
  Transient API failures are distinguished from definitive absence and only
  drop a capability after repeated failures, so the UI does not flap.
- **Two credential sources, one binary.** In-cluster ServiceAccount
  (primary) or kubeconfig (local evaluation), plus a `--mock` mode that serves
  a simulated cluster for demos and UI development.
- **JSON API**: `/api/v1/snapshot` (gzip-capable), `/api/v1/stream` (SSE),
  `/api/v1/capabilities`, and health probes.
- **Deployment manifests** with a strictly read-only ClusterRole, a hardened
  non-root pod spec, and a distroless multi-arch container image.

[Unreleased]: https://github.com/tekikaito/kshows/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/tekikaito/kshows/releases/tag/v0.1.0
