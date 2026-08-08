---
id: 26
slug: agent-workflow-composition
title: Workflow composition — chain, fan-out, and conditional routing
status: in-progress
priority: medium
type: feat
created: 2026-08-06
updated: 2026-08-08
depends_on: [23, 24]
related: [12, 23, 24]
discovered_from: []
adrs: [14]
spec: docs/superpowers/specs/2026-08-08-agent-workflow-composition-design.md
plan: docs/superpowers/plans/2026-08-08-agent-workflow-composition-plan.md
results:
trivial: false
auto_groomable:
branch: feat/agent-workflow-composition
claimed_at: 2026-08-08T22:08:21Z
pr:
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-08-agent-workflow-composition-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-08-agent-workflow-composition-design.md) |
| Plan | [2026-08-08-agent-workflow-composition-plan.md](https://github.com/ethanhinson/fuse/blob/feat/agent-workflow-composition/docs/superpowers/plans/2026-08-08-agent-workflow-composition-plan.md) |
| ADRs | [ADR-0014](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0014-pipeline-conditional-routing-skip-propagation.md) |
<!-- docket:artifacts:end -->

## Why

fuse's agent orchestration is *emergent*: the LLM decides when to spawn subagents, in what order, and how to combine their results. That is powerful but non-deterministic — there is no way to declare a fixed process ("do A, then B and C in parallel, then D over the combined result") that the harness executes reliably. Map-reduce over search results and conditional branching (if X, do Y; else Z) are encoded ad-hoc in the model's task prompt today. A **pipeline** — a DAG of named steps with declared dependencies, blackboard-backed inputs/outputs, per-step error policy, and structured routing — lets the harness run a multi-step process *without the model making sequencing decisions once the DAG is fixed*. The model authors each step's content; the DAG is the deterministic structure; executing it is deterministic.

The emphasis of this change is that **fuse generates the pipeline in real time** rather than a human hardcoding every DAG. The DAG is an *intermediate representation* with **two front doors**: **authored** (a skill/CLI hand-writes the YAML/JSON — the deterministic base) and **synthesized** (fuse is handed a goal and an LLM step generates the DAG on the fly, which the same engine then executes). The research skill is the motivating case: "5 parallel searches → dedup → synthesize" — authored as a fixed pipeline, *or* synthesized from the goal "research X thoroughly" and executed identically.

**Naming:** fuse already owns the word *workflow* — `config.WorkflowConfig` (changes 0034/0036, ADR-0007/0009) binds a skill to a spawn **pool policy** + typed workers (resource governance). This change adds a distinct **control-flow** concept, named **`Pipeline`** to avoid overloading one word across two axes. A pipeline *runs under* a workflow, reusing its pool; the two compose.

## What changes

- **New `internal/pipeline/` package** with a `Pipeline`/`Step` type, a YAML/JSON parser, and a validator that enforces the DAG (unique step names, resolvable references, **Tarjan cycle detection**, valid operators) — a cyclic or malformed pipeline is rejected before any step runs. A pipeline optionally names a `workflow:` to run under its pool.
- **Runtime synthesis** — fuse **generates** the DAG from a goal at runtime, not just interprets a hand-authored one. A synthesis LLM call (declaring the `Pipeline` schema as its 0024 `expects`) emits a DAG; the same parser/validator gates it. This is the primary emphasis; the authored path is the deterministic base beneath it.
- **`pipeline_run` built-in tool** — one tool, mode inferred from params: `{definition|name}` runs an **authored** pipeline; `{goal, confirm?}` **synthesizes** one — it generates the DAG, surfaces it (a `pipeline.plan` blackboard key + a labeled trace entry), gates on confirmation when `confirm: true`, then executes. Returns terminal status and writes `pipeline.status`.
- **Bounded, self-correcting generation** — a synthesized DAG is guaranteed **valid and bounded** before it runs: it passes the same parser/validator, then resource **caps** (`max_steps`, `max_fanout`, `max_depth`, and a required pool binding), and on any rejection the error is fed back to the synthesis model for up to `max_attempts` corrective tries before failing loudly. Never runs a half-valid or runaway graph.
- **Deterministic execution over the existing spawn seam** — the engine (not the model) owns readiness, parallelism, routing, and error policy, identically for authored and synthesized DAGs. Every ready step runs concurrently; each step is one `spawn_agent` call whose prompt is enriched with its `inputs` (blackboard keys). `fanout: N` spawns N parallel instances into a glob output namespace.
- **Blackboard-backed I/O (change 0023)** — a step's `inputs`/`outputs` are blackboard keys; a step result is readable by a later step or a free-form `spawn_agent` and vice versa.
- **Structured step results (change 0024)** — a step may declare an `expects` JSON Schema; the validated structured value lands on the output key. A schema mismatch degrades per 0024 (free text + surfaced note) and is *not* a step error.
- **Per-step error policy** — `on_error: fail | skip | retry(N)`; a hard failure sets `pipeline.status` and stops (or, per policy, skips/retries).
- **Structured conditional routing** — a step may declare ordered `conditions` of the form `{if: {key, op, value}, goto: <step>}` plus an optional `default`, evaluated by the engine against the blackboard. `op ∈ exists | eq | ne | gt | lt | contains | matches`. No expression engine — a fixed, typed operator set; an operator/type mismatch is a false condition, never an error (routing is total).

## Out of scope

- **Skill-frontmatter `pipelines:` block** (a skill body replaced by a declared DAG) — deferred to a follow-up. v1 skills invoke `pipeline_run`.
- **TUI step sub-nodes** (per-step glyphs / elapsed time under a pipeline root) — deferred to a follow-up; v1 relies on the existing per-`spawn_agent` node.
- **Generating executable code** — the synthesis path emits a **data DAG** the existing engine interprets, not model-written Go/script that gets compiled and run (a compile step and a far larger safety surface for no added expressiveness).
- **A separate `pipeline_synthesize` tool** — synthesis is a mode of `pipeline_run`; the surface + optional gate already give inspection without a second call.
- Dynamic pipelines (steps that add steps at runtime) — static DAG only.
- First-class loops / iteration — expressible via recursive pipeline calls, not a language construct.
- A CEL / arbitrary-expression condition language — fixed operator set only in v1.
- Visual editor; distributed execution — hand-authored or synthesized YAML/JSON, local goroutines only.
- Renaming the existing `Workflow`/`WorkflowPool` — left untouched; `Pipeline` is additive.

## Design decisions

Design settled through an interactive brainstorm on 2026-08-08 and captured in the linked spec. Seven decisions fixed the shape: (1) a **new `Pipeline` concept in `internal/pipeline/`**, separate from the existing pool-governance `Workflow` — not an extension of `WorkflowConfig`, not a breaking rename; a pipeline references a workflow to run under its pool. (2) **v1 ships the full engine** — parallel fan-out, `on_error` modes, **and** conditional routing — deferring only the skill-frontmatter block and TUI sub-nodes. (3) **Conditions use a fixed structured-comparison operator set** (`{key, op, value}`), not a vendored expression language. (4) **Deterministic structure, model-authored content** — the engine drives sequencing; each step is a `spawn_agent` call using 0024's `expects` for structured results. (5) **Two front doors, one engine** — the DAG is an IR produced either by hand (authored) or by **runtime synthesis** (fuse generates it from a goal); `pipeline_run` infers the mode, surfaces the generated DAG, and optionally gates it before running. (6) **Generation is bounded and self-correcting** — a synthesized DAG passes the same validator plus resource caps, with a bounded re-ask loop that *ensures* a valid, bounded pipeline or fails loudly (never runs an invalid one), and emits a **data DAG**, not executable code. (7) **Hard `depends_on: [23, 24]`** — written against the real blackboard and structured-delegation APIs (the synthesis step itself consumes 0024), no degradation shim; 0024's spec already names 0026 as its sole consumer. Change 0012 (done) is transitive and moved to `related`.

## Reconcile log

### 2026-08-08 — reconcile before build (implementer)

Re-read the change body and linked spec against current `origin/main` (tip `5ba21bf`, PR #31 merged) and the substrate packages this change builds on. **Verdict: no drift; scope unchanged; proceeding to build.**

- **Dependencies satisfied.** Both hard deps are `done`/archived: `0023-agent-blackboard` and `0024-structured-delegation` (archived 2026-08-08). Build-readiness confirmed against the change file, not just the digest.
- **Substrate APIs verified present as the spec assumes** (`internal/agent`, `internal/tools`, `internal/config`, `cmd/fuse`):
  - `agent.Blackboard` — `Put/Get/Delete/Keys(pattern)/Wait`; `Keys` supports `path.Match` glob (`hits/*`); `ForNode()` returns a `tools.BlackboardStore` with provenance + slot-yield. The step-I/O substrate (0023) exists exactly as decision-4 describes.
  - `agent.SpawnOpts.Expects any` (JSON Schema); `Spawner.Spawn(ctx, SpawnOpts) (AgentHandle, error)`; `AgentHandle.Result() (any, error)` returns the validated structured value (`ErrNoStructuredResult` on no-schema/mismatch); a schema mismatch degrades to free text + note, never a spawn error. The structured-result substrate (0024) matches decision-4's "expects-driven structured results, mismatch is not a step error."
  - `agent.NewSpawner(...Option)` with `WithTree/WithNode/WithSpawnDepth/WithChildBuilder/WithSpawnBackstop`; `ChildBuilder` closure sets child prompt (`opts.SystemPrompt`), tools (`childToolRegistry` via `Subset`/`Clone`), and model pin (`opts.ModelID`). The engine drives execution through this existing seam — no new spawn path, per decision 1.
  - Pool governance intact: `config.WorkflowConfig{Skill, Pool, Workers}`, `PoolConfig{Concurrent, Total, MaxDepth, Tokens}`, runtime `agent.WorkflowPool` mirror, scheduler `RegisterPool/Visible/Admit/StripPredicate/tokenQuotaDenied`. A pipeline references a workflow by name to run under its pool (decision 1); not modified.
- **Three cloned child builders confirmed** (`cmd/fuse/shell.go`, `cmd/fuse/main.go`, `cmd/fuse/research_probe.go`) — per the `patch-every-cloned-child-builder` learning, `pipeline_run` wiring must land in all three; the site count is to be re-derived by grep at build time, not from this list.
- **Config trust boundary** (ADR-0006): the new `pipeline.synthesis.{max_steps,max_fanout,max_depth,max_attempts}` caps are numeric and land under the same `.fuse.local.yml` tighten-only merge enforcement as pool numbers.
- **Verification path** available: the gateway-seam e2e harness (`cmd/fuse/*_e2e_test.go` scripted `httptest`/`LLM_GATEWAY_URL` doubles, e.g. `structured_delegation_e2e_test.go`, `blackboard_gateway_e2e_test.go`) is the pattern to drive `pipeline_run({goal})` synthesis-then-execution and `pipeline_run({definition})` direct execution, per the `verify-tool-loop-at-gateway-seam` learning. TUI screenshot evidence via `internal/tui/harness_test.go` `captureFrame` (`FinalModel().View()` + forced `termenv.TrueColor`, `FUSE_SCREENSHOT_DIR` gate, freeze best-effort), per the `teatest-final-frame-via-finalmodel-view` learning.

No scope adjustments, no obsolescence, no follow-up stubs surfaced this pass beyond the two already-declared deferrals (skill-frontmatter `pipelines:` block; TUI step sub-nodes) which the spec already records for follow-up (`discovered_from: 26`). Auto-capture is disabled this run, so those remain prose notes for a human to file post-merge.
