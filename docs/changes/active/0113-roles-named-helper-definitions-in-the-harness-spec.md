---
id: 113
slug: 'roles-named-helper-definitions-in-the-harness-spec'
title: 'Roles: named helper definitions in the harness spec'
status: 'proposed'
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
| Spec | [2026-11-02-roles-named-helper-definitions-in-the-harness-spec-design.md](../../superpowers/specs/2026-11-02-roles-named-helper-definitions-in-the-harness-spec-design.md) |
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
