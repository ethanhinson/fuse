---
id: 112
slug: 'harness-instructions-a-free-text-task-brief-composed-into-th'
title: 'Harness instructions: a free-text task brief composed into the system prompt'
status: 'in-progress'
priority: 'high'
type: 'feat'
deliverable: 'pr'
created: '2026-11-01'
updated: '2026-11-01'
depends_on: []
stacked_on:
related: []
discovered_from: []
adrs: []
spec: 'docs/superpowers/specs/2026-11-01-harness-instructions-a-free-text-task-brief-composed-into-th-design.md'
plan:
results:
auto_groomable:
hold:
branch_prefix:
branch: 'feat/harness-instructions-a-free-text-task-brief-composed-into-th'
pr:
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-11-01-harness-instructions-a-free-text-task-brief-composed-into-th-design.md](../../superpowers/specs/2026-11-01-harness-instructions-a-free-text-task-brief-composed-into-th-design.md) |
<!-- docket:artifacts:end -->

## Why

A harness spec can choose a persona (`Model.persona`, a named built-in style such as `coding` or `research`) and a per-model `system_prefix` meant for model-specific directives (`/no_think`), but it has no field for what the agent is *for*. A harness meant to be a support assistant, a release-notes writer or a code reviewer cannot say so in its own spec. Authors either misuse `system_prefix`, which belongs to one model entry and is placed *before* the persona prompt, or rely on files in a working directory, which a spec-driven or hosted harness does not have. `agent.ComposeSystemPrompt` (`internal/agent/personas.go:41`) already has the slot (`extra`), but nothing in `HarnessSpec` feeds it.

## What changes

- `HarnessSpec.instructions` (`proto/fuse/harness/v1/harness.proto`, next free field number): a string with `buf.validate` max_len 16384. UI descriptor: label "Instructions", help "What this agent is for and how it should work. Added to the system prompt after the persona.", `WIDGET_TEXTAREA`.
- Composition: the main agent's system prompt becomes persona prompt → harness instructions → existing `extra` (project/context instructions), each separated by a blank line, with `system_prefix` still first. `ComposeSystemPrompt` gains the parameter, or the instructions are folded into `extra` ahead of the existing content; either way, one ordering is documented and tested.
- Plumbed everywhere a spec becomes a running agent: `internal/harnessconfig` (spec → config, and the `e.set` export path so round-trips keep it), the YAML config schema (`instructions:` top level), `configcheck`, the loop server's spec-driven loops, and the CLI-adapter path in `cmd/fuse/run.go` (both `ComposeSystemPrompt` call sites).
- Included in the spec/settings digest so changing instructions changes the digest.
- Workers/sub-agents keep their own prompts. Instructions apply to the main agent only, and the field's help text says so.
- Docs: harness spec reference and an example spec with instructions.

## Out of scope

Per-worker instructions, templated variables in instructions, and prompt-injection filtering of instructions (they are author-controlled, like the rest of the spec).

## Reconcile log

### 2026-11-01

Unchanged against main f24f0c1. ComposeSystemPrompt (internal/agent/personas.go) still has exactly two call sites, both in cmd/fuse/run.go buildAgentCore; the loop server reaches agents through that chokepoint. harness.proto has no instructions field yet. Plan must ensure worker/sub-agent builds do not receive the harness instructions.
