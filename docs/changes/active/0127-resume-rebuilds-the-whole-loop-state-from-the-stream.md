---
id: 127
slug: 'resume-rebuilds-the-whole-loop-state-from-the-stream'
title: 'Resume rebuilds the whole loop state from the stream'
status: 'in-progress'
priority: 'high'
type: 'fix'
deliverable: 'pr'
created: '2026-11-03'
updated: '2026-11-04'
depends_on: []
stacked_on:
related: [54, 108, 122, 126]
discovered_from: []
adrs: [81]
spec: 'docs/superpowers/specs/2026-11-03-resume-rebuilds-the-whole-loop-state-from-the-stream-design.md'
plan: 'docs/superpowers/plans/2026-11-04-resume-rebuilds-the-whole-loop-state-from-the-stream.md'
results: 'docs/results/2026-11-04-resume-rebuilds-the-whole-loop-state-from-the-stream-results.md'
auto_groomable:
hold:
branch_prefix:
branch: 'fix/resume-rebuilds-the-whole-loop-state-from-the-stream'
pr:
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-11-03-resume-rebuilds-the-whole-loop-state-from-the-stream-design.md](../../superpowers/specs/2026-11-03-resume-rebuilds-the-whole-loop-state-from-the-stream-design.md) |
| Plan | [2026-11-04-resume-rebuilds-the-whole-loop-state-from-the-stream.md](../../superpowers/plans/2026-11-04-resume-rebuilds-the-whole-loop-state-from-the-stream.md) |
| Results | [2026-11-04-resume-rebuilds-the-whole-loop-state-from-the-stream-results.md](../../results/2026-11-04-resume-rebuilds-the-whole-loop-state-from-the-stream-results.md) |
| ADRs | [ADR-0081](../../adrs/0081-resume-is-a-full-fold-of-the-stream-orphans-are-failed-durab.md) |
<!-- docket:artifacts:end -->

## Why

Resume replays the stream but folds only the four transcript kinds. Child turns bleed into the root transcript because nothing filters by node; children that were mid-flight leave an assistant message with unanswered tool calls (an invalid request for every provider); lifetime budgets (`max_spawns`, `session_tokens`, pool totals) reset to zero; compaction summaries are lost so the first resumed call re-inflates the full raw history. A loop that moves between replicas is therefore not the same loop.

## What changes

`reconstruct` grows into a full fold: root-node filtering, a tree view from `spawn.start/done`, orphan repair (synthetic `spawn.done` and `tool.result` for calls that never completed, recorded durably on resume), budgets seeded from the stream, and summaries carried in `context.summarize` and re-inserted. Covered by golden-stream tests and a two-runtime move test.

## Out of scope

Re-dispatching orphaned children or pipelines (they are failed, not retried); durable blackboard (0131); pipeline run events (0132); remote spawning (0137).

## Reconcile log

### 2026-11-04

Reconciled against origin/main f01f122. All spec code references verified present. No design changes needed.
