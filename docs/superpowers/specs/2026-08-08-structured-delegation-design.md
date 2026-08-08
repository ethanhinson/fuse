<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0024 — Structured delegation — expected result schemas for spawn_agent](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0024-structured-delegation.md)**
<!-- docket:backlink:end -->

# Structured delegation — expected result schemas for spawn_agent

**Change:** [#0024](../../changes/active/0024-structured-delegation.md) · **Status:** design · **Date:** 2026-08-08

## Problem

`spawn_agent` (change 0012, `done`) returns **free text** — the child agent's final assistant
message, delivered to the parent model as a text tool result and, programmatically, as
`SpawnDone{Result string, Err error}` (`internal/agent/spawn.go:53`). The parent has no way to
declare "produce a result with *these* fields" and no way to detect when the child deviated. The
research skill approximates this by describing the wanted shape in the task prompt — convention,
not contract.

This change adds an optional **`expects` JSON Schema** to a spawn: the parent declares the shape,
the spawner injects it into the child's system prompt, and on return the spawner **validates** the
child's output against the schema. It is an **asymmetric hint, not a constraint** — a mismatch
never fails the spawn; it degrades to free text, surfaces a note the parent model can act on, and
is recorded for observability.

### Two realities that shaped the design

Verified against the current tree during grooming:

1. **No Go/skill code consumes a structured spawn result today.** The only `AgentHandle.Wait()`
   callers are the `cmd/fuse` child-builder closures (`research_probe.go:220`, `shell.go:274`,
   `main.go:214`) — plumbing that returns the child's *text* upward — and `spawn_agent.Execute`
   returns a *text* `Result` to the model. Per ADR-0002 the research skill is **skill-driven**,
   consuming child results as prose in the model's context, not as parsed Go structs. The
   programmatic structured-result handle this change adds is therefore a **foundation ahead of its
   consumer** (change 0026, workflow composition, is the first real consumer). The human accepted
   building it now.
2. **No JSON-Schema validation library is vendored** (`go.mod` has none). Full validation adds a
   dependency — accepted (see D2).

## Design decisions (settled)

The interactive brainstorm settled four points. `superpowers:brainstorming` was unavailable in the
grooming session, so the design was reached inline with the human (docket Skill-layer missing-skill
fallback) and is recorded here as final.

- **D1 — Both producer-side and a programmatic handle.** Inject the schema into the child's system
  prompt (producer side) **and** add `SpawnDone.Structured any` + an `AgentHandle.Result() (any,
  error)` accessor for future programmatic callers (e.g. change 0026). The handle is unused today;
  it is built as the foundation the composition change consumes.
- **D2 — Full JSON-Schema validation.** Vendor a JSON-Schema library (e.g.
  `github.com/santhosh-tekuri/jsonschema`) and validate the child output against the full schema
  (nested types, enums, formats) — not a shallow key-presence check. The extra dependency is
  accepted for real fidelity.
- **D3 — Mismatch degrades, is surfaced, and is logged.** A child output that does not validate
  **never fails the spawn**. The spawner: returns the raw text as the result; appends a
  machine note the parent **model** reads (`(result did NOT match expected schema: <first
  error>)`); leaves `Structured` nil; **and records** the mismatch in a **labeled trace entry**
  and an **`AgentNode` event** (visible in the tree drilldown) for human/observability. A match
  appends a `(matched expected schema)` note and populates `Structured`.
- **D4 — `expects` on both the tool and `SpawnOpts`.** Add an optional `expects` field (JSON Schema
  as an object) to the `spawn_agent` tool parameters **and** an `Expects` field to `SpawnOpts`, so
  both model-driven and code-driven spawns can declare a shape.

## What we build

### `expects` plumbing (D4)

- **`spawn_agent` tool** (`internal/tools/spawn_agent.go`): add an optional `expects` property
  (JSON-Schema object) to `Parameters()` (`spawn_agent.go:107`) and to the `spawnAgentInput`
  struct (`spawn_agent.go:154`). When present, the tool description notes the child will be asked
  to conform. Thread it through `SpawnRequest` into the spawn.
- **`SpawnOpts`** (`internal/agent/spawn.go:40`): add `Expects` (the JSON-Schema document; a
  `map[string]any` or reused `*model.ToolSchema.Parameters` shape). Nil ⇒ today's behavior exactly.

### Producer side — inject the schema into the child (D1)

The spawner augments the child's `SystemPrompt` when `Expects != nil`:

> Your final message MUST be a single JSON object conforming to this JSON Schema: `<schema>`.
> Output only the JSON — do not wrap it in markdown code fences or add commentary.

This is the only change to what the child sees; the child runs normally otherwise.

### Result side — validate, structure, surface, log (D1–D3)

At the point the child's final text becomes the result (`spawn.go:250` returns `result`, then
`SpawnDone{Result, Err}` at `spawn.go:266`), when `Expects != nil`:

1. **Extract JSON** leniently: strip markdown fences / surrounding whitespace, take the outermost
   JSON object, `json.Unmarshal`.
2. **Validate** the parsed value against `Expects` with the vendored JSON-Schema library (D2).
3. **On success**: set `SpawnDone.Structured = parsed`; append `(matched expected schema)` to the
   text result the model reads.
4. **On failure** (unparseable or invalid, D3): leave `Structured = nil`; append `(result did NOT
   match expected schema: <first validation error>)` to the text result; emit a labeled trace
   entry and an `AgentNode.AddEvent(...)` mismatch event (`tree.go:114`). **Never** return an
   error — the spawn succeeds with free text, exactly as today.

`AgentHandle.Result() (any, error)` returns `Structured` (or an error if the child produced no
structured result). `Wait()` continues to return the raw text unchanged.

### Model-facing contract

The model calling `spawn_agent` always receives text. With `expects`, that text is the child's JSON
plus a one-line match/mismatch signal — so the model can trust the shape or react to a deviation.
The `Structured any` value is for the (future) programmatic caller; the model never depends on it.

## Out of scope

- **A programmatic consumer of `Structured`** — none exists today; change 0026 is the first. This
  change ships the handle, not a consumer.
- **Bounded re-ask of the child on mismatch** — considered; deferred (a second child model call per
  miss). Degrade-and-note is the v1 behavior.
- **Nested schemas for sub-sub-agents** — each child negotiates its own contract; no propagation.
- **Result-schema propagation through the agent-tree display** — beyond the mismatch event.
- **`ensures` (parent-side post-delegation validation)** — a possible follow-on named in the stub.

## Tests

- **Nil `expects`** ⇒ byte-identical behavior to today (golden result text; `Structured` nil; no
  note).
- **Producer injection**: with `expects`, the child's system prompt carries the schema directive
  (assert against the spawned child's effective system prompt).
- **Match path**: a child returning conforming JSON ⇒ `Structured` populated, `(matched expected
  schema)` note, `Result()` returns the value.
- **Lenient extraction**: conforming JSON wrapped in ```` ```json ```` fences / with trailing prose
  still parses and validates.
- **Mismatch path (D3)**: non-conforming or unparseable output ⇒ raw text returned, `Structured`
  nil, `(did NOT match … <error>)` note present, **a labeled trace entry and an `AgentNode` event
  recorded**, spawn does **not** error.
- **Full-schema fidelity (D2)**: nested-object, array-of, enum, and type mismatches are each
  detected (cases the vendored library must catch, guarding the dependency choice).
- **Tool surface (D4)**: `spawn_agent` params advertise `expects`; a model-authored `expects`
  round-trips through `spawnAgentInput` → `SpawnRequest` → `SpawnOpts.Expects`.
- **Real-binary seam** (learning `verify-tool-loop-at-gateway-seam`): drive the real binary against
  a scripted `LLM_GATEWAY_URL` double whose child returns (a) conforming and (b) non-conforming
  output, asserting the parent turn sees the match/mismatch note — the TUI harness fakes the
  Completer seam and never exercises the `cmd/fuse` spawn wiring. Wire `expects` through **all**
  child-builder sites per `patch-every-cloned-child-builder` (grep at build time:
  `research_probe.go`, `shell.go`, `main.go`/`run.go`).

## Risks & mitigations

- **Building a handle nothing consumes** (dead code until 0026) — accepted deliberately (D1); the
  producer-side value (clean child JSON + model-facing signal) stands alone even before a Go
  consumer exists.
- **New dependency** (JSON-Schema lib) — scoped to result validation; pin the version; the D2
  fidelity tests justify it over a hand-rolled shallow check.
- **False-negative validation** (good data, bad wrapping) — mitigated by lenient JSON extraction
  before validation.
- **Mismatch invisible to the model** — foreclosed by the D3 model-facing note (distinct from the
  trace/event, which serve the human).
- **Missed wiring site** — `expects` threaded through every child builder, enumerated by grep at
  build time.

## Follow-ups (not this change)

- Change 0026 (workflow composition) consumes `Structured` to feed one step's output into the next.
- Optional bounded re-ask on mismatch.
- `ensures` — parent-side post-delegation validation.
