---
id: 33
slug: spawn-tool-stripping
title: Strip spawn_agent from tool schemas at the concurrency cap and on budget exhaustion
status: in-progress
priority: high
type: feat
created: 2026-08-06
updated: 2026-08-06
depends_on: []
related: [24, 26]
discovered_from: []
adrs: []
spec: docs/superpowers/specs/0033-spawn-tool-stripping.md
plan:
results:
trivial: false
auto_groomable:
branch: feat/spawn-tool-stripping
pr:
claimed_at: 2026-08-06T18:14:02Z
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [0033-spawn-tool-stripping.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/0033-spawn-tool-stripping.md) |
<!-- docket:artifacts:end -->

## Why

When spawn limits refuse a `spawn_agent` call, the model has already paid for the
delegation: the task prompts are written, the permission gate has approved the calls, and
the refusal error lands after the fact. Observed live in a deepseek-flash research run: 22
spawns approved by the gate then killed by the exhausted 16-spawn budget, leaving the
parent agents to do 22 children's worth of file reading inline in their own context
windows — the worst of both worlds for context isolation, plus a confusing permission log
full of "allowed" spawns that never existed. Every peer harness that bounds spawning
(Claude Code, Grok Build) removes the spawn tool from the model's schema at the limit
instead of erroring at call time, so the model plans inline work from the start.

## What changes

Strip `spawn_agent` from per-turn tool schemas when a brake is engaged, restore it when
the brake releases:

- **Active cap (reversible):** raise `MaxConcurrentSpawns` 8 → 16, make it configurable
  (`agents.max_concurrent`); while running+pending children ≥ cap, the tool vanishes from
  every agent's next turn and returns as children exit.
- **Lifetime budget (permanent):** raise default `agents.max_spawns` 16 → 64; at
  exhaustion the tool is stripped for the rest of the session.
- **Depth (static):** a child at `MaxDepth` never gets the tool in its registry.
- Call-time errors, budget-line injection, queue/yield semantics all stay as backstops.

Design detail and acceptance criteria in the linked spec.

## Out of scope

- Refusing instead of queueing over-batched spawns; the pending queue stays.
- Tree-UI ghost nodes for refused spawns; permission-gate short-circuiting.
- Making `MaxDepth` configurable.

## Reconcile log
