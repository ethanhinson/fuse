---
id: 116
slug: 'roles-and-workers-can-be-tool-less'
title: 'Tool-less loops, roles and workers'
status: 'done'
priority: 'high'
type: 'feat'
deliverable: 'pr'
created: '2026-11-03'
updated: '2026-11-03'
depends_on: []
stacked_on:
related: [113, 114, 115]
discovered_from: []
adrs: []
spec: 'docs/superpowers/specs/2026-11-03-roles-and-workers-can-be-tool-less-design.md'
plan: 'docs/superpowers/plans/2026-11-03-roles-and-workers-can-be-tool-less.md'
results: 'docs/results/2026-11-03-roles-and-workers-can-be-tool-less-results.md'
auto_groomable:
hold:
branch_prefix:
branch: 'feat/roles-and-workers-can-be-tool-less'
pr: 'https://github.com/ethanhinson/fuse/pull/132'
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-11-03-roles-and-workers-can-be-tool-less-design.md](../../superpowers/specs/2026-11-03-roles-and-workers-can-be-tool-less-design.md) |
| Spec (merged) | [2026-11-03-roles-and-workers-can-be-tool-less-design.md](https://github.com/ethanhinson/fuse/blob/main/docs/superpowers/specs/2026-11-03-roles-and-workers-can-be-tool-less-design.md) |
| Plan | [2026-11-03-roles-and-workers-can-be-tool-less.md](../../superpowers/plans/2026-11-03-roles-and-workers-can-be-tool-less.md) |
| Results | [2026-11-03-roles-and-workers-can-be-tool-less-results.md](../../results/2026-11-03-roles-and-workers-can-be-tool-less-results.md) |
<!-- docket:artifacts:end -->

## Why

A role's `tools` list carries two meanings in one field. A non-empty list is the child's whole, bound tool set; an empty list means "inherit the parent's tools". So there is no way to define a helper that should only think and write, such as a drafter, summarizer, classifier or judge. The narrowest set you can express is one real tool, which widens the helper's blast radius and tempts the model to use a tool it was never meant to.

## What changes

- Add an explicit tool-less form to `Role`, and to `Worker`, which follows the same rule. This could be a `no_tools: true` boolean, or one equivalent sentinel.
- Validation rejects it when a non-empty `tools` list is also set.
- `resolveRoleTools` maps it to an empty bound set, so a spawn-time `tools` request can only narrow to nothing.
- The `spawn_agent` and pipeline tool descriptions show that the helper is text-only.
- The blackboard tools are still always added, as for any bound child.
- The proto comments document the three states: inherit, narrowed and none.
- Tests: a validator test, and a spawn test asserting that the child is offered only the blackboard tools.

## Out of scope

Changing what an empty `tools` list means; it stays inherit.

## Reconcile log

### 2026-11-03

Unchanged against `origin/main` 374384f. No `no_tools` exists yet; #0130 (carried skills) and #0131 (CancelLoop) landed since the spec but touch neither tool composition nor role/worker resolution. Scope follows the linked spec, which supersedes this record's earlier note that tool-less children still get the blackboard: they get an empty registry.

## Build evidence

<!-- docket:build-evidence:start -->
command:  make test
result:   green
head_sha: 4859c4c8b86cad866bd96a4927e628003e50f7f1
ran_at:   2026-11-03T08:09:41Z
<!-- docket:build-evidence:end -->
