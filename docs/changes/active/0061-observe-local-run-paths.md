---
id: 61
slug: observe-local-run-paths
title: Wire observability into local run paths (fuse shell + one-shot + runtime bindings)
status: proposed
priority: medium
type: feat
created: 2026-08-13
updated: 2026-08-13
depends_on: []
related: [51]
discovered_from: [51]
adrs: [40]
spec: docs/superpowers/specs/2026-08-13-observe-local-run-paths-design.md
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
| Spec | [2026-08-13-observe-local-run-paths-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-13-observe-local-run-paths-design.md) |
| ADRs | [ADR-0040](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0040-provider-neutral-composite-observer.md) |
<!-- docket:artifacts:end -->

## Why

Change #0051 built the loop observability stack (OTEL traces, Prometheus metrics,
structured logs) as a projection over the event stream, but only wired it into a
single entry point: the `fuse loop-serve-net` networked server. Every other way
of running `fuse` — including `fuse shell` and one-shot `fuse <task>` — builds
agents on `observe.NoopObserver{}` and emits nothing, regardless of config.

Running `fuse shell` with observability configured therefore shows no telemetry.
That is a real gap for local dogfooding of the stack #0051 built. This change
closes it by extending the observer wiring to the local run paths.

## What changes

Piece 1 of a two-piece plan: thread a single, session-shared `observe.Observer`
into every locally-built agent (traces + operation metrics), defaulting to
`NoopObserver{}` for callers that don't opt in.

- Add an observer parameter to `buildAgentCore` / `buildAgentWithRendererAndTrace`
  (`cmd/fuse/run.go`) and call `agent.WithObserver(observer)` on the built agent.
- In `fuse shell` (`runShell`), construct the observe layer once via
  `newObservability(...)` and thread the shared observer through the `build`
  closure into root + every child agent; start the metrics endpoint when
  `metrics.bind` is set (nil verifier; `metrics.access: public` acceptable for a
  local `127.0.0.1` bind).
- Give the one-shot `fuse <task>` path the same treatment through the shared
  build seam.
- Replace the `observe.NoopObserver{}` hardcodes in `runtime_binding.go`
  (~lines 87, 304, 539) with the config-constructed observer.

Settled behavior: metrics-endpoint bind failure warns and continues; invalid
observability config fails shell startup fast; one observer instance is shared
across the session. Telemetry stays gated behind the existing config opt-ins
(`observability.metrics.enabled` / `traces.enabled` / `logging.enabled`).

## Out of scope

- **Piece 2** (explicit follow-up): payload-free JSON access-log projection over
  the shell's event stream. The shell uses the legacy `EventStore` interface, not
  the tenant-scoped `DurableStore`/`CommittedDurableStore` the serve path's
  `projectingDurableStore` / `observe.Runner` build on, so those helpers can't be
  reused verbatim — Piece 2 must mirror `startProjectedLogConsumer`.
- Any bearer-token / operator auth for the shell metrics endpoint (local,
  public-on-loopback only).
- Changes to the already-wired `loop-serve-net` path.
- Trace-carrier-on-the-wire for remote clients (by design in #0051; not a bug).

## Open questions

- Confirm during reconcile whether one-shot `fuse <task>` already flows through
  the same `buildAgentCore` helper, or needs its own wiring site.

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->
