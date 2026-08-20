---
id: 77
slug: sandbox-resource-limits-concurrency-ceiling
title: sandbox resource limits — cgroup caps per container and a concurrency ceiling on in-flight Execs
status: proposed
priority: high
type: feat
created: 2026-08-20
updated: 2026-08-20
depends_on: [63]
related: [63, 64, 75, 76]
discovered_from: [63]
adrs: [44]
spec:
plan:
results:
trivial: false
auto_groomable:
branch:
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| ADRs | [ADR-0044](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0044-bash-tool-contained-not-credentialed.md) |
<!-- docket:artifacts:end -->

## Why

The #63 container substrate handles container **lifetime and cleanup** well — `docker run --rm` per `Exec` is ephemeral and self-cleaning, the warm pool keeps at most one idle context per `Principal` and idle-reaps it after `DefaultIdleTTL` (5m), and `Close` tears everything down deterministically. But it does **no resource *limiting*, and that is the DoS surface**:

- **No cgroup caps on the container.** The run argv (`internal/tools/sandbox/container.go`) is `run --rm -i --env… -v… -w… image sh -c …` with **no `--memory`, `--cpus`, `--pids-limit`, or `--ulimit`**. A model-authored command can fork-bomb, exhaust host RAM, spin CPU, or fill the mount up to whatever the host allows. There is a `TODO(#0064)` for `--network` but **no owner at all** for compute/memory/pid/disk limits.
- **No ceiling on concurrent `Exec`s.** The per-principal "max one warm" rule bounds *idle* containers only; nothing bounds *in-flight* commands. N concurrent loops — or one loop issuing rapid commands — spawn N concurrent `docker run`s with no cap and no queue (the pool has no semaphore; `grep` for max/limit/semaphore finds only the reap-interval clamp).
- **Unbounded inline image pull.** A cold image pays an unbounded `pull` under the command's own deadline on first `Exec`.

This is the resource-management layer every orchestration target depends on: it is what a k8s handler (#75) expresses as Pod `resources.limits` + a per-tenant `ResourceQuota`, and what the Helm chart (#76) needs a config seam for. Doing it once, host-side and behind config, gives both a uniform model to map onto. It is filed `high` because it is a live denial-of-service hole in a shipped feature, not a nice-to-have.

## What changes

- **Per-container cgroup caps** on the local container substrate: `--memory`, `--cpus`, `--pids-limit`, and a disk/`--ulimit` bound, sourced from **trusted operator config only** (never model output, never a wire field — same trust boundary as the off-switch and env-allowlist), with fail-safe defaults so an unconfigured hosted profile is still bounded.
- **A concurrency ceiling on in-flight `Exec`s**, enforced at the pool (or a sibling admission gate): a global cap and/or a per-`Principal.Tenant` cap, with a bounded queue and a clear refusal (or wait-with-deadline) when the ceiling is hit — so one tenant cannot starve others and the host cannot be swamped. Coordinate with ADR-0007's Scheduler as the existing admission authority rather than inventing a second one.
- **Bounded image acquisition**: a pull timeout / pre-pull path so a cold image cannot hang an `Exec` indefinitely.
- **New bounded observability**, consistent with #63's families: a refused/queued-for-limit signal (candidate: extend `fuse_sandbox_*` with a rejection counter, or reuse the `KindSandboxHealth` `acquire_failed` reason once #74 lands an emitter — cross-reference #74).

## Out of scope

- The network/egress floor — Change #64 (`--network none` + allowlist). This change owns compute/memory/pid/disk and concurrency, not network.
- Per-tenant filesystem scoping of the mount — Change #65. Disk *quota* here is host-level; per-tenant FS *isolation* is #65.
- The k8s handler and Helm chart that consume this model — Changes #75 and #76. This change defines the host-side enforcement and the config seam they map onto.
- The microVM handler's own resource model — a follow-on to ADR-0044's microVM note.

## Open questions

<!-- Groomed into a build-ready spec later. -->
- Default caps for the hosted profile (memory/cpu/pids/disk) that are safe without being uselessly small — and whether locally the default is "unlimited" (matching allow-all-locally) or a generous cap.
- Where the concurrency ceiling lives: a new semaphore in `sandbox.Pool`, or admission through the existing ADR-0007 Scheduler (which already owns subagent admission/throughput) so there is one admission authority, not two.
- Refuse-vs-queue-with-deadline when the ceiling is hit, and how that surfaces to the model (a tool error the model can react to vs. transparent backpressure).
- Whether the per-tenant cap keys on `Principal.Tenant` (consistent with #65/#34) and how it interacts with the single-warm-per-principal pool rule.
- The right bounded metric for limit-driven refusals, and whether it reuses #74's `acquire_failed` health reason or a dedicated `fuse_sandbox_rejected_total` family.

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->
