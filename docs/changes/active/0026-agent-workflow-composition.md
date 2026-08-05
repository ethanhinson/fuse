---
id: 26
slug: agent-workflow-composition
title: Workflow composition — chain, fan-out, and conditional routing
status: proposed
priority: medium
type: feat
created: 2026-08-06
updated: 2026-08-06
depends_on: [12, 23, 24]
related: [23, 24]
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

fuse's agent model is emergent: the LLM decides when to spawn subagents, in what order, and how to use their results. This is powerful but unpredictable — there is no way to express a fixed workflow ("always do A, then B and C in parallel, then D with the combined result") that the harness executes deterministically. Patterns like chain-of-thought with tool use, map-reduce over search results, and conditional branching (if file X exists, do Y; else Z) are currently encoded ad-hoc in the model's task prompt. A lightweight workflow composition layer — expressed as a simple DAG (directed acyclic graph) of named steps with inputs, outputs, and routing rules — would let skills and CLI tasks declare reliable multi-step processes that the harness executes without the LLM making sequencing decisions on the fly.

## What changes

- **`Workflow` type** in `internal/agent/` (or a new `internal/workflow/` package): a DAG of `Step` nodes:
  ```go
  type Step struct {
      Name       string
      Agent      string        // persona or model override
      Prompt     string        // task for this step
      Inputs     []string      // blackboard keys to inject into the prompt
      Outputs    []string      // blackboard keys to write results to
      DependsOn  []string      // step names that must complete first
      Next       string        // default next step
      Conditions []Condition   // conditional routing
  }
  ```
- **`workflow_run` tool**: a new built-in tool that accepts a workflow definition (YAML or JSON) and executes it step-by-step. Steps that list no `DependsOn` run immediately; steps whose dependencies are all satisfied run in parallel goroutines; steps with conditions evaluate the condition against the blackboard and route to the matching next step.
- **Deterministic execution**: the workflow engine runs steps without LLM sequencing decisions — the DAG structure is the plan. Each step produces its own `spawn_agent` call under the hood, with the step prompt enriched by its `Inputs` from the blackboard.
- **Error handling**: configurable per-step: `on_error: fail | skip | retry(N)`. A failing step that stops the workflow records the failure and sets a `workflow.status` key on the blackboard.
- **Integration with skills**: a skill's frontmatter can declare a `workflow:` block, replacing the model-authored procedural body with a DAG that the `workflow_run` tool executes. The existing procedural skill body remains the default.
- **TUI visibility**: the agent tree shows workflow steps as sub-nodes under a workflow root, with status glyphs and elapsed time per step.

## Out of scope

- Dynamic workflows (steps that add steps at runtime) — static DAG only.
- Loops / iteration — expressible via recursive workflow calls but not first-class.
- Visual workflow editor — YAML/JSON authored in a text editor.
- Distributed execution across machines — local subagent goroutines only.

## Research notes (input for the brainstorm)

Workflow composition is the middle ground between fully emergent orchestration (what fuse does today via LLM + `spawn_agent`) and fully deterministic pipelines (what LangGraph, Temporal, and Prefect do). The research skill is the natural first use case: instead of the model deciding how to diversify queries, the workflow would declare "parallel 5 search steps → dedup step → synthesize step." The model would still handle the content, but the structure would be deterministic. Key design tension: workflow steps should be composable with the blackboard (inputs/outputs are blackboard keys), so a workflow step can write a result that a subsequent free-form spawn_agent call reads, and vice versa. The DAG must be acyclic enforced at parse time (Tarjan's algorithm for cycle detection). Error handling is where workflows beat pure LLM orchestration: `on_error: retry(3)` is more reliable than hoping the model re-spawns a failed agent.
