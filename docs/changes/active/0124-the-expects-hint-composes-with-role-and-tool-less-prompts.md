---
id: 124
slug: 'the-expects-hint-composes-with-role-and-tool-less-prompts'
title: 'The expects hint composes with role and tool-less prompts'
status: 'proposed'
priority: 'medium'
type: 'fix'
deliverable: 'pr'
created: '2026-11-03'
updated: '2026-11-03'
depends_on: []
stacked_on:
related: [114, 116]
discovered_from: [116]
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

When a spawn passes `expects`, the return_result hint is written into the child's system prompt as an explicit prompt. A role then refuses the spawn, because a role supplies the system prompt, so a role combined with `expects` is always refused. A tool-less worker given `expects` loses its tool-less persona line, and the hint tells it to use its other tools first.

## What changes

- Carry the return_result hint separately from an explicit `system_prompt`, for example as a hint-only `SpawnOpts` flag.
- Compose the hint after the role brief or the tool-less persona, instead of replacing them.
- Keep refusing an explicit `system_prompt` together with a role.
- Tests:
  - a role with `expects` is accepted, and its brief and the hint are both in the prompt;
  - a tool-less worker with `expects` keeps its persona line, and the hint doesn't mention other tools.

## Out of scope

Changing return_result itself.
