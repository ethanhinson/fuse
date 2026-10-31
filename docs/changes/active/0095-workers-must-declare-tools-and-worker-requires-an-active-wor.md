---
id: 95
slug: 'workers-must-declare-tools-and-worker-requires-an-active-wor'
title: 'Workers must declare tools, and worker requires an active workflow'
status: 'proposed'
priority: 'medium'
type: 'fix'
deliverable: 'pr'
created: '2026-10-31'
updated: '2026-10-31'
depends_on: []
stacked_on:
related: [90, 91]
discovered_from: [91]
adrs: [67]
spec: 'docs/superpowers/specs/2026-10-31-workers-must-declare-tools-and-worker-requires-an-active-wor-design.md'
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
| Artifact | Link |
|---|---|
| Spec | [2026-10-31-workers-must-declare-tools-and-worker-requires-an-active-wor-design.md](../../superpowers/specs/2026-10-31-workers-must-declare-tools-and-worker-requires-an-active-wor-design.md) |
| ADRs | [ADR-0067](../../adrs/0067-workflows-activate-per-spawn-do-not-nest-and-root-at-a-coord.md) |
<!-- docket:artifacts:end -->

## Why

Now that workflows activate in every host, two silent footguns reach every binding. A worker with an empty tools list inherits every tool, including spawn_agent, although the config schema promises that a worker whose allowlist omits spawn_agent cannot nest. And a spawn or pipeline step that names a worker outside any active workflow is silently ignored and runs with the full tool set. Neither produces an error the author can see.

## What changes

- Config and harness validation refuse a workflow worker that declares no tools, with a field path.
- The schema comment about nesting is corrected.
- A spawn or pipeline step naming `worker` with no active workflow is refused with an error that says to pass `workflow`.
- A hosted-loop test proves `pool.tokens` refuses a worker's model call once the workflow's token pool is spent.

## Out of scope

- A separate inherit-all flag for workers.
- New pool limits or scheduler policy.
