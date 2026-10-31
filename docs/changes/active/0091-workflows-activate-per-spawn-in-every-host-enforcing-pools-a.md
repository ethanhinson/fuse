---
id: 91
slug: 'workflows-activate-per-spawn-in-every-host-enforcing-pools-a'
title: 'Workflows activate per spawn in every host, enforcing pools and worker tools'
status: 'implemented'
priority: 'medium'
type: 'fix'
deliverable: 'pr'
created: '2026-10-31'
updated: '2026-10-31'
depends_on: [90]
stacked_on:
related: [34, 86]
discovered_from: []
adrs: [60, 61, 67]
spec: 'docs/superpowers/specs/2026-10-31-workflows-activate-per-spawn-in-every-host-enforcing-pools-a-design.md'
plan: 'docs/superpowers/plans/2026-10-31-workflows-activate-per-spawn-in-every-host-enforcing-pools-a.md'
results: 'docs/results/2026-10-31-workflows-activate-per-spawn-in-every-host-enforcing-pools-a-results.md'
auto_groomable:
hold:
branch_prefix:
branch: 'fix/workflows-activate-per-spawn-in-every-host-enforcing-pools-a'
pr: 'https://github.com/ethanhinson/fuse/pull/115'
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-10-31-workflows-activate-per-spawn-in-every-host-enforcing-pools-a-design.md](../../superpowers/specs/2026-10-31-workflows-activate-per-spawn-in-every-host-enforcing-pools-a-design.md) |
| Plan | [2026-10-31-workflows-activate-per-spawn-in-every-host-enforcing-pools-a.md](../../superpowers/plans/2026-10-31-workflows-activate-per-spawn-in-every-host-enforcing-pools-a.md) |
| Results | [2026-10-31-workflows-activate-per-spawn-in-every-host-enforcing-pools-a-results.md](../../results/2026-10-31-workflows-activate-per-spawn-in-every-host-enforcing-pools-a-results.md) |
| ADRs | [ADR-0060](../../adrs/0060-harnessspec-proto-is-the-portable-harness-definition-client.md), [ADR-0061](../../adrs/0061-a-loop-runs-a-client-harnessspec-layered-over-the-operator-c.md), [ADR-0067](../../adrs/0067-workflows-activate-per-spawn-do-not-nest-and-root-at-a-coord.md) |
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

## Reconcile log

### 2026-10-31

Unchanged against origin/main (7964f40); dependency 90 is merged.

## Build evidence

<!-- docket:build-evidence:start -->
command:  make test
result:   green
head_sha: 56a62339c742d9a16fd46770c21a27aa71a61c58
ran_at:   2026-10-31T20:56:20Z
<!-- docket:build-evidence:end -->
