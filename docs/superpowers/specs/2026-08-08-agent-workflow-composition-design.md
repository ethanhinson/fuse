<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0026 — Workflow composition — chain, fan-out, and conditional routing](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/archive/2026-08-08-0026-agent-workflow-composition.md)**
<!-- docket:backlink:end -->

# Pipeline composition — deterministic DAG execution over the subagent runtime

Design doc for change **#0026** (`agent-workflow-composition`).

> **Naming note.** The change is titled "workflow composition," but fuse already
> owns the word *workflow*: `config.WorkflowConfig` (changes 0034/0036,
> ADR-0007/0009) binds a named skill to a spawn **pool policy** + typed workers,
> and the scheduler carries `WorkflowPool`, `RegisterPool`, and `WorkflowRoot`.
> That existing concept is **resource governance** over a subtree. What this
> change adds is **control flow** — a deterministic step graph. To avoid
> overloading one word across two axes, the new concept is named **`Pipeline`**.
> A pipeline *runs under* a workflow (reusing its pool); the two compose.

## Summary

fuse orchestrates subagents *emergently*: the model decides when to `spawn_agent`,
in what order, and how to combine results. That is powerful but non-deterministic.
This change adds a **pipeline** — a directed acyclic graph (DAG) of named steps
with declared dependencies, blackboard-backed inputs/outputs, per-step error
policy, and structured conditional routing — that the **harness** executes
without the model making sequencing decisions once the DAG is fixed. The model
still authors each step's *content* (every step is a `spawn_agent` call under the
hood); the DAG is the *structure*, and executing the structure is deterministic.

The DAG is an **intermediate representation with two front doors**:

- **Authored** — a skill or CLI task hand-writes a pipeline (YAML/JSON). The
  deterministic base.
- **Synthesized** — fuse is handed a *goal* and an LLM step **generates the DAG
  at runtime**, which the same engine then executes. This is the emphasis of the
  design: rather than a human hardcoding every pipeline, fuse builds the pipeline
  for the task on the fly, then runs it deterministically. Generation is bounded
  and validated so a synthesized DAG is guaranteed valid-and-bounded before any
  step runs (see decision 6).

Both doors produce the same `Pipeline` value and feed the same engine — synthesis
is a *front end* that emits a DAG, never a second execution path.

The motivating use case is the research skill: "5 parallel searches → dedup →
synthesize" — authored as a fixed pipeline, or synthesized from the goal "research
X thoroughly" and then executed identically.

## Goals

- A `Pipeline`/`Step` type and a parser (YAML/JSON) with **parse-time cycle
  detection** — a pipeline with a cycle is rejected before any step runs.
- A **`pipeline_run` built-in tool** that executes a pipeline definition and
  returns terminal status.
- **Parallel fan-out**: every step whose `depends_on` set is satisfied runs
  concurrently (goroutines), governed by the pool of the workflow it runs under.
- **Blackboard-backed I/O**: a step's `inputs` are blackboard keys injected into
  its prompt; its `outputs` are blackboard keys its result is written to. Built
  directly on change **0023**.
- **Structured step results**: a step may declare an `expects` JSON Schema; the
  step's `spawn_agent` call carries it and the validated structured value is what
  lands on the output key. Built directly on change **0024**.
- **Per-step error policy**: `on_error: fail | skip | retry(N)`.
- **Structured conditional routing**: a step may declare `conditions` — a list of
  `{if: {key, op, value}, goto: <step>}` plus an optional `default` — evaluated by
  the engine against the blackboard to pick the next step.
- **Runtime synthesis**: `pipeline_run` accepts a *goal* (as an alternative to an
  authored definition); an LLM step generates a `Pipeline` DAG from the goal, the
  engine **surfaces** it (trace + blackboard key) with an optional confirmation
  gate, then executes it. A synthesized DAG is guaranteed **valid and bounded**
  before execution via a validate-and-retry loop plus resource caps.
- **Terminal status** surfaced both as the tool return and as a
  `pipeline.status` blackboard key.

## Non-goals (v1)

- **Skill-frontmatter `pipelines:` block** (a skill body replaced by a declared
  DAG) — deferred to a follow-up stub. v1 ships the engine + tool; skills invoke
  it via `pipeline_run`.
- **TUI step sub-nodes** (pipeline steps as tree nodes with per-step glyphs /
  elapsed time) — deferred to a follow-up stub. v1 relies on the existing
  per-`spawn_agent` node each step already produces.
- **Dynamic pipelines** (steps that add steps at runtime) — static DAG only.
- **First-class loops / iteration** — expressible via recursive pipeline calls,
  not a language construct.
- **A CEL / arbitrary-expression condition language** — v1 is a fixed operator
  set (below); an expression engine is a later extension if the operator set
  proves insufficient.
- **A visual editor / distributed execution** — YAML/JSON authored by hand or
  synthesized; local subagent goroutines only.
- **Generating executable code** (model-written Go/script compiled and run) — the
  synthesis path emits a **data DAG** the existing engine interprets, not code. Running
  model-authored code is a compile step and a far larger safety surface for no added
  expressiveness the DAG lacks.
- **A separate `pipeline_synthesize` tool** — synthesis is a mode of `pipeline_run`,
  not its own tool; the surface + optional gate already give inspection without a
  second call.
- **Renaming the existing `Workflow`/`WorkflowPool`** — left untouched; `Pipeline`
  is additive.

## Design decisions

Settled through an interactive brainstorm on 2026-08-08.

### 1. A new `Pipeline` concept, separate from `Workflow` (not an extension, not a rename)

Rejected: extending `config.WorkflowConfig` with a `steps:` block (overloads one
struct with governance + control-flow axes) and renaming the existing
`Workflow`→`Pool` (a breaking rename across `scheduler.go`, `strip.go`, the config
schema, ADRs, and every user `.fuse.yml` — too large a blast radius for a medium
change). Instead **`Pipeline` is its own type in a new `internal/pipeline/`
package**, and a pipeline **references a workflow by name** to run under its pool:

```yaml
# unchanged pool-governance construct (internal/config, internal/agent):
workflows:
  research:
    pool: {concurrent: 5, tokens: 200000}
    workers: {searcher: {tools: [...]}}

# NEW control-flow construct (internal/pipeline):
pipelines:
  research-flow:
    workflow: research        # runs under research's pool (optional; omit ⇒ global pool)
    steps:
      - name: search
        worker: searcher      # optional: a typed worker of the referenced workflow
        prompt: "Search for: {{query}}"
        fanout: 5             # run 5 parallel instances
        outputs: [hits/*]
      - name: dedup
        depends_on: [search]
        inputs: [hits/*]
        outputs: [deduped]
      - name: synth
        depends_on: [dedup]
        inputs: [deduped]
        outputs: [report]
```

`internal/pipeline` may import `internal/agent` (for the `Spawner`, blackboard,
and pool activation) but the reverse never holds — the scheduler must not learn
about pipelines. The engine drives execution through the **existing** `Spawner` /
`spawn_agent` seam; it introduces no new spawn path.

### 2. v1 scope: core engine + parallel fan-out + error modes + conditional routing

The v1 line is drawn to ship the full "chain, fan-out, **and conditional
routing**" the title promises — the hard, load-bearing part — while deferring the
surface polish (skill-frontmatter block, TUI sub-nodes) that can layer on later
without reworking the engine.

**In v1:** `Pipeline`/`Step` types; YAML/JSON parse with Tarjan cycle detection;
ready-step parallel execution; blackboard I/O; `expects`-driven structured
results; `on_error: fail|skip|retry(N)`; structured conditional routing; the
`pipeline_run` tool with **both** the authored (`definition`/`name`) and
**synthesized** (`goal`) front doors, including the surface + optional gate and
the bounded validate-and-retry generation loop; `pipeline.status` and
`pipeline.plan` output keys.

**Deferred (each becomes a follow-up stub, `discovered_from: 26`):** the
skill-frontmatter `pipelines:` block; TUI step sub-nodes (a synthesized DAG surfaced
as tree nodes is a natural extension of this deferral).

### 3. Condition DSL: a fixed structured-comparison operator set (no eval engine)

Rejected: a vendored CEL/expr/govaluate string language (adds a dependency and an
eval surface over a stringly-typed JSON blackboard for cases the operator set
already covers) and truthy-key-presence-only (pushes branching logic back into
step prompts, against the deterministic-structure goal). v1 uses a **fixed,
typed operator set** — no parser, no arbitrary expressions, trivially testable:

```yaml
steps:
  - name: scan
    outputs: [scan/status, scan/errors]
    conditions:
      - if: {key: "scan/status", op: eq, value: "clean"}
        goto: synth
      - if: {key: "scan/errors", op: exists}
        goto: repair
    default: synth        # taken when no condition matches
```

`op ∈ exists | eq | ne | gt | lt | contains | matches`:

- `exists` — the key is present on the blackboard (value ignored).
- `eq` / `ne` — JSON-value equality / inequality against `value`.
- `gt` / `lt` — numeric comparison (both sides coerced to number; a
  non-numeric side makes the condition **false**, never an error).
- `contains` — string-substring or array-membership against `value`.
- `matches` — the key's string value matches the `value` regex.

Conditions evaluate **in listed order**; the first match wins; `default` (or, if
absent, the DAG's normal `depends_on` successor ordering) applies when none
match. An `op`/type mismatch is a **false condition, never a pipeline error** —
routing is deterministic and total.

### 4. Deterministic structure, model-authored content

Each step executes as one `spawn_agent` call:

1. The engine resolves the step's `inputs` — reads each blackboard key
   (glob-expanded, e.g. `hits/*`) and injects the values into the step `prompt`
   (template substitution; exact templating form is an implementation detail, but
   inputs are injected, not left to the model to fetch).
2. The step runs under the pool of its referenced `workflow` (via the existing
   `WorkflowRoot` / `RegisterPool` activation) and, when set, as its named
   `worker` (tool allowlist + model pin).
3. If the step declares `expects`, the spawn carries that schema (change 0024);
   the **validated structured value** is what lands on the `outputs` key. Without
   `expects`, the raw final text lands on the key. A 0024 schema *mismatch*
   degrades exactly as 0024 specifies (free text + surfaced note) — the pipeline
   does **not** treat a schema mismatch as a step error; `on_error` governs only
   spawn/execution failure.
4. On completion the engine writes the result to each `outputs` key, then
   evaluates `conditions` to route.

The engine — not the model — owns readiness, parallelism, routing, and error
policy. `fanout: N` spawns N parallel instances of the step; their outputs are
expected to use a glob key namespace (e.g. `hits/*`) so a downstream `inputs:
[hits/*]` collects them all.

### 5. Runtime synthesis — fuse generates the DAG, then runs it (the two front doors)

The DAG is an **intermediate representation**, and the emphasis of this change is
that fuse **generates** it in real time rather than a human hardcoding every
pipeline. Two front doors produce the same `Pipeline` value:

- **Authored** — `pipeline_run({definition: <yaml|json>})` or a registered pipeline
  by name. The deterministic base from decisions 1–4.
- **Synthesized** — `pipeline_run({goal: "<natural-language goal>"})`. The tool
  runs an internal **synthesis** LLM call: given the goal (plus the available
  workers/tools and the pipeline schema as context), the model emits a `Pipeline`
  DAG. The engine then **surfaces** the generated DAG — a labeled trace entry and
  a `pipeline.plan` blackboard key — and, when the caller sets `confirm: true`,
  **gates** execution on confirmation before the first step runs; otherwise it
  proceeds straight to execution. The surfaced DAG is the record of what fuse
  decided to do.

**One tool, mode inferred from params:** `definition`/`name` ⇒ authored;
`goal` ⇒ synthesized. Exactly one must be supplied. Synthesis is a *front end that
emits a DAG* — once the `Pipeline` value exists, execution is byte-identical to the
authored path (same engine, same parallelism, same routing, same `on_error`).
Rejected: a separate `pipeline_synthesize` tool (a two-call inspect-then-run dance
the model rarely needs, since the surface + gate already give visibility) and
generating executable **code** rather than a data DAG (running model-written Go is
a compile step and a far larger safety surface than a validated, bounded data
structure the existing engine interprets).

### 6. Generation is bounded and self-correcting — a synthesized DAG is guaranteed valid before it runs

A generated DAG is model-authored structure, so the engine must **ensure** it is a
valid, bounded pipeline before executing it — not merely detect that it is not. Three
layers, in order, gate every synthesized DAG (the authored path uses layers 1–2 only):

1. **The same parser/validator as the authored path** — unique step names, every
   `depends_on`/`goto`/`default` resolves to a real step, valid operators, and
   **Tarjan cycle detection**. If it does not parse, it is not run.
2. **Resource caps** (synthesis-specific, config-defaulted conservative): `max_steps`,
   `max_fanout` per step, `max_depth`, and a synthesized pipeline **must bind a
   workflow pool** so the subtree's concurrency/token caps (ADR-0007/0009) apply.
   This guards the *valid-but-runaway* DAG — a 500-step or 1000-way-fan-out graph
   that parses cleanly but would exhaust the pool. "Valid" here means **bounded**,
   not just acyclic.
3. **Bounded validate-and-retry** — if layer 1 or 2 rejects the generated DAG, the
   rejection reason is fed back to the synthesis model for up to **N corrective
   attempts** (config `synthesis.max_attempts`, default small). This is what turns
   "the model usually emits a valid DAG" into "the tool *ensures* a valid DAG or
   fails loudly": a single generation pass cannot guarantee validity, a bounded
   correction loop converges on one or returns a clear synthesis-failure error the
   caller (`pipeline_run`) surfaces — it never runs a half-valid graph.

The retry loop and the caps are **synthesis-only**; an authored DAG that violates a
cap is the author's error (a plain parse/validation error), not something to re-ask
a model about.

### 7. Hard dependency on 0023 and 0024 (no degradation path)

`depends_on: [23, 24]`. The engine is written against the **real** blackboard
(0023) and structured-delegation handle (0024) APIs — no shim, no second
free-text code path to build, test, and later delete. 0026 is simply not
build-ready (the selector skips it) until both merge. 0024's spec already names
0026 as its sole consumer, so this ordering is mutual. The **synthesis step itself
consumes 0024** — the synthesis LLM call declares the `Pipeline` schema as its
`expects`, so the generated DAG comes back as a validated structured value rather
than free text to parse. Change **0012** (subagent runtime) is transitively
required and already `done`; it is dropped from `depends_on` in favor of the two
direct substrates (kept in `related`).

## Surfaces & data model

New package **`internal/pipeline/`**:

```go
type Pipeline struct {
    Name     string
    Workflow string   // optional: the WorkflowConfig whose pool this runs under ("" ⇒ global pool)
    Steps    []Step
}

type Step struct {
    Name      string
    Worker    string        // optional: a typed worker of the referenced workflow
    Prompt    string        // task; {{key}} placeholders filled from Inputs
    Inputs    []string      // blackboard keys (glob-expandable) injected into Prompt
    Outputs   []string      // blackboard keys the result is written to
    DependsOn []string      // step names that must complete first
    Fanout    int           // 0/1 ⇒ single; N ⇒ N parallel instances
    Expects   json.RawMessage // optional JSON Schema (change 0024) for a structured result
    OnError   ErrorPolicy   // fail | skip | retry(N)   (default: fail)
    Conditions []Condition  // ordered; first match routes
    Default   string        // next step when no condition matches
}

type Condition struct {
    Key   string
    Op    string   // exists|eq|ne|gt|lt|contains|matches
    Value any
    Goto  string
}
```

- **Parser / validator** (`Parse([]byte) (*Pipeline, error)` and
  `Validate(*Pipeline, Caps) error`): YAML/JSON → `Pipeline`; validates step-name
  uniqueness, that every `depends_on`/`goto`/`default` references an existing step,
  valid operators, and runs **Tarjan cycle detection**; `Validate` additionally
  enforces the resource `Caps` (max steps/fan-out/depth, pool binding) for the
  synthesis path. Any violation is an error naming the offending step(s).
- **Synthesizer** (`Synthesize(ctx, goal string, ctxInfo, *agent.Spawner, Caps)
  (*Pipeline, error)`): runs the synthesis LLM call (declaring the `Pipeline`
  schema as its 0024 `expects`), then `Parse`→`Validate`; on failure it re-asks
  the model with the error up to `synthesis.max_attempts` times before returning a
  synthesis-failure error. Never returns an invalid or over-cap DAG.
- **Engine** (`Run(ctx, *Pipeline, *agent.Spawner, *agent.Blackboard) (Status,
  error)`): topological, readiness-driven, parallel; owns routing and `on_error`.
  Identical for authored and synthesized DAGs.
- **`pipeline_run` tool** — mode inferred from params, exactly one required:
  - `{definition: <yaml|json string>}` or `{name: <registered pipeline>}` — authored.
  - `{goal: <string>, confirm?: bool}` — synthesized: calls `Synthesize`, writes the
    generated DAG to the `pipeline.plan` blackboard key and a labeled trace entry,
    gates on confirmation when `confirm: true`, then runs.

  Wired into agent builders like other built-ins (all three cloned child builders —
  see the `patch-every-cloned-child-builder` learning). Returns terminal `Status`
  and writes `pipeline.status` to the blackboard so a subsequent free-form
  `spawn_agent` or step can read the outcome.

**Config** (`internal/config`, under the existing schema): synthesis caps and the
retry bound, e.g. `pipeline.synthesis.{max_steps, max_fanout, max_depth,
max_attempts}` — each 0 = unset/default; conservative defaults. Subject to the
same `.fuse.local.yml` tighten-only trust boundary (ADR-0006) as pool numbers,
since a loosened cap is a resource-safety loosening.

## Error handling

- `on_error: fail` (default) — a failing step stops the pipeline; the engine
  records the failure and sets `pipeline.status` = failed with the offending step.
- `on_error: skip` — the step's failure is recorded; its outputs are absent;
  execution continues (downstream steps that `inputs` a missing key see it absent,
  which their conditions can test with `op: exists`).
- `on_error: retry(N)` — up to N re-spawns of the step before falling through to
  fail.
- A **schema mismatch is not a step error** (see decision 4) — it degrades per
  0024.

## Testing strategy

- **Parser**: cycle rejection (Tarjan), dangling `depends_on`/`goto` references,
  duplicate step names, YAML and JSON parity.
- **Engine (deterministic, no real LLM)**: inject a fake `Spawner` /
  scripted step results; assert readiness ordering, parallel fan-out
  (`fanout: N` ⇒ N spawns, glob outputs collected), `on_error` for each mode,
  and conditional routing across every operator including type-mismatch →
  false-not-error.
- **Blackboard integration**: inputs injected from keys, outputs written to keys,
  `hits/*` glob round-trip.
- **`expects` integration**: a step with a schema lands the validated structured
  value; a mismatch degrades (0024) without failing the step.
- **Synthesis (deterministic, scripted synthesis response)**: with a fake/scripted
  synthesis reply, assert that a well-formed reply yields a `Pipeline` that runs;
  that a cyclic/dangling/over-cap reply triggers the re-ask loop with the error fed
  back; that the loop **fails loudly** after `max_attempts` rather than running an
  invalid DAG; that the generated DAG is written to `pipeline.plan` and, with
  `confirm: true`, execution waits for confirmation. Caps (`max_steps`/`max_fanout`/
  `max_depth`, pool-binding required) each reject an over-cap synthesized DAG.
- **End-to-end via the real binary** against a scripted `LLM_GATEWAY_URL` double
  (per the `verify-tool-loop-at-gateway-seam` learning): a `pipeline_run({goal})`
  call drives synthesis then execution, and the gateway log shows first the
  synthesis call, then the expected step spawns in dependency order — and a
  `pipeline_run({definition})` call skips synthesis and spawns directly.

## Open questions carried to build

- **Prompt templating form** — `{{key}}` vs a richer form; an implementation
  detail, decided at build time. The contract is only that `inputs` are injected,
  not fetched by the model.
- **Whether `pipeline_run` is always wired or spawn-gated** — likely always wired
  (like the blackboard tool in 0023), honoring an explicit `tools`-subset
  exclusion. Confirm against the child-builder wiring at build time (three cloned
  builders — see the `patch-every-cloned-child-builder` learning).
- **What context the synthesizer is given** — at minimum the goal, the `Pipeline`
  schema, and the available workers/tools of the bound workflow; whether it also
  sees current blackboard keys is a build-time call. The contract is only that the
  synthesis call declares the `Pipeline` schema as its 0024 `expects`.
- **How the confirmation gate is surfaced** in a headless/autonomous run (no human
  to confirm) — likely `confirm` defaults to `false` (synthesize-and-run) so an
  autonomous caller is never blocked, with `confirm: true` reserved for
  interactive/inspectable use. Decide the default at build time.
- **Default cap values** (`max_steps`/`max_fanout`/`max_depth`/`max_attempts`) —
  pick conservative defaults at build; they are tighten-only via `.fuse.local.yml`.

## Dependencies & references

- **0023** (agent-blackboard) — step I/O substrate. **Hard dep.**
- **0024** (structured-delegation) — `expects`-driven structured step results;
  0026 is 0024's named consumer. **Hard dep.**
- **0012** (subagent runtime) — the `Spawner`/`spawn_agent` seam. Done;
  transitive.
- **ADR-0007 / 0009** — the Scheduler and workflow-pool visibility a pipeline runs
  under (does not modify).
- Learnings: `patch-every-cloned-child-builder`, `slot-cap-yield-while-blocked-on-children`,
  `verify-tool-loop-at-gateway-seam`.
