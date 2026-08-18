---
id: 71
slug: turn-scoped-trace-roots-interactive-loops
title: Turn-scoped trace roots for interactive loops — end loop.run at first park, per-turn root spans
status: in-progress
priority: high
type: fix
created: 2026-08-18
updated: 2026-08-18
depends_on: []
related: [51, 54, 62]
discovered_from: [62]
adrs: [40, 45]
spec: docs/superpowers/specs/2026-08-18-turn-scoped-trace-roots-design.md
plan:
results:
trivial: false
auto_groomable:
branch: feat/turn-scoped-trace-roots-interactive-loops
claimed_at: 2026-08-18T01:36:58Z
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-18-turn-scoped-trace-roots-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-18-turn-scoped-trace-roots-design.md) |
| ADRs | [ADR-0040](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0040-provider-neutral-composite-observer.md), [ADR-0045](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0045-conversational-turn-attribution-by-timestamp-bucketing.md) |
<!-- docket:artifacts:end -->

## Why

A persistent interactive loop (change #54's session detach) parks between turns and
lives until the idle reaper — but its `fuse.loop.run` root span only exports at
`End()`. Observed live while testing #62's wander demo: every child span of the
session lands in Tempo while the root does not exist there, so the Traces Drilldown
root-span views exclude the whole trace, search shows an unknown root, duration is
meaningless, and an unclean process death loses the root forever. Session-lifetime
root spans are a known OTEL anti-pattern; the fix is bounding roots to units of work
(turns), which also makes turn boundaries visible in traces for the first time.

## What changes

Per the linked spec: interactive loops end `loop.run` at first park (covering startup
plus the first turn); every subsequent Send→park cycle becomes a `fuse.loop.turn`
**root** span linked to the session's `loop.run` context (the existing delayed-carrier
idiom) with `loop_id`/`tenant`/`fuse.turn.index` attributes; resume emits linked turn
roots without restarting `loop.run`; every teardown path defensively ends open spans.
One-shot trace shape stays byte-identical.

## Out of scope

- ADR-0045's TUI turn attribution (timestamp bucketing) — a possible follow-up once
  the turn span proves out.
- Metrics/logging projections, Tempo/Grafana config, SDK surface.

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->
