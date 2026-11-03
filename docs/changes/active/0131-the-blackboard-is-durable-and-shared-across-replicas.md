---
id: 131
slug: 'the-blackboard-is-durable-and-shared-across-replicas'
title: 'The blackboard is durable and shared across replicas'
status: 'proposed'
priority: 'high'
type: 'feat'
deliverable: 'pr'
created: '2026-11-03'
updated: '2026-11-03'
depends_on: [127]
stacked_on:
related: [23]
discovered_from: []
adrs: []
spec: 'docs/superpowers/specs/2026-11-03-the-blackboard-is-durable-and-shared-across-replicas-design.md'
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
| Spec | [2026-11-03-the-blackboard-is-durable-and-shared-across-replicas-design.md](../../superpowers/specs/2026-11-03-the-blackboard-is-durable-and-shared-across-replicas-design.md) |
<!-- docket:artifacts:end -->

## Why

The blackboard is the only coordination channel between a loop's agents and the data plane of pipelines, and it is a per-process map: no event, no persistence. A move loses every key and every waiter; a child running elsewhere would see a different board. It needs a backend seam and a durable form, like the transcript already has.

## What changes

Blackboard writes become loop-stream events (`blackboard.put`/`blackboard.delete`), the in-memory map becomes a cache fed by the stream, `Wait` becomes a stream subscription so it works from any process holding the store, and `Resume` folds the board back. A `BlackboardBackend` interface with in-memory and stream-backed implementations plus a conformance suite.

## Out of scope

Cross-loop boards; changing the tool surface (`blackboard_put/get/wait/...` schemas); pipeline run records (0132).
