---
id: 49
slug: auth-multi-tenancy
title: Auth / multi-tenancy — loop_id ownership and per-tenant isolation for the deployed service
status: proposed
priority: medium
type: feat
created: 2026-08-10
updated: 2026-08-10
depends_on: [48]
related: [45, 46, 48]
discovered_from: [45]
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
<!-- docket:artifacts:end -->

## Why

The networked binding (change 48) is single-tenant — every caller shares one trust domain, an
explicit non-goal carried forward from 0045. A **deployed** service that many users' apps target
needs authentication and multi-tenancy: each caller may only start, drive, observe, and replay the
loops it owns, and loops are isolated per tenant. Needs brainstorming: the identity/auth model,
loop_id ownership, and per-tenant isolation boundaries over the networked seam.

## What changes

To be designed during grooming. At a sketch: an auth layer over the networked binding that
establishes caller identity, records loop_id ownership at `loop.start`, and enforces per-tenant
isolation on every subsequent operation (send / observe / replay), so tenants cannot see or drive
each other's loops.

## Out of scope

To be defined during grooming. The transport itself is change 48; the client SDK is change 50.

## Open questions

- Identity / auth mechanism (tokens, mTLS, OIDC — TBD).
- Where loop_id → owner is recorded, and how it interacts with the durable store (change 47).
- Isolation boundary granularity (per-user, per-org) and cross-tenant denial semantics.
