---
id: 24
slug: structured-delegation
title: Structured delegation — expected result schemas for spawn_agent
status: done
priority: medium
type: feat
created: 2026-08-06
updated: 2026-08-08
depends_on: [12]
related: [23, 26]
discovered_from: []
adrs: [12]
spec: docs/superpowers/specs/2026-08-08-structured-delegation-design.md
plan: docs/superpowers/plans/0024-structured-delegation.md
results: docs/results/2026-08-08-structured-delegation-results.md
trivial: false
auto_groomable:
branch: feat/structured-delegation
claimed_at: 
pr: https://github.com/ethanhinson/fuse/pull/29
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-08-structured-delegation-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-08-structured-delegation-design.md) |
| Plan | [0024-structured-delegation.md](https://github.com/ethanhinson/fuse/blob/main/docs/superpowers/plans/0024-structured-delegation.md) |
| Results | [2026-08-08-structured-delegation-results.md](https://github.com/ethanhinson/fuse/blob/main/docs/results/2026-08-08-structured-delegation-results.md) |
| PR | [#29](https://github.com/ethanhinson/fuse/pull/29) |
| ADRs | [ADR-0012](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0012-vendor-jsonschema-validation-library.md) |
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

## Reconcile log

### 2026-08-08 — reconcile before build (implementer)

Verified the spec against the current tree (`origin/main` @ 106b792). The design holds — no
fundamental invalidation, only line-number drift and a few wiring refinements to fold into the
plan:

- **Dependency #12** (`subagent-ux`) is archived `done`; **#26** (workflow-composition, the intended
  consumer of the `Structured` handle) is still `proposed`, so the "foundation ahead of its
  consumer" premise stands. **#23** (blackboard) and **#27** (context-summarization) did **not**
  touch `spawn.go`/`spawn_agent.go`; the recent editors were **#0034** (workflows) and **#0036**
  (scheduler/quotas).
- **Current shapes (verbatim):** `SpawnOpts` (`internal/agent/spawn.go:40`) has fields
  `Label, Task, SystemPrompt, Tools, ModelID, MaxTurns, MaxTokens, Worker` — **no `Expects`** yet.
  `SpawnDone` (`spawn.go:54`) is `{Result string; Err error}` — **no `Structured`**. `AgentHandle`
  (`spawn.go:61`) exposes `Wait() SpawnDone` only — **no `Result()`**. The result-assembly site is
  `spawn.go:250` (`s.buildChild(...)`) → `spawn.go:266` (`doneCh <- SpawnDone{Result, Err}`).
- **Tool surface:** `spawn_agent` `Parameters()` (`spawn_agent.go:107`) advertises
  `label, task, system_prompt, tools, model` (+ conditional `worker`) — **no `expects`**;
  `spawnAgentInput` at `:152`; `SpawnRequest` at `:13`. Note the tool's `Execute` (`:184`) now
  composes the result as `result + t.budgetLine() + t.quotaWarning()` — the `(matched…)`/`(did NOT
  match…)` note therefore rides **inside `done.Result`** (assembled in `spawn.go`), composing
  cleanly with those existing suffixes rather than being a fourth suffix seam in the tool.
- **THREE child-builder/adapter sites, not two.** Per learning `patch-every-cloned-child-builder`,
  `expects` must thread through the `agent.SpawnOpts{...}` construction and the `SpawnFunc` adapter
  in **all three**: `cmd/fuse/main.go` (`SpawnOpts` @ ~204, adapter returns `done.Result, done.Err`
  @ ~223), `cmd/fuse/research_probe.go` (@ ~144/adapter), and `cmd/fuse/shell.go` (@ ~189/adapter).
  Each adapter yields its spawn slot around `handle.Wait()`
  (learning `slot-cap-yield-while-blocked-on-children`), so `Structured` must survive that
  yield/unyield path — it does, since it lives on `SpawnDone` returned by `Wait()`. Enumerate the
  sites by grep at build time.
- **Observability:** `AgentNode.AddEvent(AgentEvent)` at `internal/agent/tree.go:114`; `AgentEvent`
  is `{Kind EventKind; Name string; Payload map[string]any; TS time.Time}` (`tree.go:82`). The
  `EventKind` enum (`tree.go`) has **no** dedicated schema-mismatch kind — the mismatch event should
  reuse an existing kind with a labeled `Name` (e.g. `KindError`/`KindToolResult` + a
  `"schema_mismatch"` `Name` and error in `Payload`), or add one `EventKind` in the same change.
  Plan should pick one explicitly.
- **Dependency:** `go.mod` (Go 1.26.5) vendors **no** JSON-Schema library — D2's vendor step
  (`github.com/santhosh-tekuri/jsonschema` or equivalent) is still required.
- **Verification seam is present:** `cmd/fuse/blackboard_gateway_e2e_test.go` already drives the
  real binary against a scripted gateway double — the model to copy for the match/mismatch
  real-binary test (learning `verify-tool-loop-at-gateway-seam`). Existing unit tests:
  `internal/agent/spawn_test.go`, `internal/tools/spawn_agent_test.go`.

Scope, out-of-scope, and the four design decisions are unchanged. No auto-capture (disabled).
