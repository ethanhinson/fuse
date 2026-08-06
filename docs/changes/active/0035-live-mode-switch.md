---
id: 35
slug: live-mode-switch
title: Mode switch must bite mid-turn — gates read the SessionMode holder live, not a construction snapshot
status: proposed
priority: critical
type: fix
created: 2026-08-06
updated: 2026-08-06
depends_on: []
related: [17]
discovered_from: [17]
adrs: [5, 6]
spec:
plan:
results:
trivial: true
auto_groomable:
branch:
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
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
