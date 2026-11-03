---
id: 122
slug: 'spawn-start-names-the-worker-a-child-was-bound-to'
title: 'spawn.start names the worker a child was bound to'
status: 'proposed'
priority: 'low'
type: 'feat'
deliverable: 'pr'
created: '2026-11-03'
updated: '2026-11-03'
depends_on: []
stacked_on:
related: [114, 115]
discovered_from: []
adrs: []
spec:
plan:
results:
auto_groomable:
hold:
branch_prefix:
branch:
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
<!-- docket:artifacts:end -->

## Why

fuse's `spawn.start` event names the role a child was started as, but not the worker it was bound to. A consumer of the event stream (a console, the TUI, the session-log projection) therefore cannot say which worker a child ran as. It can only reconstruct that from the parent's preceding `spawn_agent` `tool.call` arguments, which is fragile. For children a workflow step starts, no tool call precedes them, so it is impossible.

## What changes

- Add an optional `worker` field to `SpawnStartPayload`. `emitSpawnStart` fills it from the child node's bound worker, the same way it fills `role` today.
- The change is additive and backward compatible: existing readers ignore the field.
- Tests for a spawn_agent child bound to a worker and for a pipeline-step child.

## Out of scope

Changing how workers are resolved.
