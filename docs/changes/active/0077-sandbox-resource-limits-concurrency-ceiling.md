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
related: [63, 64, 74, 75, 76]
discovered_from: [63]
adrs: [44]
spec: docs/superpowers/specs/2026-08-20-sandbox-resource-limits-concurrency-ceiling-design.md
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
| Spec | [2026-08-20-sandbox-resource-limits-concurrency-ceiling-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-20-sandbox-resource-limits-concurrency-ceiling-design.md) |
| ADRs | [ADR-0044](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0044-bash-tool-contained-not-credentialed.md) |
<!-- docket:artifacts:end -->

## Why

The #63 container substrate handles container **lifetime and cleanup** well — `docker run --rm` per `Exec` is ephemeral and self-cleaning, the warm pool keeps at most one idle context per `Principal` and idle-reaps it after `DefaultIdleTTL` (5m), and `Close` tears everything down deterministically. But it does **no resource *limiting*, and that is the DoS surface**:

- **No cgroup caps on the container.** The run argv (`internal/tools/sandbox/container.go`) is `run --rm -i --env… -v… -w… image sh -c …` with **no `--memory`, `--cpus`, `--pids-limit`, or `--ulimit`**. A model-authored command can fork-bomb, exhaust host RAM, spin CPU, or fill the mount up to whatever the host allows. There is a `TODO(#0064)` for `--network` but **no owner at all** for compute/memory/pid/disk limits.
- **No ceiling on concurrent `Exec`s.** The per-principal "max one warm" rule bounds *idle* containers only; nothing bounds *in-flight* commands. N concurrent loops — or one loop issuing rapid commands — spawn N concurrent `docker run`s with no cap and no queue (the pool has no semaphore; `grep` for max/limit/semaphore finds only the reap-interval clamp).
- **Unbounded inline image pull.** A cold image pays an unbounded `pull` under the command's own deadline on first `Exec`.

This is the resource-management layer every orchestration target depends on: it is what a k8s handler (#75) expresses as Pod `resources.limits` + a per-tenant `ResourceQuota`, and what the Helm chart (#76) needs a config seam for. Doing it once, host-side and behind config, gives both a uniform model to map onto. It is filed `high` because it is a live denial-of-service hole in a shipped feature, not a nice-to-have.

## What changes

Four axes, settled in grooming (design detail in the linked spec):

- **Per-container cgroup caps** on the container substrate: `--memory` (with `--memory-swap` pinned equal), `--cpus`, `--pids-limit`, and a `--ulimit` bound, added to the run argv, sourced from **trusted operator config only** (never model output, never a wire field — same trust boundary as the off-switch and env-allowlist). **Fail-safe posture split:** caps default *off* locally (matching #63's allow-all-locally stance) and *on* when hosted, so an unconfigured hosted profile is still bounded. An unset limit emits no flag.
- **A soft, high-bounded concurrency queue on in-flight `Exec`s** — "the queue is the queue." Execs *wait* for a slot up to their own context deadline and the queue drains naturally; the bound is a **runaway backstop**, refusing only in genuinely pathological cases, never under normal load. Scoped as a **per-`Principal.Tenant` soft share under a high global backstop** (a tenant's burst queues within its own share; others keep flowing). Admission lives in a **dedicated sandbox-layer gate owned by the process-scoped `Service`** — *not* `agent.Scheduler` (spawn-tree-scoped and agent-coupled; the sandbox is deliberately agent-free) and *not* the per-loop `Pool` (which would bound nothing host-wide).
- **A loop- and operator-facing backpressure signal.** When an `Exec` waited past a threshold, a **bounded, closed-form note** is appended to its `Output` the model can read and adapt to; queue depth/wait also emit a sandbox lifecycle event projected to Prometheus (`fuse_sandbox_exec_queued_total`, a wait histogram), and capacity refusals land on a **dedicated `fuse_sandbox_rejected_total`** — deliberately *not* reusing #74's `acquire_failed` health reason, so #77 carries no hard dependency on #74 and a load event never pages an operator watching for a broken substrate.
- **Bounded image acquisition**: a single-flight pre-pull under its own timeout, with `--pull=never` on the run, so a cold image cannot hang an `Exec` on the command's own deadline.

## Out of scope

- The network/egress floor — Change #64 (`--network none` + allowlist). This change owns compute/memory/pid/disk and concurrency, not network.
- Per-tenant filesystem scoping of the mount — Change #65. Disk *quota* here is host-level; per-tenant FS *isolation* is #65.
- The k8s handler and Helm chart that consume this model — Changes #75 and #76. This change defines the host-side enforcement and the config seam they map onto.
- The microVM handler's own resource model — a follow-on to ADR-0044's microVM note.

## Open questions

<!-- All grooming-time questions were resolved into the linked spec (2026-08-20). Remaining unknowns are build-time details the implementer's reconcile pass settles against merged reality: -->
- Concrete default values (the hosted-profile cap numbers, the global/per-tenant queue bounds, the `max_queued` refusal bound, the pull timeout) — invented in the spec as starting points, to be tuned at build time.
- Exactly which run stage the pre-pull hooks (handler `Acquire` vs. first `Exec`), pending the merged #63 substrate's final shape.

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->
