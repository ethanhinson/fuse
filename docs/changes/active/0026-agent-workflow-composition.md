---
id: 26
slug: agent-workflow-composition
title: Workflow composition — chain, fan-out, and conditional routing
status: proposed
priority: medium
type: feat
created: 2026-08-06
updated: 2026-08-08
depends_on: [23, 24]
related: [12, 23, 24]
discovered_from: []
adrs: []
spec: docs/superpowers/specs/2026-08-08-agent-workflow-composition-design.md
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
| Spec | [2026-08-08-agent-workflow-composition-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-08-agent-workflow-composition-design.md) |
<!-- docket:artifacts:end -->

## Why

fuse's agent orchestration is *emergent*: the LLM decides when to spawn subagents, in what order, and how to combine their results. That is powerful but non-deterministic — there is no way to declare a fixed process ("do A, then B and C in parallel, then D over the combined result") that the harness executes reliably. Map-reduce over search results and conditional branching (if X, do Y; else Z) are encoded ad-hoc in the model's task prompt today. A **pipeline** — a DAG of named steps with declared dependencies, blackboard-backed inputs/outputs, per-step error policy, and structured routing — lets skills and CLI tasks declare a multi-step process the harness runs *without the model making sequencing decisions*. The model still authors each step's content; the DAG is the deterministic structure. The research skill is the motivating case: "5 parallel searches → dedup → synthesize" as declared structure rather than improvisation.

**Naming:** fuse already owns the word *workflow* — `config.WorkflowConfig` (changes 0034/0036, ADR-0007/0009) binds a skill to a spawn **pool policy** + typed workers (resource governance). This change adds a distinct **control-flow** concept, named **`Pipeline`** to avoid overloading one word across two axes. A pipeline *runs under* a workflow, reusing its pool; the two compose.

## What changes

- **New `internal/pipeline/` package** with a `Pipeline`/`Step` type and a YAML/JSON parser that enforces the DAG at parse time (unique step names, resolvable references, **Tarjan cycle detection** — a cyclic pipeline is rejected before any step runs). A pipeline optionally names a `workflow:` to run under its pool.
- **`pipeline_run` built-in tool** — accepts a pipeline definition (inline YAML/JSON, or a registered name) and executes the DAG, returning terminal status and writing a `pipeline.status` blackboard key.
- **Deterministic execution over the existing spawn seam** — the engine (not the model) owns readiness, parallelism, routing, and error policy. Every step whose `depends_on` set is satisfied runs concurrently; each step is one `spawn_agent` call whose prompt is enriched with its `inputs` (blackboard keys). `fanout: N` spawns N parallel instances into a glob output namespace.
- **Blackboard-backed I/O (change 0023)** — a step's `inputs`/`outputs` are blackboard keys; a step result is readable by a later step or a free-form `spawn_agent` and vice versa.
- **Structured step results (change 0024)** — a step may declare an `expects` JSON Schema; the validated structured value lands on the output key. A schema mismatch degrades per 0024 (free text + surfaced note) and is *not* a step error.
- **Per-step error policy** — `on_error: fail | skip | retry(N)`; a hard failure sets `pipeline.status` and stops (or, per policy, skips/retries).
- **Structured conditional routing** — a step may declare ordered `conditions` of the form `{if: {key, op, value}, goto: <step>}` plus an optional `default`, evaluated by the engine against the blackboard. `op ∈ exists | eq | ne | gt | lt | contains | matches`. No expression engine — a fixed, typed operator set; an operator/type mismatch is a false condition, never an error (routing is total).

## Out of scope

- **Skill-frontmatter `pipelines:` block** (a skill body replaced by a declared DAG) — deferred to a follow-up. v1 skills invoke `pipeline_run`.
- **TUI step sub-nodes** (per-step glyphs / elapsed time under a pipeline root) — deferred to a follow-up; v1 relies on the existing per-`spawn_agent` node.
- Dynamic pipelines (steps that add steps at runtime) — static DAG only.
- First-class loops / iteration — expressible via recursive pipeline calls, not a language construct.
- A CEL / arbitrary-expression condition language — fixed operator set only in v1.
- Visual editor; distributed execution — hand-authored YAML/JSON, local goroutines only.
- Renaming the existing `Workflow`/`WorkflowPool` — left untouched; `Pipeline` is additive.

## Design decisions

Design settled through an interactive brainstorm on 2026-08-08 and captured in the linked spec. Five decisions fixed the shape: (1) a **new `Pipeline` concept in `internal/pipeline/`**, separate from the existing pool-governance `Workflow` — not an extension of `WorkflowConfig`, not a breaking rename; a pipeline references a workflow to run under its pool. (2) **v1 ships the full engine** — parallel fan-out, `on_error` modes, **and** conditional routing — deferring only the skill-frontmatter block and TUI sub-nodes. (3) **Conditions use a fixed structured-comparison operator set** (`{key, op, value}`), not a vendored expression language. (4) **Deterministic structure, model-authored content** — the engine drives sequencing; each step is a `spawn_agent` call using 0024's `expects` for structured results. (5) **Hard `depends_on: [23, 24]`** — written against the real blackboard and structured-delegation APIs, no degradation shim; 0024's spec already names 0026 as its sole consumer. Change 0012 (done) is transitive and moved to `related`.
