---
id: 106
slug: 'loop-registry-live-count-method-for-the-per-tenant-live-loop'
title: 'Loop registry live-count method for the per-tenant live-loop cap'
status: 'proposed'
priority: 'medium'
type: 'feat'
deliverable: 'pr'
created: '2026-11-01'
updated: '2026-11-01'
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

The per-tenant live-loop cap counts a tenant's live loops by listing its whole registry history on every capped start. History is never pruned, so each capped start costs O(history) rows on the Postgres store and O(history) file reads on the filesystem store.

## What changes

- Optional RegistryCounter interface on loop registries: CountLive(ctx, tenant, now).
- Postgres implementation: SELECT count(*) FROM loops WHERE tenant_id=$1 AND live AND (lease_expiry IS NULL OR lease_expiry > $2), with a partial index on (tenant_id) WHERE live.
- The tenant limiter prefers CountLive when the store implements it, falling back to List.
- Conformance test shared by both stores.

## Out of scope

- Pruning finished loop records.
- Cross-replica exact admission.
