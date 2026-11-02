---
id: 113
slug: 'roles-named-helper-definitions-in-the-harness-spec'
title: 'Roles: named helper definitions in the harness spec'
status: 'implemented'
priority: 'high'
type: 'feat'
deliverable: 'pr'
created: '2026-11-02'
updated: '2026-11-02'
depends_on: []
stacked_on:
related: []
discovered_from: []
adrs: []
spec: 'docs/superpowers/specs/2026-11-02-roles-named-helper-definitions-in-the-harness-spec-design.md'
plan: 'docs/superpowers/plans/2026-11-02-roles-named-helper-definitions-in-the-harness-spec.md'
results: 'docs/results/2026-11-02-roles-named-helper-definitions-in-the-harness-spec-results.md'
auto_groomable:
hold:
branch_prefix:
branch: 'feat/roles-named-helper-definitions-in-the-harness-spec'
pr: 'https://github.com/ethanhinson/fuse/pull/127'
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-11-02-roles-named-helper-definitions-in-the-harness-spec-design.md](../../superpowers/specs/2026-11-02-roles-named-helper-definitions-in-the-harness-spec-design.md) |
| Plan | [2026-11-02-roles-named-helper-definitions-in-the-harness-spec.md](../../superpowers/plans/2026-11-02-roles-named-helper-definitions-in-the-harness-spec.md) |
| Results | [2026-11-02-roles-named-helper-definitions-in-the-harness-spec-results.md](../../results/2026-11-02-roles-named-helper-definitions-in-the-harness-spec-results.md) |
<!-- docket:artifacts:end -->

## Why

A harness spec cannot say who a subagent is. Instructions brief only the main agent. A workflow Worker has tools and a model, but no prompt or description, and exists only inside a workflow. Outside workflows, the parent model invents each child's system prompt. Spec authors need reusable, named helper definitions such as "reviewer" or "researcher".

## What changes

- Add `HarnessSpec.roles`: a map of `Role` entries, each with a description, instructions, an optional tool set (empty means inherit) and an optional model.
- Add `Workflow.roles`, naming which roles may take part in a workflow.
- Carry both through the YAML config and harnessconfig, so they round-trip.
- Validation: lengths, the count cap and the key pattern; every `Workflow.roles` name must resolve to a defined role.
- The spec digest covers roles; an empty `roles` leaves the digest unchanged.
- README section and an example spec.

## Out of scope

Runtime use of roles (spawn_agent and pipelines are follow-ups). Deprecating workers. Changing the built-in research workflow. Per-role limits. Validating role tool names or model aliases.

## Reconcile log

### 2026-11-02

Unchanged against main 58f1e92: HarnessSpec field 15 and Workflow field 4 still free; no roles concept exists yet.

## Build evidence

<!-- docket:build-evidence:start -->
command:  make test
result:   green
head_sha: 23081739d9254e701283e2d7d0b7fcb43ee9d20a
ran_at:   2026-11-02T05:59:08Z
<!-- docket:build-evidence:end -->
