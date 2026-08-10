---
id: 23
slug: structured-delegation-return-result-tool
title: Structured delegation returns via a synthesized return_result tool, not a final-message directive
status: Accepted
date: 2026-08-09
supersedes: []
reverses: []
relates_to: [12]
change: 42
---

## Context

When a child agent is spawned with an `expects` JSON Schema (structured
delegation, change 0024), the harness previously injected a directive into the
child's **persistent system prompt** telling it that its final message MUST be
ONLY a JSON object conforming to the schema. That directive was present on
**every turn**, alongside the full tool set. This created an unavoidable
**instruction-space contradiction** — "output ONLY JSON" vs. "call these tools"
— which capable models resolved by cramming the structured result object into a
tool call's arguments. It was observed in production as `write_file` called with
`content={structured result}` and no `path`, which then tripped auto-mode's
missing-path safety prompt.

This is a **structural defect, not a model-quality issue**: it reproduces with
capable models because the contradiction is forced by code on every tool-calling
turn. It affects **every structured spawn that also uses tools**, via both the
model-driven `spawn_agent` path and authored pipelines — both set
`agent.SpawnOpts.Expects` and converge at one choke point, `spawn.go`.

Note: the mechanism this ADR replaces — the "final message = only JSON"
system-prompt directive — was the *companion design* to structured delegation
(change 0024), a prompt-directive implementation detail. It was **never itself an
ADR**, so this ADR records the replacement in Context/Consequences rather than
via the `supersedes:` field. This ADR does **not** reverse ADR-0012 (the vendored
jsonschema validation library); that validator is reused unchanged, hence
`relates_to: [12]`.

## Decision

**When a spawn carries `Expects`, synthesize a per-child tool named
`return_result` whose parameters schema IS the expects schema, add it to the
child's tool set, and STOP injecting the "final message = only JSON" directive**
(replaced by a short, non-contradictory hint naming `return_result`).

The child works normally with its tools and, when done, calls
`return_result({...})` to deliver its verdict. This moves the structured result
off the **message channel** and onto the **tool channel** — the same channel the
working tools already use — so it never competes with the message channel.

Run-loop rules:

- A valid `return_result` call is **terminal**: the loop validates the call's
  arguments against the schema (reusing ADR-0012's `validateAgainstSchema`), and
  that validated value becomes `SpawnDone.Structured`.
- **Invalid args are returned to the child as the tool result** so it can
  self-repair, bounded by a **retry cap (N=2)**. On exhaustion the run ends with
  **no structured result** (`ErrNoStructuredResult`) — never a hard spawn failure
  (ADR-0024 best-effort spirit).
- A **lenient final-message fallback is retained permanently**: if a child never
  calls `return_result`, the loop still tries `validateAgainstSchema` on the final
  message text. Behavior is therefore never worse than before, and the change is
  **strictly additive at the `AgentHandle.Result()` contract level**.

## Consequences

- **Enables.** Structured delegation that also calls tools now works reliably —
  the production bug is structurally eliminated, guarded by a regression test
  asserting `write_file{path,content}` and `return_result` coexist without
  cross-contamination. The fix converges at one choke point (`spawn.go`), so
  `spawn_agent` and pipelines are fixed together with no pipeline-specific code.
- **Costs / trade-offs.** A synthesized per-child tool adds one entry to the
  `Expects`-child's tool set (it never leaks to non-`Expects` children). The
  self-repair loop can spend up to N extra turns on a non-conforming child before
  degrading to no-structured-result.
- **Rejected alternatives (recorded for durability).**
  - *(a) Prompt reword* — a probabilistic band-aid, silent on failure, not
    deterministically testable.
  - *(b) Provider-native `response_format` / `json_schema`* — support is
    undiscoverable per-route and only safe on a tool-free turn, unfit as the
    portable default (a possible later per-alias opt-in).
  - *(c) Agent-extractor two-phase* — costs an extra model call and can return
    confidently-wrong values, so it is documented as a fallback pattern only, not
    built.
- **Relationship to prior decisions.** This **supersedes the "final-message = JSON
  directive" mechanism** that was the companion design to structured delegation
  (change 0024) — a prompt-directive design, not itself an ADR, hence recorded
  here rather than in `supersedes:`. It does **not** reverse ADR-0012 (the
  vendored jsonschema validation library) — that validator is **reused unchanged**.
