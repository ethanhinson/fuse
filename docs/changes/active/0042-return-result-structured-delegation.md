---
id: 42
slug: return-result-structured-delegation
title: Fix structured-delegation (expects) vs tool-calling collision via a return_result tool
status: in-progress
priority: high
type: fix
created: 2026-08-09
updated: 2026-08-10
depends_on: []
related: [24]
discovered_from: []
adrs: [12]
spec: docs/superpowers/specs/0042-return-result-structured-delegation.md
plan:
results:
trivial: false
auto_groomable:
branch: feat/return-result-structured-delegation
pr:
blocked_by:
claimed_at: 2026-08-10T00:02:00Z
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [0042-return-result-structured-delegation.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/0042-return-result-structured-delegation.md) |
| ADRs | [ADR-0012](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0012-vendor-jsonschema-validation-library.md) |
<!-- docket:artifacts:end -->

## Why

A subagent spawned with an `expects` JSON Schema is told — via a directive baked into
its persistent system prompt — that its final message must be "ONLY the JSON object"
conforming to the schema. That directive is present on every turn, alongside the full
tool set. The model is thus given two contradictory instructions at once: "output only
JSON" and "call these tools." It resolves the conflict by cramming the expects result
object into a tool call's arguments — observed in production as `write_file` invoked
with `content = {structured result}` and no `path`, which then (correctly) tripped
auto-mode's missing-path safety prompt.

This is a confirmed structural defect in change 0024 (structured delegation), not a
model-quality issue — it reproduces with capable models because the contradiction is
forced by the harness on every tool-calling turn. It affects every structured spawn
that also uses tools, via both the model-driven `spawn_agent` tool and authored
pipelines (both set the same `SpawnOpts.Expects`). Structured delegation is the typed
return-value channel for multi-agent coordination (grading, ensemble, review), so a
bug that silently corrupts it or blocks tool use undermines the whole delegation
feature.

## What changes

Fix the collision structurally by moving the structured result off the message channel
and onto the tool channel:

- When a spawn carries `expects`, synthesize a per-child `return_result` tool whose
  parameters schema IS the expects schema, and STOP injecting the "final message = only
  JSON" directive. The child works with its normal tools, then calls
  `return_result({...})` to deliver its verdict.
- The run loop treats a valid `return_result` call as terminal and validates its args
  (reusing the existing ADR-0012 validator); an invalid call returns the validation
  error as the tool result so the child can self-repair, bounded by a small retry cap.
- Result assembly reads the structured value from the captured `return_result` call
  instead of re-validating the final message; a lenient final-message fallback keeps
  behavior no worse than today for children that never call the tool.
- One implementation choke point (`agent.SpawnOpts.Expects`) fixes both the spawn path
  and pipelines.

Design detail, the rejected alternatives (prompt reword, provider-native
`response_format`, agent-extractor), and the exact code sites are in the linked spec.

## Out of scope

- Provider-native `response_format` / `json_schema` structured outputs — support is
  undiscoverable per-route; a possible later opt-in optimization.
- Building the agent-extractor two-phase pattern — documented in the spec as a fallback
  only.
- Changing the `AgentHandle.Result()` / `ErrNoStructuredResult` contract.
- Changing auto-mode's missing-`path` prompt (correct safety behavior; it's what
  surfaced this bug).

## Open questions

- Retry cap for self-repair (proposed 2) and whether it's configurable.
- Keep the lenient final-message fallback permanently or behind a flag.
- New ADR superseding the directive design vs. an `## Update` on the 0024-lineage ADR.

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->
