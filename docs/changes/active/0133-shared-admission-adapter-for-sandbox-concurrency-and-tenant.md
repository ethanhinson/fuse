---
id: 133
slug: 'shared-admission-adapter-for-sandbox-concurrency-and-tenant'
title: 'Shared admission adapter for sandbox concurrency and tenant start rate'
status: 'proposed'
priority: 'medium'
type: 'feat'
deliverable: 'pr'
created: '2026-11-03'
updated: '2026-11-03'
depends_on: []
stacked_on:
related: [77, 103, 106, 107]
discovered_from: []
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

`max_inflight`, `max_inflight_per_tenant` and `starts_per_minute` are enforced by in-process semaphores and buckets, so with N replicas the fleet admits N times what the operator configured; only the live-loop cap reads the shared registry. Operators cannot express a fleet-wide limit today.

## What changes

An `Admitter` seam for counted, time-windowed admission with a local implementation (today's semantics) and a Postgres implementation (advisory-lock-free counter rows with expiry), selected by config; the sandbox gate and the tenant limiter use it; conformance suite and a two-runtime test that proves the combined count respects the cap.

## Out of scope

Model-call throughput rate limiting (`internal/ratelimit`, provider RPM/TPM — per-process by design, documented); priority/fairness changes; removing the local implementation.
