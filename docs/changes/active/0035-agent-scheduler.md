---
id: 35
slug: agent-scheduler
title: Agent scheduler — global queue, cross-pool fairness, and turn-level throughput limits
status: proposed
priority: high
type: feat
created: 2026-08-06
updated: 2026-08-06
depends_on: [34]
related: [33, 34]
discovered_from: [34]
adrs: [7]
spec: docs/superpowers/specs/0035-agent-scheduler.md
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
| Artifact | Link |
|---|---|
| Spec | [0035-agent-scheduler.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/0035-agent-scheduler.md) |
| ADRs | [ADR-0007](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs) |
<!-- docket:artifacts:end -->

## Why

After 0033 (global brakes) and 0034 (workflow pools), three gaps remain: the spawn
queue is unbounded with arrival-order draining and no fairness across subtrees (one
15-child batch convoys everything behind it); pool reservations are caps with nothing
preventing transient starvation; and concurrency is not throughput — 16 slots say
nothing about requests or tokens per minute hitting the gateway, so tight-loop agents
can exceed provider rate limits and burn spend at unbounded velocity while violating no
configured cap. Rather than a third bespoke mechanism, the architecture decision
(ADR-0007) is one Scheduler component as the single admission, queueing, and throughput
authority — seeded by 0034's pool accounting, completed here.

## What changes

- **Scheduler component** finalized as the sole owner of slots, budgets, queue, and
  strip decisions; 0033/0034 stripping collapses into one visibility predicate
  ("admission would deny ⇒ schema absent").
- **Bounded, fair queue**: FIFO per pool, round-robin across pools (freeform spawns =
  one implicit pool); per-pool bound of 2× slots, overflow strips the schema, call-time
  deny stays as the race backstop.
- **Turn-level rate gate**: token-bucket rpm/tpm at `Adapter.Complete` (the single
  choke point), global with per-provider overrides; unset = unlimited.
- **Hard token quotas**: per-workflow `pool.tokens` and optional session ceiling;
  exhaustion strips spawn_agent in scope and injects a machine-authored warning line so
  agents conclude with what they have. No mid-turn aborts.
- **Observability counters**: queued/slots/rate/quota per pool for the status bar and
  agents view.

Design detail and acceptance criteria in the linked spec.

## Out of scope

- Weighted/priority fairness; guaranteed min-slot reservations.
- Dollar-cost accounting; cross-session spend tracking; adaptive 429-driven throttling.

## Open questions

- Session-ceiling halt polish (strip + warning vs one explicit synthesis prompt).
- `queue_bound` multiplier vs absolute; whether the rate gate should default on.

## Reconcile log
