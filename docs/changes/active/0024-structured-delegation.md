---
id: 24
slug: structured-delegation
title: Structured delegation — expected result schemas for spawn_agent
status: proposed
priority: medium
type: feat
created: 2026-08-06
updated: 2026-08-06
depends_on: [12]
related: [23]
discovered_from: []
adrs: []
spec:
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
<!-- docket:artifacts:end -->

## Why

`spawn_agent` (change 0012) returns free-text results — the child agent's final assistant message. The parent has no way to express "I need a structured result with specific fields" and no way to validate that the child produced what was asked for. The research skill works around this by embedding the expected report structure in the task prompt ("synthesize a single markdown report with [N] citation markers and a numbered source list"), but this is convention, not contract. Making the expected result shape explicit — via an optional `expects` parameter with a JSON Schema — would let the parent (a) tell the child what shape to produce, (b) get a parseable result back, and (c) detect when the child deviated from expectations. This is the foundation for reliable agent composition: a parent can call `spawn_agent` and destructure the result programmatically.

## What changes

- **`expects` parameter on `SpawnOpts`**: an optional `*model.ToolSchema` (reusing the existing JSON Schema type from the tools registry) describing the expected result shape. Example: `{"type":"object","properties":{"title":{"type":"string"},"findings":{"type":"array"}}}`.
- **Child agent awareness**: the child's system prompt gains a directive (injected by the spawner) structuring its final output — "Your final message MUST be a JSON object conforming to this schema: <schema>. Do not wrap in markdown code fences."
- **Result parsing**: the spawner attempts to parse the child's final message as JSON and validate it against the schema. If it parses and validates, `SpawnOutput` carries `Structured any` alongside the raw `text`. If not, the raw text is returned as today (no error — the `expects` is a hint, not a constraint).
- **`SpawnHandle` enhancement**: a new `Result() (any, error)` method that returns the structured result (or an error if the child didn't produce one). The existing `Wait()` continues to return the raw text.
- **Tool integration**: `spawn_agent`'s parameters gain an optional `expects` field (JSON Schema as a map). When provided, the tool description is updated to reflect the expected structure.
- **Default behavior**: when `expects` is nil (the common case), behavior is identical to today — free-text result.

## Out of scope

- Schema validation errors causing spawn failure — always degrade gracefully to free-text.
- Nested schemas for sub-sub-agents — each child negotiates its own contract.
- Result schema propagation through the agent tree display.

## Research notes (input for the brainstorm)

This follows the pattern of OpenAI's structured outputs / Anthropic's tool-use strict mode — but applied to agent results rather than tool calls. The key risk is false negatives: a child may produce perfectly good structured data that doesn't parse as JSON (e.g. markdown code fence wrapping, trailing text). The validation should be lenient: try to extract JSON from markdown fences, try `json.Unmarshal` after stripping whitespace, and always fall back to the raw text. The `expects` hint is asymmetric — it only constrains what the parent tells the child to produce, not what the parent must consume. A follow-on could add `ensures` (post-delegation validation) for the parent side. The research skill is the natural first adopter: the synthesizer agent could declare `expects: {type: "object", properties: {report: ..., sources: ...}}`.
