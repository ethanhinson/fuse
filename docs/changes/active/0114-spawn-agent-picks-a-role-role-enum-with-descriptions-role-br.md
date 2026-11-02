---
id: 114
slug: 'spawn-agent-picks-a-role-role-enum-with-descriptions-role-br'
title: 'spawn_agent picks a role: role enum with descriptions, role brief composed into the child prompt'
status: 'implemented'
priority: 'high'
type: 'feat'
deliverable: 'pr'
created: '2026-11-02'
updated: '2026-11-02'
depends_on: [113]
stacked_on:
related: [112]
discovered_from: []
adrs: []
spec: 'docs/superpowers/specs/2026-11-02-spawn-agent-picks-a-role-role-enum-with-descriptions-role-br-design.md'
plan: 'docs/superpowers/plans/2026-11-02-spawn-agent-picks-a-role-role-enum-with-descriptions-role-br.md'
results: 'docs/results/2026-11-02-spawn-agent-picks-a-role-role-enum-with-descriptions-role-br-results.md'
auto_groomable:
hold:
branch_prefix:
branch: 'feat/spawn-agent-picks-a-role-role-enum-with-descriptions-role-br'
pr: 'https://github.com/ethanhinson/fuse/pull/128'
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-11-02-spawn-agent-picks-a-role-role-enum-with-descriptions-role-br-design.md](../../superpowers/specs/2026-11-02-spawn-agent-picks-a-role-role-enum-with-descriptions-role-br-design.md) |
| Plan | [2026-11-02-spawn-agent-picks-a-role-role-enum-with-descriptions-role-br.md](../../superpowers/plans/2026-11-02-spawn-agent-picks-a-role-role-enum-with-descriptions-role-br.md) |
| Results | [2026-11-02-spawn-agent-picks-a-role-role-enum-with-descriptions-role-br-results.md](../../results/2026-11-02-spawn-agent-picks-a-role-role-enum-with-descriptions-role-br-results.md) |
<!-- docket:artifacts:end -->

## Why

Change 0113 lets a spec define roles, but the runtime ignores them. Delegation should be selection, not invention: the parent picks a role by its description, and the runtime builds the child from the role's definition (prompt, tools, model).

## What changes

- `spawn_agent` gains an optional `role` parameter: an enum of role names, with `name — description` lines in its description.
  - Outside a workflow, every role is offered. Inside one, the workflow's listed roles are offered.
- A role brief is composed into the child's system prompt in the slot the harness instructions take for the root.
- Role tools: a non-empty list becomes the child's whole tool set and can only be narrowed; an empty list inherits the parent's tools.
- Model precedence: the call's model, then the role's model, then the parent's.
- Refused: `role` with `system_prompt`; `role` with `worker`; an unknown role.
- The role is resolved the same way at all five child-builder sites.
- The role name appears on the agent tree node, the `spawn.start` event and the span.
- With no roles defined, the schema and all prompts are byte-identical to today's.

## Out of scope

Pipeline steps naming a role. Per-role limits. A strict "must use a role" mode or a prompt-override flag. Any change to worker semantics or the research workflow.

## Reconcile log

### 2026-11-02

Unchanged against origin/main 90f185e (0113 merged). Spec anchors verified: the five child-builder sites (runtime_binding.go 1-3 of 4, loop_server.go 4 of 4, shell.go stale 2 of 3), checkRequest/prepareChild/resolveWorkerTools and childPlan.bound in workflow.go all present as described.

## Build evidence

<!-- docket:build-evidence:start -->
command:  make test
result:   green
head_sha: d6ebe753fe3e7d6623dd3a62447bd31e26158e70
ran_at:   2026-11-02T08:01:11Z
<!-- docket:build-evidence:end -->
