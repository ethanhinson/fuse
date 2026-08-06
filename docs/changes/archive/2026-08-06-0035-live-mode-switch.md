---
id: 35
slug: live-mode-switch
title: Mode switch must bite mid-turn — gates read the SessionMode holder live, not a construction snapshot
status: done
priority: critical
type: fix
created: 2026-08-06
updated: 2026-08-06
depends_on: []
related: [17]
discovered_from: [17]
adrs: [5, 6]
spec:
plan: docs/superpowers/plans/2026-08-06-live-mode-switch-plan.md
results: docs/results/2026-08-06-live-mode-switch-results.md
trivial: true
auto_groomable:
branch: feat/live-mode-switch
pr: https://github.com/ethanhinson/fuse/pull/16
blocked_by:
reconciled: true
claimed_at: 
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Plan | [2026-08-06-live-mode-switch-plan.md](https://github.com/ethanhinson/fuse/blob/feat/live-mode-switch/docs/superpowers/plans/2026-08-06-live-mode-switch-plan.md) |
| Results | [2026-08-06-live-mode-switch-results.md](https://github.com/ethanhinson/fuse/blob/feat/live-mode-switch/docs/results/2026-08-06-live-mode-switch-results.md) |
| PR | [#16](https://github.com/ethanhinson/fuse/pull/16) |
| ADRs | [ADR-0005](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0005-per-segment-allow-rule-evaluation.md), [ADR-0006](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0006-fuse-local-yml-tighten-only-trust-boundary.md) |
<!-- docket:artifacts:end -->

## Why

0017's D10 shipped Shift+Tab / `/mode` switching, but every `PermissionGate`
snapshots the mode at construction (`WithMode(sessionGateMode(...))` in
`cmd/fuse/run.go`; `currentMode()` reads the gate's own field) and
`CloneForChild` snapshots again at spawn. The holder is only consulted when a
new gate is built at the next turn. The moment a human actually reaches for
Shift+Tab is *mid-turn* — a long agent run is prompting repeatedly and they
want it to stop asking NOW. Post-merge first use confirmed: flipping to auto
mid-run has no effect on the running turn or its children.

## What changes (design settled with the human, 2026-08-06)

- New gate option `WithSessionMode(*SessionMode)`: when the holder is set,
  `currentMode()` returns `holder.Get()` live — the snapshot field is the
  fallback for holderless gates (one-shot, mcp-server, tests). `SetMode`
  keeps working for holderless gates.
- `CloneForChild` propagates the holder, so running children follow the
  session mode live too (supersedes D10's "children keep their spawn mode").
- Valve semantics preserved across the new read path: the gate tracks the
  last observed mode under the existing `modeMu`; observing an auto →
  non-auto transition resets the escalation valve (same behavior SetMode
  implements today).
- Wiring: the shell path (`buildGate`/`autoModeOptions`) appends
  `WithSessionMode(sm)` when `sm != nil`; nil paths unchanged.
- Tests: mid-turn flip on a live gate (smart → prompt observed → flip holder
  to auto → same gate auto-approves read-only bash with no rebuild); a child
  cloned before the flip follows it; valve reset fires on the observed
  transition; holderless gates behave exactly as today.

## Out of scope

- One-shot and mcp-server switching surfaces (no switcher exists there).
- Any TUI change — Shift+Tab and `/mode` already write the holder.

## Reconcile log

### 2026-08-06

Verified against `main` (integration branch) at claim time. Ground truth
holds exactly as the body describes:

- `WithSessionMode(*SessionMode)` does **not** exist yet — the gate exposes
  only `WithMode(PermissionMode)` (`internal/permissions/gate.go`), and the
  shell path snapshots via `buildGate` → `WithMode(sessionGateMode(cfg, sm))`
  (`cmd/fuse/run.go:200-204`, `sessionGateMode` at 187-192).
- `currentMode()` reads `g.mode` under `modeMu` (gate.go:192-196); the
  snapshot field is the only mode source today.
- `CloneForChild` snapshots `g.currentMode()` into the child's own `mode`
  field (gate.go:450-455).
- The `SessionMode` holder (`internal/permissions/sessionmode.go`) and the
  valve reset on the auto→non-auto transition in `SetMode`
  (gate.go:221-230, `escalationValve.reset` at 127-132) are present.

No scope drift; no work already done elsewhere. Design settled with the human
2026-08-06 stands. `trivial: true` — no spec. Proceeding to plan.
