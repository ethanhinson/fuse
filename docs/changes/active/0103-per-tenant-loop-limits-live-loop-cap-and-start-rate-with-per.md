---
id: 103
slug: 'per-tenant-loop-limits-live-loop-cap-and-start-rate-with-per'
title: 'Per-tenant loop limits: live-loop cap and start rate with per-tenant overrides'
status: 'in-progress'
priority: 'medium'
type: 'feat'
deliverable: 'pr'
created: '2026-11-01'
updated: '2026-11-01'
depends_on: []
stacked_on:
related: []
discovered_from: []
adrs: []
spec: 'docs/superpowers/specs/2026-11-01-per-tenant-loop-limits-live-loop-cap-and-start-rate-with-per-design.md'
plan: 'docs/superpowers/plans/2026-11-01-per-tenant-loop-limits.md'
results: 'docs/results/2026-11-01-per-tenant-loop-limits-results.md'
auto_groomable:
hold:
branch_prefix:
branch: 'feat/per-tenant-loop-limits-live-loop-cap-and-start-rate-with-per'
pr:
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-11-01-per-tenant-loop-limits-live-loop-cap-and-start-rate-with-per-design.md](../../superpowers/specs/2026-11-01-per-tenant-loop-limits-live-loop-cap-and-start-rate-with-per-design.md) |
| Plan | [2026-11-01-per-tenant-loop-limits.md](../../superpowers/plans/2026-11-01-per-tenant-loop-limits.md) |
| Results | [2026-11-01-per-tenant-loop-limits-results.md](../../results/2026-11-01-per-tenant-loop-limits-results.md) |
<!-- docket:artifacts:end -->

## Why

throughput limits (`throughput.*`) are per loop; nothing bounds how many loops one tenant may keep alive or how fast it may start them on a shared networked server, so one credential holder can exhaust the process for every other tenant. `internal/runtime/inproc.go` notes per-tenant policy as a follow-up.

## What changes

- `LoopServerConfig.Limits` (`loop_server.limits`): `default {max_live_loops, starts_per_minute}` and `tenants: {<tenant>: {...}}` overrides; zero means unlimited (today's behaviour).
- Enforced at the Connect edge after authentication: a `StartLoop` beyond `starts_per_minute` (token bucket per tenant) or when the tenant's live-loop count (registry records with `Live` and an unexpired lease) is at `max_live_loops` returns `CodeResourceExhausted` with a message naming the limit and, for the rate case, a `Retry-After` header in seconds.
- Counters are per process; the registry count is the durable truth for the cap.
- `GetCapabilitiesResponse` gains `Limits {max_live_loops, starts_per_minute}` for the caller's tenant.
- ADR recording: per-tenant limits are an operator floor at the edge, process-local for rate, registry-backed for the cap.

## Out of scope

spend quotas (tokens per tenant per day); per-subject limits; distributed rate limiting across processes; limits as token claims (file as later).

## Reconcile log

### 2026-11-01

Unchanged against origin/main 736e994.
