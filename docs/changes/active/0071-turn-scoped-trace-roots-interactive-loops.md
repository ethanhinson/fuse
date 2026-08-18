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
plan: docs/superpowers/plans/2026-08-18-turn-scoped-trace-roots-interactive-loops-plan.md
results:
trivial: false
auto_groomable:
branch: feat/turn-scoped-trace-roots-interactive-loops
claimed_at: 2026-08-18T01:43:21Z
pr:
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-18-turn-scoped-trace-roots-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-18-turn-scoped-trace-roots-design.md) |
| Plan | [2026-08-18-turn-scoped-trace-roots-interactive-loops-plan.md](https://github.com/ethanhinson/fuse/blob/feat/turn-scoped-trace-roots-interactive-loops/docs/superpowers/plans/2026-08-18-turn-scoped-trace-roots-interactive-loops-plan.md) |
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

### 2026-08-18 — reconcile at claim

Verdict: **scope-adjustable, no invalidation.** All four spec decisions (D1–D4) hold
against current `origin/main`. Verified against `origin/main` via `git show`, not the
local working tree (learnings: `reconcile-verify-claims-against-origin-not-working-tree`).

What was checked and what changed:

- **`related` are all closed.** 0051 (loop observability OTEL metrics), 0054 (durable
  resumable sessions), and 0062 (wander refresh-to-restore) are archived `done`. None of
  them built any part of this change. Change 0066 (agents-tab multiturn turn groups,
  2026-08-16) landed ADR-0045's TUI attribution — which **confirms** D4's out-of-scope
  call rather than invalidating it: the heuristic is now concrete at
  `internal/tui/agents_model.go:1468` (`turnIndexFor`), and migrating it onto the new
  turn span stays a separate follow-up.
- **The problem still exists exactly as described.** `launchLoop`
  (`internal/runtime/inproc.go:325`) starts `fuse.loop.run` at line 326 and ends it only
  from the run goroutine's completion path — so a parked interactive session's root span
  still does not export until the session dies.
- **The idiom D2 depends on is intact.** `Observer.StartFromCarrier(ctx, carrier,
  delayed=true, …)` (`internal/observe/otel/observer.go:37`) does precisely
  `trace.WithNewRoot()` + `trace.WithLinks(...)` at line 46, reached through the
  capability-probe helper `observe.StartFromCarrier` (`internal/observe/contracts.go:91`)
  which degrades to plain `Start` for observers that lack it. No new Observer surface is
  needed; ADR-0040 provider-neutrality is preserved.
- **No prior art to drop.** There is no `fuse.loop.turn` span, `turn.index` attribute, or
  turn-index concept anywhere in `internal/` today; the only `turnIndex*` matches are
  ADR-0045's TUI attribution, which is out of scope.
- **Spec detail corrected (added constraint).** The spec says the `loop.run` handle "is
  already idempotent via `sync.Once`". It is not: `launchLoop` guards the end with a
  plain `ended bool` captured in the `end(out)` closure (`inproc.go:327–334`), which is
  single-goroutine-safe only because today exactly one goroutine calls it. D1 adds a
  second caller (the park path) racing the run goroutine's completion, so the build MUST
  harden that guard (`sync.Once` or a mutex) rather than assume the existing
  idempotence. This is a mechanism correction, not a decision change.
- **D3's park/wake seam made concrete.** The park boundary is already announced inside
  the agent at `internal/agent/loop.go:598` — `event.KindLoopParked` on the interactive
  no-tool-calls terminal path, immediately before `humanInjector.Wait(ctx)`. The wake is
  the runtime's own `Send` (and `Resume`, which carries its message). Both points are
  visible to `internal/runtime` without adding a callback into `internal/agent`, so D3's
  "owned by the runtime/agent park-wake seam" resolves to those two seams; the plan picks
  between observing the parked event and adding an explicit runtime-side park hook, and
  must confirm the Send-side wake covers the resume path (learnings:
  `persistent-loop-needs-explicit-completion-event`).

Scope, acceptance criteria, and non-goals are otherwise unchanged. Auto-capture is
disabled for this repo, so no follow-up stubs were minted; the only adjacent work this
pass surfaced is the already-noted ADR-0045 migration, which the change body records as
out of scope.
