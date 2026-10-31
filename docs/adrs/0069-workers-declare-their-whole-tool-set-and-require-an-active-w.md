---
id: 69
slug: 'workers-declare-their-whole-tool-set-and-require-an-active-w'
title: 'Workers declare their whole tool set and require an active workflow'
status: 'Accepted'
date: '2026-10-31'
supersedes: [67]
reverses: []
relates_to: []
change: 95
---

## Context

ADR-0067 settled how workflows attach to spawns: a per-spawn workflow argument, no nesting, a pipeline coordinator node, and blackboard tools for worker-bound children. Its decision 5 said a worker name used outside any active workflow was ignored, and it let a worker declare an empty tool allowlist. Both left gaps: an ignored worker name silently produced an unbound child the author could not diagnose from a hosted loop, and an empty allowlist meant a worker inherited the parent's tools, so the documented rule "omit spawn_agent to prevent nesting" did not hold. Change 95 closes both gaps.

## Decision

Decisions 1-4 of ADR-0067 are restated unchanged:

1. A workflow activates per spawn through the workflow argument, and accounting roots at that spawn.
2. Workflows do not nest.
3. A pipeline runs under a coordinator node.
4. Worker-bound children receive the blackboard tools.

Decision 5 is replaced:

- (a) Unknown workflow and worker names are refused before any node is created (unchanged).
- (b) A worker named outside any active workflow is refused, by spawn_agent and pipeline steps alike, with an error that says to pass workflow (agent.ErrWorkerOutsideWorkflow). It is no longer ignored. The synthesis palette offers only workers it can bind.
- (c) Every worker declares at least one tool. The config validator, the harness validator and the shape rules (repeated.min_items) refuse an empty allowlist at the same field path workflows["<wf>"].workers["<worker>"].tools, and the runtime refuses it again. A worker's allowlist is its whole tool set, so "omit spawn_agent to prevent nesting" holds.

## Consequences

Authors get a visible, actionable error in the tool result instead of a silently unbound child. Worker tool sets are explicit and auditable, and nesting prevention is enforceable by omission. The CLI and hosted paths agree because both run Config.Problems. Existing configs with an empty worker allowlist, or that name workers without a workflow, now fail validation and must be updated.

## Alternatives considered

- An inherit_all: true worker flag - rejected: out of scope, and an inheriting worker is not a worker.
- Keeping "ignored" and only logging - rejected: the author cannot see a log from a hosted loop; the tool result is the one channel the model and author share.
- Validating only in the harness validator - rejected: the CLI and hosted path must agree, which Config.Problems guarantees.
