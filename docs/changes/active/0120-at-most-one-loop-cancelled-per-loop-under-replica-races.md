---
id: 120
slug: 'at-most-one-loop-cancelled-per-loop-under-replica-races'
title: 'At most one loop.cancelled per loop under replica races'
status: 'proposed'
priority: 'low'
type: 'fix'
deliverable: 'pr'
created: '2026-11-03'
updated: '2026-11-03'
depends_on: []
stacked_on:
related: [117]
discovered_from: [117]
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

In rare races between replicas, two `loop.cancelled` events can be written without the second being marked superseded, so `fuse_loops_cancelled_total` counts that cancel twice. Readers take the first event, so behaviour is correct; only the count is off.

## What changes

- Append the terminal conditionally at the store (append only if no `loop.cancelled` exists), or mark later ones superseded so the projection skips them.
- Add a two-replica race test.
- Also, in the legacy path with no durable store, surface a failed terminal append instead of returning seq 0 silently.

## Out of scope

Registry schema changes.
