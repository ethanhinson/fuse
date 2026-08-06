---
id: 39
slug: agents-tab-idle-timer-and-model
title: Agents tab — timer runs before the first prompt; shows the default model, not the selected one
status: in-progress
priority: high
type: fix
created: 2026-08-06
updated: 2026-08-06
depends_on: []
related: [12, 35]
discovered_from: []
adrs: []
spec:
plan:
results:
trivial: true
auto_groomable:
branch: feat/agents-tab-idle-timer-and-model
claimed_at: 2026-08-06T07:00:33Z
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
<!-- docket:artifacts:end -->

## Why

Two visible correctness bugs on the agents tab (shipped in change 0012) make its
status display misleading before and across turns:

1. **The elapsed timer starts at shell startup, not at the first prompt.**
   `NewAgentTree()` stamps the root node with `StartedAt: time.Now()` at tree
   initialization (`internal/agent/tree.go:240`), so opening the agents tab
   before asking anything shows a running timer measuring time-since-launch.
   `BeginTurn()` already resets `root.StartedAt` correctly on prompt submit
   (`internal/agent/tree.go:296`) — only the pre-first-turn state is wrong.

2. **The model label is frozen at the default.** The root node's `Label`/`Model`
   are set once from the startup alias (`cmd/fuse/shell.go:133`) and never
   updated when `/model` switches the session alias
   (`internal/tui/shell_model.go:743`), so the tab always renders the initial
   model (`internal/tui/agents_model.go:307`) regardless of the current
   selection. Change 0035 established the pattern that session-level settings
   should be read live, not snapshotted at construction — the model label has
   the same snapshot bug.

## What changes

- Root node `StartedAt` stays zero until the first `BeginTurn()`; the agents
  tab renders no elapsed time (idle state) for a node with a zero `StartedAt`.
- The agents tab shows the currently selected model: either the `/model`
  handler updates the tree root's label/model on switch, or the tab reads the
  live session alias — whichever matches the live-read pattern from 0035 most
  cleanly.

## Out of scope

- Any change to per-subagent timers or labels for spawned child nodes — they
  are stamped at spawn time, which is correct.
- Mid-turn model switching semantics (which model a running turn actually
  uses) — this is display-only.
- Broader agents-tab UX rework.

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->
