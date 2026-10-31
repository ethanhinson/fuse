---
id: 91
slug: 'workflows-activate-per-spawn-in-every-host-enforcing-pools-a'
title: 'Workflows activate per spawn in every host, enforcing pools and worker tools'
status: 'proposed'
priority: 'medium'
type: 'fix'
deliverable: 'pr'
created: '2026-10-31'
updated: '2026-10-31'
depends_on: [90]
stacked_on:
related: [34, 86]
discovered_from: []
adrs: [60, 61]
spec:
plan:
results:
auto_groomable:
hold:
branch_prefix:
branch:
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| ADRs | [ADR-0060](../../adrs/0060-harnessspec-proto-is-the-portable-harness-definition-client.md), [ADR-0061](../../adrs/0061-a-loop-runs-a-client-harnessspec-layered-over-the-operator-c.md) |
<!-- docket:artifacts:end -->

## Why

Workflow activation is only built by the research probe. In shell, one-shot and both hosted bindings, the child builder never resolves worker tools, never binds workers and never registers a pool. So a spec's workflow pools and worker tool allowlists are accepted but inert, against ADR-0061's promise that workflows and pools inherit the loop's config.

## What changes

- `spawn_agent` and `pipeline_run` gain an optional `workflow` argument. The spawned child becomes that workflow's root, its pool is registered there, and its subtree resolves `worker` against that workflow.
- Nested activation inside an active workflow subtree is refused.
- Activation and worker resolution are shared helpers used by every binding.
- The research probe's root activation stays as the one root-level case.
- ADR for per-spawn workflow activation.

## Out of scope

- A spec-level root `workflow` field (it would change the portable spec).
- New pool limits or scheduler policy.
- Token pool budgets (enforced by the budgets change this builds on).
