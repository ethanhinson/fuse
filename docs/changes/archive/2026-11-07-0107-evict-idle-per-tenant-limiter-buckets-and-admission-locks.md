---
id: 107
slug: 'evict-idle-per-tenant-limiter-buckets-and-admission-locks'
title: 'Evict idle per-tenant limiter buckets and admission locks'
status: 'killed'
priority: 'low'
type: 'fix'
deliverable: 'pr'
created: '2026-11-01'
updated: '2026-11-07'
depends_on: []
stacked_on:
related: [103]
discovered_from: [103]
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

The per-tenant limiter keeps a token bucket and an admission lock for every tenant that has ever started a loop. With prefix-admitted tenants the set of tenant ids is operator-unbounded, so the maps grow without limit.

## What changes

- A periodic sweep drops a tenant's bucket once it has refilled to capacity and its admission lock is unheld.
- Test that the maps shrink to zero after idle tenants refill.

## Out of scope

- Changing limit semantics.
- Registry record pruning.

## Why killed

Out of scope after the 2026-11-06 refocus: fuse is a coding harness run as one container per task, with no shared multi-tenant server or harness specs. Merged work (PR #145) targets per-tenant limits, which are being removed.
