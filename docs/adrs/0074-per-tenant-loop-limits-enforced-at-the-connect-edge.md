---
id: 74
slug: 'per-tenant-loop-limits-enforced-at-the-connect-edge'
title: 'Per-tenant loop limits enforced at the Connect edge'
status: 'Accepted'
date: '2026-11-01'
supersedes: []
reverses: []
relates_to: []
change: 103
---

## Context

The loop server is multi-tenant. Without limits, one tenant can start unbounded live loops or flood loop starts, exhausting shared capacity for every other tenant. Operators need a floor that bounds each tenant's live-loop count and start rate, enforced in a place tenants cannot bypass or influence.

## Decision

Per-tenant loop limits are an operator floor enforced at the Connect edge, after authentication resolves the tenant.

- Start rate: a process-local token bucket per tenant.
- Live-loop cap: registry-backed - the count of Live loop records with an unexpired lease - evaluated under a per-tenant admission lock that also counts in-flight reservations, so concurrent starts cannot overshoot the cap.
- Configuration: limits are read only from the trusted home config (`loop_server.limits`); zero means unlimited.
- A resuming Send is admitted through the same limiter as a fresh start.

## Consequences

Enables a simple, tenant-unforgeable bound on live loops and start rate with no new infrastructure.

Costs:
- The registry count is O(tenant history) per capped start.
- An in-flight start can briefly be double-counted (reservation plus record), causing fail-closed over-refusal rather than overshoot.
- Per-tenant buckets and admission locks are never evicted, so memory grows with the number of distinct tenants seen by a process.
- The start rate is per process, not distributed: N server processes admit up to N times the configured rate.

## Alternatives considered

- Limits carried as token claims: rejected - limits would be minted by whoever issues tokens rather than set by the operator, and an operator floor must come from trusted local config.
- Distributed rate limiting (shared store): rejected for now - adds a coordination dependency and latency on every start; per-process buckets are sufficient as a floor.
- Per-subject limits: rejected - the capacity-isolation boundary is the tenant; per-subject limits add configuration surface without protecting other tenants.
