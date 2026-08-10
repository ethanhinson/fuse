---
id: 42
slug: return-result-structured-delegation
title: Fix structured-delegation (expects) vs tool-calling collision via a return_result tool
status: implemented
priority: high
type: fix
created: 2026-08-09
updated: 2026-08-10
depends_on: []
related: [24]
discovered_from: []
adrs: [12, 23]
spec: docs/superpowers/specs/0042-return-result-structured-delegation.md
plan: docs/superpowers/plans/2026-08-10-return-result-structured-delegation-plan.md
results: docs/results/2026-08-10-return-result-structured-delegation-results.md
trivial: false
auto_groomable:
branch: feat/return-result-structured-delegation
pr: https://github.com/ethanhinson/fuse/pull/45
blocked_by:
claimed_at: 2026-08-10T01:14:48Z
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [0042-return-result-structured-delegation.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/0042-return-result-structured-delegation.md) |
| Plan | [2026-08-10-return-result-structured-delegation-plan.md](https://github.com/ethanhinson/fuse/blob/feat/return-result-structured-delegation/docs/superpowers/plans/2026-08-10-return-result-structured-delegation-plan.md) |
| Results | [2026-08-10-return-result-structured-delegation-results.md](https://github.com/ethanhinson/fuse/blob/feat/return-result-structured-delegation/docs/results/2026-08-10-return-result-structured-delegation-results.md) |
| PR | [#45](https://github.com/ethanhinson/fuse/pull/45) |
| ADRs | [ADR-0012](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0012-vendor-jsonschema-validation-library.md), [ADR-0023](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0023-structured-delegation-return-result-tool.md) |
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
- One implementation choke point (`agent.SpawnOpts.Expects`) drives the code path
  (pipelines and code-generated spawns).
- **Architectural boundary (added):** remove the `expects` param from the freeform
  `spawn_agent` tool so model-driven spawns return **prose**; `expects` is reserved for
  the pipeline / code-generated path (which sets `SpawnOpts.Expects` directly and is
  untouched). For a code-editor / problem-solver whose subagent results are consumed by
  another LLM, prose is the right default; forcing a schema is what invited the collision.
  With the affordance gone, the collision is structurally impossible on the freeform
  path, and `return_result` fires only where a machine consumes the result.

Design detail, the rejected alternatives (prompt reword, provider-native
`response_format`, agent-extractor, and keep-but-soften the param), and the exact code
sites are in the linked spec.

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

### 2026-08-10 — reconcile (docket-implement-next)

Verified the spec against current `main` code. All cited code sites still hold:

- `internal/agent/spawn.go:339-340` — directive injection (`augmentPromptWithSchema`) at
  the single choke point, confirmed. Result assembly at `spawn.go:362-404`, confirmed.
- `internal/agent/loop.go` — tool dispatch is in `executeTools` (loop.go:551), invoked from
  the `Run` loop at loop.go:522; range still matches the spec's "around loop.go:455-526".
- `internal/agent/schemavalidate.go` — `validateAgainstSchema` + `augmentPromptWithSchema`
  present and reusable; the ADR-0012 vendored validator (`santhosh-tekuri/jsonschema/v6`)
  is intact.
- Pipeline path (`internal/pipeline/engine.go:349-376` `spawnOnce`) reads structured results
  via `AgentHandle.Result()` → `SpawnDone.Structured`, so fixing `spawnLocal` to source
  `Structured` from the captured `return_result` value fixes pipelines with no
  pipeline-specific code (spec D6 confirmed). `synthesize.go:54` uses the same `sp.Spawn`.
- `internal/tools/spawn_agent.go:142-148` — `expects` param description present; parent-facing
  wording update per spec still applies.

Scope refinement (not an invalidation): the `ChildBuilder` seam
(`agent.ChildBuilder = func(...) (string, error)`) returns only text, and is defined at THREE
cmd sites (`cmd/fuse/main.go:174`, `cmd/fuse/shell.go:254`, `cmd/fuse/research_probe.go:147`).
The captured `return_result` value therefore must flow back to `spawnLocal` through a
package-internal channel driven off `opts.Expects` at the choke point — NOT by widening the
`ChildBuilder` signature (which would churn all three cmd sites and any external adapter). The
design still converges at `spawn.go` exactly as the spec requires; this note records the
implementation shape the plan/build must honor: the synthesized tool + terminal handling live
in the `agent` package (loop + spawner), and the cmd-site child builders stay untouched.

Design is sound and NOT fundamentally invalidated. `auto_capture` is disabled repo-wide, so no
stubs minted this pass; no adjacent follow-up work surfaced beyond the ADR question the spec
already defers to review.

### 2026-08-10 — reopened (implemented → in-progress): scope added by human decision

Live verification of the shipped `return_result` fix (driving `glm` on a by-domain review +
novelty debate against the feature branch) confirmed it works — 11/11 structured returns
well-formed across all 9 domains — but also showed the old failure mode still occurs occasionally
(1 path-less `write_file{content:{...}}` cram in 20 writes). Root discussion concluded that
`expects` is the wrong DEFAULT for the freeform, model-driven `spawn_agent` path (consumer is
another LLM; prose is better and carries nuance), and belongs only where a MACHINE consumes the
result (pipelines / code-generated spawns, which set `SpawnOpts.Expects` directly).

Decision (human): **remove the `expects` param from the `spawn_agent` tool** (prose-only freeform
path); keep `SpawnOpts.Expects` + `return_result` for the code path. Folded into THIS change and
shipping in the SAME PR (#45) as the `return_result` work — the two are halves of one idea. Spec
updated with the added decision + D7 + testing. Reopened to in-progress to extend the existing
`feat/return-result-structured-delegation` branch; branch/pr/plan/results retained.
