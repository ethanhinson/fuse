---
id: 24
slug: structured-delegation
title: Structured delegation — expected result schemas for spawn_agent
status: proposed
priority: medium
type: feat
created: 2026-08-06
updated: 2026-08-08
depends_on: [12]
related: [23, 26]
discovered_from: []
adrs: []
spec: docs/superpowers/specs/2026-08-08-structured-delegation-design.md
plan:
results:
trivial: false
auto_groomable:
branch:
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-08-structured-delegation-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-08-structured-delegation-design.md) |
<!-- docket:artifacts:end -->

## Why

`spawn_agent` (change 0012) returns free-text results — the child agent's final assistant message. The parent has no way to express "I need a structured result with specific fields" and no way to validate that the child produced what was asked for. The research skill works around this by embedding the expected report structure in the task prompt ("synthesize a single markdown report with [N] citation markers and a numbered source list"), but this is convention, not contract. Making the expected result shape explicit — via an optional `expects` parameter with a JSON Schema — would let the parent (a) tell the child what shape to produce, (b) get a parseable result back, and (c) detect when the child deviated from expectations. This is the foundation for reliable agent composition: a parent can call `spawn_agent` and destructure the result programmatically.

## What changes

- **`expects` on both surfaces**: an optional JSON-Schema object on the `spawn_agent` tool params
  (model-driven) and an `Expects` field on `SpawnOpts` (code-driven). Nil ⇒ behavior identical to
  today.
- **Producer-side injection**: when `expects` is set, the spawner augments the child's system
  prompt — "your final message MUST be a single JSON object conforming to this schema; output only
  the JSON."
- **Full JSON-Schema validation** of the child's output against `expects`, using a vendored
  JSON-Schema library (nested types, enums, formats) — not a shallow key check. JSON is extracted
  leniently first (strip fences/whitespace).
- **Structured handle**: `SpawnDone.Structured any` + `AgentHandle.Result() (any, error)` carry the
  validated value for programmatic callers. `Wait()` still returns raw text. (No consumer exists
  today — this is the foundation change 0026 consumes.)
- **Mismatch = degrade + surface + log**: an output that does not validate **never fails the
  spawn** — the raw text is returned, a `(did NOT match expected schema: <error>)` note is appended
  for the parent **model**, and the mismatch is recorded in a **labeled trace entry and an
  `AgentNode` event** (tree drilldown) for the human. A match appends `(matched expected schema)`
  and populates `Structured`.

## Out of scope

- A programmatic consumer of the structured result — none exists today; change 0026 is the first.
  This change ships the handle, not a consumer.
- Bounded re-ask of the child on mismatch — deferred (a second child model call per miss).
- Nested schemas for sub-sub-agents — each child negotiates its own contract.
- Result-schema propagation through the agent-tree display (beyond the mismatch event).
- `ensures` (parent-side post-delegation validation) — a possible follow-on.

## Design decisions

Design settled through an interactive brainstorm on 2026-08-08 and captured in the linked spec.
Four decisions fixed the shape: (1) **both** producer-side injection **and** a programmatic
`Structured`/`Result()` handle (a foundation ahead of its consumer, change 0026); (2) **full
JSON-Schema validation** via a vendored library (accepting the dependency for real fidelity); (3)
a mismatch **degrades to free text, surfaces a note the model reads, and is logged** to the trace
and an `AgentNode` event — never fails the spawn; (4) `expects` on **both** the `spawn_agent` tool
param and `SpawnOpts`. Two grooming findings shaped this: no Go/skill code consumes a structured
result today (research is skill-driven, ADR-0002), and no JSON-Schema library is vendored yet.
