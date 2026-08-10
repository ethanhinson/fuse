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
related: [45, 46, 47, 48]
discovered_from: [45, 47]
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

### Folded-in findings from the 0047 dogfood (discovered_from: 47)

Building and dogfooding 0047 (a real `fuse loop-server` loop persisted to Postgres, live-tailed
cross-process) surfaced three concrete gaps that live squarely in 0049's tenant/ownership/liveness
boundary and must be fixed here:

1. **`loop.start` carries no tenant → the loop-server binding is single-tenant.** The durable
   *store* is fully tenant-scoped (0047), but `loop.start`'s params are `{task, model}` only, so
   every loop lands under `event.DefaultTenant` (`_default`), while `loop.send` / `loop.observe`
   already accept a tenant. The asymmetry is a real trap: a `loop.send` under tenant X for a loop
   started via `loop.start` (which is `_default`) returns `runtime: loop not found`. 0049 must make
   tenant (and, with auth, the authenticated identity) a **first-class parameter of `loop.start`**
   so the binding is genuinely multi-tenant and the three verbs agree on the key.

2. **Loop ownership is not enforced on the wire.** 0047 records `OwnerNodeID` in the durable
   registry, but nothing checks that the caller sending/observing a loop is entitled to it. 0049
   binds authenticated identity → `(tenant, owner)` at `loop.start` and enforces it on every
   subsequent `send` / `observe` / `replay` (cross-tenant AND cross-owner denial), returning a
   **distinguishable authz error** rather than `loop not found`.

3. **Stale liveness on hard process death.** A loop-server killed mid-run leaves its registry record
   `live: true` (it never reaches the clean-completion `SetLive(false)`), so a cold instance sees a
   "live" loop no process owns. 0049 needs an **owner liveness lease / heartbeat** (owner TTL) so a
   cold instance can distinguish genuinely-live from abandoned and reap / re-own it — the same
   lease shape docket already uses for claim reclaim.

## Out of scope

To be defined during grooming. The transport itself is change 48; the client SDK is change 50.

## Open questions

- Identity / auth mechanism (tokens, mTLS, OIDC — TBD).
- Where loop_id → owner is recorded, and how it interacts with the durable store (change 47).
- Isolation boundary granularity (per-user, per-org) and cross-tenant denial semantics.
- **From finding 1:** exact shape of tenant/identity on `loop.start` — a param, or derived from the
  authenticated principal — and how the three loop verbs converge on one `(tenant, loop)` key.
- **From finding 2:** authz error taxonomy — is "not yours" distinguishable from "doesn't exist",
  or deliberately collapsed to avoid a loop-id oracle across tenants?
- **From finding 3:** liveness-lease TTL, who renews it (owner heartbeat vs. store-side), and the
  reap/re-own semantics for an expired-lease live loop.
