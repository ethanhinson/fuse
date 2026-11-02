---
id: 115
slug: 'pipeline-steps-name-a-role-roles-in-the-synthesis-palette'
title: 'Pipeline steps name a role; roles in the synthesis palette'
status: 'in-progress'
priority: 'low'
type: 'feat'
deliverable: 'pr'
created: '2026-11-02'
updated: '2026-11-02'
depends_on: [114]
stacked_on:
related: [113]
discovered_from: []
adrs: []
spec: 'docs/superpowers/specs/2026-11-02-pipeline-steps-name-a-role-roles-in-the-synthesis-palette-design.md'
plan: 'docs/superpowers/plans/2026-11-02-pipeline-steps-name-a-role-roles-in-the-synthesis-palette.md'
results:
auto_groomable:
hold:
branch_prefix:
branch: 'feat/pipeline-steps-name-a-role-roles-in-the-synthesis-palette'
pr:
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-11-02-pipeline-steps-name-a-role-roles-in-the-synthesis-palette-design.md](../../superpowers/specs/2026-11-02-pipeline-steps-name-a-role-roles-in-the-synthesis-palette-design.md) |
| Plan | [2026-11-02-pipeline-steps-name-a-role-roles-in-the-synthesis-palette.md](../../superpowers/plans/2026-11-02-pipeline-steps-name-a-role-roles-in-the-synthesis-palette.md) |
<!-- docket:artifacts:end -->

## Why

Pipeline steps can name a workflow worker, but not a role. Once spawn_agent picks roles (0114), a self-written pipeline should be able to assign a step to a defined role, and synthesis should know the roles exist.

## What changes

- Pipeline steps gain an optional `role` field.
  - Naming both `role` and `worker`, or an unknown role, is a parse error.
  - Unlike worker steps, role steps are allowed outside a workflow.
- A role step builds its child with the same resolution as spawn_agent.
- The role palette joins pipeline synthesis, and the pipeline_run tool description mentions `role` when roles exist.
- With no roles defined, nothing changes.

## Out of scope

Pipeline scheduling or limit changes. Making roles mandatory in pipelines.

## Reconcile log

### 2026-11-02

Unchanged against main 15d827c (0114 merged; reuse its role resolution in cmd/fuse/workflow.go). Fold in 0114 reviewer nit: spawn_agent role param description notes that with workflow, the role must be one that workflow offers.
