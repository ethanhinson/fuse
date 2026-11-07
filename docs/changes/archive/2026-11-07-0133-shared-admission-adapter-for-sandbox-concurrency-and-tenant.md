---
id: 133
slug: 'shared-admission-adapter-for-sandbox-concurrency-and-tenant'
title: 'Shared admission adapter for sandbox concurrency and tenant start rate'
status: 'killed'
priority: 'medium'
type: 'feat'
deliverable: 'pr'
created: '2026-11-03'
updated: '2026-11-07'
depends_on: []
stacked_on:
related: [77, 103, 106, 107]
discovered_from: []
adrs: []
spec: 'docs/superpowers/specs/2026-11-03-shared-admission-adapter-for-sandbox-concurrency-and-tenant-design.md'
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
| Spec | [2026-11-03-shared-admission-adapter-for-sandbox-concurrency-and-tenant-design.md](../../superpowers/specs/2026-11-03-shared-admission-adapter-for-sandbox-concurrency-and-tenant-design.md) |
<!-- docket:artifacts:end -->

## Why

`max_inflight`, `max_inflight_per_tenant` and `starts_per_minute` are enforced by in-process semaphores and buckets, so with N replicas the fleet admits N times what the operator configured; only the live-loop cap reads the shared registry. Operators cannot express a fleet-wide limit today.

## What changes

An `Admitter` seam for counted, time-windowed admission with a local implementation (today's semantics) and a Postgres implementation (advisory-lock-free counter rows with expiry), selected by config; the sandbox gate and the tenant limiter use it; conformance suite and a two-runtime test that proves the combined count respects the cap.

## Out of scope

Model-call throughput rate limiting (`internal/ratelimit`, provider RPM/TPM — per-process by design, documented); priority/fairness changes; removing the local implementation.

## Why killed

Out of scope after the 2026-11-06 refocus: fuse is a coding harness run as one container per task, with no shared multi-tenant server or harness specs. Only needed for the shared server or fleet.
