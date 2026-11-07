---
id: 139
slug: 'segment-index-is-safe-with-concurrent-writers'
title: 'Segment index is safe with concurrent writers'
status: 'killed'
priority: 'low'
type: 'fix'
deliverable: 'pr'
created: '2026-11-03'
updated: '2026-11-07'
depends_on: []
stacked_on:
related: [128, 126]
discovered_from: [128]
adrs: []
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
<!-- docket:artifacts:end -->

## Why

The compaction segment index assumes one writer per loop. If two replicas archive segments for the same loop at once (possible before ownership is fenced, or during failover), an index entry can be lost.

## What changes

- Make segment-index updates atomic per entry: an append-only index, or compare-and-set on the index, in every artifact backend.
- A conformance test with concurrent writers.

## Out of scope

Changing segment format.

## Why killed

Out of scope after the 2026-11-06 refocus: fuse is a coding harness run as one container per task, with no shared multi-tenant server or harness specs. Only arises with multiple replicas writing one loop.
