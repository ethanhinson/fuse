---
id: 74
slug: sandbox-health-emitter
title: sandbox health emitter — feed KindSandboxHealth so fuse_sandbox_unhealthy_total stops being defined-but-unfed
status: deferred
priority: medium
type: feat
created: 2026-08-20
updated: 2026-09-04
depends_on: [63]
related: [63, 65, 77]
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

Change #63 defined the fourth sandbox metric family, `fuse_sandbox_unhealthy_total`, its projection (`internal/observe/projector.go` `decorateSandbox`, `internal/observe/prometheus/recorder.go` `case "sandbox.health"`), the `event.KindSandboxHealth` kind, the `event.SandboxHealthPayload` (closed reason enum `oom | runtime_exit | pull_failed | acquire_failed | unresponsive | recovered`), and an alert rule — **but no code path in the running system ever emits the event.** `grep` for `SandboxHealthPayload` / `KindSandboxHealth` / `Healthy:` finds only the type, the projector, and the recorder — zero producers. The metric and its alert are therefore live-but-always-zero.

The #63 end-to-end verification (`internal/tools/sandbox_metrics_e2e_test.go`, `TestContainerLifecycleFeedsSandboxMetricsEndToEnd`) proved the other three families (`fuse_sandbox_acquire_total`, `fuse_sandbox_cold_start_seconds`, `fuse_sandbox_reaped_total`, plus the `fuse_sandbox_active` leak-guard gauge) against a real container end-to-end, and **pins this gap**: it asserts `fuse_sandbox_unhealthy_total` gains no series from a real run, so the day an emitter lands that guard fails on purpose — forcing this change to extend the E2E coverage rather than ship a silently-unproven metric.

This is deliberately its own change because "close the gap" is not a wire-up — it is an unbuilt design decision (see Open questions). The #63 close-out flagged it as "a health signal for `KindSandboxHealth` is its own change."

## What changes

- **A real producer of `event.KindSandboxHealth`** at whichever call sites can honestly observe a health transition, emitting through the existing seam (`internal/tools/sandbox_events.go` `SandboxEventHooks`, or a sibling hook) so the payload-free discipline and the loop's own `StreamKey`/`NodeID` envelope are preserved exactly as the acquire/release/reap hooks already are.
- **Reason classification** that maps observed failures onto the closed enum without ever putting raw error text, command, or environment into the payload — the same closed-enum discipline `sandboxCause` already enforces for release/reap causes.
- **Extend `TestContainerLifecycleFeedsSandboxMetricsEndToEnd`** (or a sibling) to drive at least one real unhealthy transition and assert `fuse_sandbox_unhealthy_total{...}` moves on the live `/metrics` scrape, and flip the current negative-assertion guard that pins the family as unfed.

## Out of scope

- The three families already proven end-to-end by #63.
- The persistent-container / per-tenant substrate work — Change #65. The `unresponsive` and `recovered` reasons presuppose a long-lived container with a health probe; the current substrate is stateless-per-`Exec` (`docker run --rm` per command, no container outlives an `Exec`, `ContainerID` always `""`), so those two reasons are **not** observable until #65's substrate exists. This change should scope to the reasons observable on the current substrate and defer the prober-dependent ones to #65 (or a follow-on), rather than inventing a prober here.
- Any emit inserted at an arbitrary point purely to make the metric non-zero — a fabricated health signal is worse than an empty one, because an operator would trust it.

## Open questions

<!-- Groomed into a build-ready spec later. -->
- **Which reasons are honestly reachable on the current (stateless-per-Exec) substrate?** `acquire_failed` / `pull_failed` are the closest — the `docker run` / pull error is right there in `containerRunner.Exec` / `runClientCommand` — but the payload wants a `ContainerID` this design never produces, and it must be decided whether an image-pull error is a *health transition* vs. a transient acquire error already surfaced to the caller through `Output{ExitCode:-1}`.
- **`oom` / `runtime_exit` classification:** distinguishing "the container was OOM-killed / the runtime exited non-zero" (exit 137, runtime-level failure) from "the command itself exited non-zero" (a user `grep` that found nothing) requires a classifier over `docker run` exit codes / stderr that does not exist today. Is that classifier in scope here, or does it belong with #65?
- **`unresponsive` / `recovered`:** confirm these are deferred to #65 (they need a persistent container + health-probe loop), and that this change explicitly does not stub a prober.
- Whether the emitter is a new field on `sandbox.PoolHooks` or a separate seam, given health is not a pool checkout/hand-back/reap event.

## Why deferred

**2026-09-04 (groom).** Deferred until #65 lands, on the grounds that the emitter
cannot be built honestly twice.

Of `SandboxHealthPayload`'s six closed reasons, only two — `pull_failed` and
`acquire_failed` — are observable on today's stateless-per-`Exec` substrate
(`docker run --rm` per command, nothing outliving an `Exec`). `oom` and
`runtime_exit` need an exit-code classifier that does not exist, and
`unresponsive` / `recovered` presuppose a long-lived container with a health
probe, which #65's per-tenant persistent substrate is what creates. Building an
acquire-path-only emitter now would mean designing the seam against two reasons
and then re-opening it for the other four — and would leave
`fuse_sandbox_unhealthy_total` near-zero in normal operation, proving the path
without earning its alert.

The #63 close-out's E2E tripwire (`internal/tools/sandbox_metrics_e2e_test.go`,
the negative assertion that the family gains no series) is the mechanism that
keeps this gap visible: it stays armed and keeps failing on purpose the moment
any emitter lands, so deferring loses no safety.

`ContainerID` rides along to the same place — see the note below.

## Revive when

#65 (`bash per-tenant filesystem isolation`) is `done` — at that point a
persistent, tenant-scoped container exists, all six reasons become observable at
once, and `ContainerID` is a real value rather than `""`.

**2026-09-04 update (groom of #65).** #65's spec now takes the emitter into its
own scope (Decision 3): it populates `ContainerID`, builds the exit-code
classifier for `oom`/`runtime_exit`, and flips #63's E2E tripwire. If #65 lands
that in full, this change closes as **done-by-#65** rather than reviving. It
stays open only for the residual case #65's spec names explicitly: if #65 ships
per-tenant mounts *without* a long-lived container, `unresponsive`/`recovered`
defer again and land here.

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->
