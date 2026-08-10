<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0042 — Fix structured-delegation (expects) vs tool-calling collision via a return_result tool](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0042-return-result-structured-delegation.md)**
<!-- docket:backlink:end -->

# Spec 0042 — Fix structured-delegation (`expects`) vs. tool-calling collision via a `return_result` tool

## Problem

A subagent spawned with an `expects` JSON Schema is instructed — via a directive
injected into its **persistent system prompt** — that its **final message MUST be a
single JSON object** conforming to the schema, "Output ONLY the JSON object — no code
fences, no commentary, no surrounding prose" (`internal/agent/schemavalidate.go:132-134`,
injected at `internal/agent/spawn.go:339-340`). That directive is unconditional and
present on **every turn** (`withSystem()` re-prepends the system prompt each turn —
`internal/agent/loop.go:461`), while the **full tool set is offered on those same
turns** (`loop.go:459-464`). No code relaxes the directive when tools are present, and
there is no separate channel for the structured result.

The result is a **channel collision**. The model has two output channels per turn: the
**tool-call channel** (`tool_calls[].arguments`, validated against a *tool's* schema)
and the **message-content channel** (the assistant reply). The `expects` feature reads
the structured result from the **message channel** and validates *that*
(`spawn.go:362-404` → `validateAgainstSchema`). But the child still needs the tool
channel to do its work. Told "output ONLY the JSON object" while also being handed
`write_file{path,content}`, a model reconciles the contradiction by routing the
**expects result object into a tool call's arguments** — observed in production as
`write_file` called with `content = {"novelty":[...]}` (the *result* schema) and **no
`path`**. This is not model weakness (reproduced with a capable model); it is an
instruction-space contradiction the harness forces on every tool-calling turn.

This is a structural defect in **change 0024 (structured delegation)** / **ADR-0012**
(the vendored JSON-Schema validation library). It affects **every** structured spawn
that also calls tools — both the model-driven `spawn_agent` path
(`internal/tools/spawn_agent.go:202-203`) and authored pipelines
(`internal/pipeline/engine.go:355-357`), because both set the same
`agent.SpawnOpts.Expects` and pass through the single choke point at `spawn.go:339`.

## Root cause, precisely

Prompt-content contradiction, *made unavoidable by code*: the "final message = only
JSON" directive and the tool schemas are presented together, unconditionally, on every
turn. A wording-only softening ("once you're done with tools, your final message must
be JSON") lowers but does not remove the failure rate — it asks the model to hold a
temporal condition across an unknown number of turns, fails silently and corruptingly
when misjudged, and is not deterministically testable. The durable fix must remove the
collision structurally: the structured result must live on the **tool channel**, the
same channel the working tools already use, so it never competes with the message
channel.

## Decision — `return_result` tool as the structured-output channel (primary)

When a spawn carries `Expects`, synthesize a per-child tool named `return_result` whose
**parameters schema IS the `expects` schema**, add it to the child's tool set, and
**stop injecting the "final message = JSON" directive**. The child works normally with
its tools and, when done, calls `return_result({...})` to deliver its verdict. The loop
treats a `return_result` call as **terminal**: it validates the call's arguments against
the schema (reusing `validateAgainstSchema` / the ADR-0012 validator) and that validated
value becomes `SpawnDone.Structured`. Returning the result and calling other tools become
the same kind of action — no contradictory directive, no channel competition.

### Why this over the alternatives (recorded for durability)

- **vs. prompt reword:** reword is a band-aid — probabilistic, silent-on-failure, not
  deterministically testable. May ship as an *additional* safety nudge but not as the fix.
- **vs. provider-native `response_format`/`json_schema`:** support is **undiscoverable
  and per-route** — `GET /v1/models` exposes no capability flag (verified against the
  local gateway), support varies by upstream model and by whether LiteLLM passes it
  through, and it's only safe on a tool-free turn. Good as an *opt-in per-alias
  optimization* later; unfit as the portable default. Out of scope here.
- **vs. agent-extractor (two-phase):** worker returns prose, a second cheap agent
  extracts+validates. Documented as a **fallback pattern** (see below), not built:
  it costs an extra model call per structured spawn, adds latency, is harder to test,
  and — critically — can return a **confidently-wrong** value (the extractor summarizes
  the transcript rather than reporting what the model decided). For a coordination/
  grading surface, silent wrongness is the worst failure, so extraction must not be the
  default.

## Decision (added) — the freeform `spawn_agent` path returns PROSE; `expects` is reserved for the pipeline / code-generated path

Live verification of the `return_result` fix (driving `glm` on a by-domain review +
novelty debate) confirmed the fix works — 11/11 structured returns well-formed across
all domains — but also surfaced the deeper issue: **`expects` is the wrong default for a
model-driven, freeform code-editing / problem-solving spawn.** For that class of work the
consumer of a subagent's result is *another LLM* (the parent synthesizing), which reads
prose fine and benefits from its nuance; forcing a rigid final object couples "doing" with
"formatting," invites premature structure, and — as observed — nudges the model to invent
`expects` schemas for inherently prose-shaped tasks (a code review), occasionally routing
its result through `write_file` instead. `expects` belongs where the consumer is **code**:
authored pipelines (routing/gating on the result) and any code-generated spawn.

**The two paths are already fully separable in the codebase** (verified): the pipeline /
authored path sets `agent.SpawnOpts.Expects` **directly**
(`internal/pipeline/engine.go:357`, `internal/pipeline/synthesize.go:52`) and never
touches the `spawn_agent` tool; the **only** model-facing entry for `expects` is the
`spawn_agent` tool's `expects` param (`internal/tools/spawn_agent.go:142-148` schema →
`spawnAgentInput.Expects` line 177 → `req.Expects` lines 202-204).

**Decision:** **remove the `expects` param from the `spawn_agent` tool** — delete it from
the tool's advertised schema, from `spawnAgentInput`, and from the `req.Expects` threading.
Freeform model-driven spawns become **prose-only**. `agent.SpawnOpts.Expects` and the whole
`return_result` machinery (above) **remain unchanged** for pipelines and code-generated
spawns — that path is where structured delegation is correct. Net effect: `return_result`
now only ever fires on the code-driven path, which is exactly where a machine consumer
needs it.

**Rejected — "keep the param but soften its description":** relies on the model heeding
guidance to *not* reach for a tool it can see — the same guidance-dependent design class
that produced the original collision. Removing the affordance is the robust choice; a tool
the model cannot see is a tool it cannot misuse.

**Scope note:** this decision is folded into change 0042 (this spec) and ships in the same
PR (#45) as the `return_result` work — the two are halves of one idea (structured return
belongs to the code path only), and shipping them together avoids an intermediate state
where `return_result` exists yet the model can still request `expects` on freeform spawns.

## Design

All changes converge at the single choke point (`spawn.go:339`) so spawn + pipeline are
fixed together.

### D1 — Synthesize the `return_result` tool

- At spawn time, when `opts.Expects != nil`, build a `return_result` tool whose
  `Parameters()` returns the `expects` schema (a `map[string]any`), with a description
  like: *"Call this once, when your task is complete, to return your final structured
  result. Its arguments must conform to the schema. Do your work with the other tools
  first, then call return_result exactly once."*
- Add it to the child's registry/tool set for this run only (per-child, like the
  provenance-bound blackboard handle). It must **not** leak to children that carry no
  `Expects`.
- Name collision: if a real tool named `return_result` ever exists, the synthesized one
  wins for `Expects` children (document it; today none exists).

### D2 — Stop injecting the message-channel directive

- When `Expects` is set AND the `return_result` tool is installed, do **not** call
  `augmentPromptWithSchema` (`spawn.go:339-340`). Instead inject a short, non-
  contradictory hint naming the `return_result` tool as the way to finish (see D1 text).
- `augmentPromptWithSchema` may remain for a possible fallback mode, but the default
  `Expects` path no longer uses it. (If we keep a no-tools fallback, the directive is
  used only there — see D5.)

### D3 — Terminal handling in the run loop

- In the loop (`internal/agent/loop.go`, around the tool-dispatch at `loop.go:455-526`),
  when the child calls `return_result`: validate its `arguments` against the schema.
  - **Valid** → the run is complete; set the structured value as the result and end the
    loop (do not require a further assistant message). Executing `return_result` yields a
    trivial tool result (e.g. `"result recorded"`); the loop then terminates for this
    child.
  - **Invalid** → return the validation error as the tool result (the concise,
    model-readable message `validateAgainstSchema` already produces) and **let the child
    retry** — this is the self-repair backstop (D4). Do not fail the spawn.
- Preserve the existing `stripSpawn` behavior and other loop invariants; `return_result`
  is additive.

### D4 — Self-repair loop (reliability backstop)

- Bound the retries (e.g. up to N=2 re-calls) so a child that keeps emitting non-
  conforming `return_result` args cannot loop forever. On exhaustion, end with **no
  structured result** — exactly today's `ErrNoStructuredResult` posture
  (`spawn.go:44,132-135`), never a hard spawn failure (ADR-0024 best-effort spirit).
- This replaces today's *silent* single-shot "final message didn't match → nil
  Structured" with an *active* correct-the-model loop.

### D5 — Result assembly at the choke point

- Rework `spawn.go:362-404`: the structured result now comes from the validated
  `return_result` call captured during the run, not from re-validating the final message
  text. `SpawnDone.Structured` is set from that captured value; `SpawnDone.Result`
  (the human-facing text) stays whatever the child's last assistant message was (may be
  empty, which is fine — the programmatic channel is `Structured`).
- **Back-compat:** if a child never calls `return_result` (older prompt, or model chose
  not to), keep a lenient fallback that still tries `validateAgainstSchema` on the final
  message text — so behavior is never *worse* than today. This makes the change strictly
  additive at the contract level: `Result()` still returns the parsed value or
  `ErrNoStructuredResult`.

### D6 — Pipelines

- No pipeline-specific code should be needed: pipelines set `opts.Expects` via
  `engine.go:355-357` and inherit the choke-point behavior. Verify `synthesize.go:26`
  (which already parses "structured value or text") still composes — its structured
  branch now receives the `return_result`-sourced value.

### D7 — Remove `expects` from the freeform `spawn_agent` tool (prose-only model path)

Per the added decision above: strip the model-facing `expects` affordance so freeform
spawns are prose-only, leaving the code path's `SpawnOpts.Expects` untouched.

- Delete the `expects` property from the `spawn_agent` tool's advertised parameters
  (`internal/tools/spawn_agent.go:142-148`).
- Delete the `Expects` field from `spawnAgentInput` (`spawn_agent.go:177`) and the
  `if input.Expects != nil { req.Expects = input.Expects }` threading
  (`spawn_agent.go:202-204`). `SpawnRequest.Expects` may remain as a field for the
  code/pipeline path, but nothing model-facing populates it anymore.
- Update the `spawn_agent` tool description to state that a child returns its findings
  as prose (no structured-schema option on this path).
- Consequence to verify: with no model-facing `expects`, a freeform child is never given
  a `return_result` tool and never carries the schema directive — so the collision is
  not merely mitigated but **structurally impossible** on the freeform path. The
  `return_result` machinery (D1–D5) now fires only via the pipeline/code path.

## Affected code

- `internal/agent/spawn.go` — `spawn.go:339-340` (stop directive; install tool),
  `spawn.go:362-404` (result assembly from captured `return_result`).
- `internal/agent/loop.go` — `loop.go:455-526` (detect `return_result`, validate,
  terminal-vs-retry), capture the validated value for the spawner.
- `internal/agent/schemavalidate.go` — reuse `validateAgainstSchema`; `return_result`
  tool schema construction may live here or beside the spawner.
- A new synthesized tool (small) — `return_result`, schema = the `expects` schema.
- `internal/tools/spawn_agent.go` — **remove** the `expects` param (schema
  `spawn_agent.go:142-148`, `spawnAgentInput.Expects` line 177, threading lines 202-204);
  reword the tool description to prose-only (D7). This supersedes the earlier plan to
  merely reword the param.
- Docs: note the mechanism change under change 0024 lineage; consider an ADR update or
  a new ADR recording "structured delegation returns via a synthesized `return_result`
  tool, not a final-message directive" (supersedes the directive approach of ADR-0012's
  companion design). ADR decision deferred to build/review.

## Non-goals

- Provider-native `response_format` / `json_schema` decoding (undiscoverable per-route;
  a later opt-in optimization).
- Building the agent-extractor path (documented as a fallback pattern only, below).
- Changing the `AgentHandle.Result()` contract or `ErrNoStructuredResult` semantics.
- Changing auto-mode's missing-`path` prompt (that safety behavior is correct and is how
  this bug was surfaced).
- Reworking the blackboard, scheduler, or unrelated tool schemas.

## Documented fallback pattern (NOT built): agent-extractor

For a worker that cannot be given a `return_result` tool (an un-modifiable/opaque child)
or is so tool-heavy that producing the final object in-band is undesirable: run the
worker with no `Expects`, then spawn a cheap second agent (or use the parent) to extract
and `validateAgainstSchema` the structured value from the worker's transcript. Costs an
extra model call and carries a fidelity risk (extractor may mis-read the transcript), so
it is an escape hatch, not the default. Recorded here so the choice is not re-litigated.

## Testing

- **Unit — tool synthesis:** an `Expects` spawn installs a `return_result` tool whose
  parameters equal the schema; a non-`Expects` spawn does not.
- **Unit — no directive:** with `return_result` installed, the child's system prompt does
  NOT contain the "final message MUST be a single JSON object" directive.
- **Loop — terminal valid:** a child that calls `return_result` with conforming args ends
  the run and yields that value as `Structured`; assert `Result()` returns it.
- **Loop — repair then success:** first `return_result` call is non-conforming → the tool
  result carries the validation error → the child's next `return_result` conforms →
  success. Assert bounded retries and that a spawn is never hard-failed.
- **Loop — exhaustion:** persistent non-conforming calls hit the retry cap →
  `ErrNoStructuredResult`, spawn still completes.
- **Regression (the reported bug):** a child with `Expects` that also uses `write_file`
  produces a well-formed `write_file{path,content}` AND a separate `return_result` —
  assert the structured object is NOT crammed into `write_file.content` and `path` is
  present. This is the direct guard for the production failure.
- **Back-compat:** a child that returns a conforming final message and never calls
  `return_result` still yields a structured result via the lenient fallback (D5).
- **Pipeline:** an authored pipeline step with `expects` gets a structured result via the
  same path (`engine.go`/`synthesize.go` compose unchanged).
- **D7 — spawn_agent prose-only:** the `spawn_agent` tool's advertised schema does NOT
  contain an `expects` property; a model-supplied `expects` in raw args is ignored (or
  rejected) rather than threaded to `SpawnRequest`; a freeform spawn is given no
  `return_result` tool and no schema directive. Assert the pipeline path (`SpawnOpts.Expects`
  set directly) is unaffected and still yields a structured result.

## Open questions (resolve at build time)

- Retry cap N (proposed 2) and whether it's configurable.
- Whether to keep the lenient final-message fallback (D5) permanently or behind a flag.
- Whether this warrants a new ADR superseding the directive design, or an `## Update`
  note on the existing 0024-lineage ADR — decide during review per the ADR skill.
- Exact home for the synthesized tool (schemavalidate.go vs. a new small file).
