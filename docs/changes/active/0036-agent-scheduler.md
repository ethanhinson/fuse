---
id: 36
slug: agent-scheduler
title: Agent scheduler — global queue, cross-pool fairness, and turn-level throughput limits
status: implemented
priority: high
type: feat
created: 2026-08-06
updated: 2026-08-06
depends_on: [34]
related: [33, 34]
discovered_from: [34]
adrs: [7, 8, 9]
spec: docs/superpowers/specs/0036-agent-scheduler.md
plan: docs/superpowers/plans/0036-agent-scheduler.md
results: docs/results/2026-08-06-agent-scheduler-results.md
trivial: false
auto_groomable:
branch: feat/agent-scheduler
pr: https://github.com/ethanhinson/fuse/pull/21
blocked_by:
claimed_at: 2026-08-07T00:10:00Z
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [0036-agent-scheduler.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/0036-agent-scheduler.md) |
| Plan | [0036-agent-scheduler.md](https://github.com/ethanhinson/fuse/blob/feat/agent-scheduler/docs/superpowers/plans/0036-agent-scheduler.md) |
| Results | [2026-08-06-agent-scheduler-results.md](https://github.com/ethanhinson/fuse/blob/feat/agent-scheduler/docs/results/2026-08-06-agent-scheduler-results.md) |
| PR | [#21](https://github.com/ethanhinson/fuse/pull/21) |
| ADRs | [ADR-0007](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0007-scheduler-single-admission-authority.md), [ADR-0008](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0008-rate-gate-per-logical-request-tpm-steady-state.md), [ADR-0009](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0009-queue-bound-visibility-global-pool-only.md) |
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

- **2026-08-06** — Reconciled against `origin/main` (c595d5e, post-0034 merge). All
  premises hold: `Adapter.Complete` remains the single dispatch choke point
  (`internal/model/adapter.go:212`); per-node `TokensIn`/`TokensOut` counters and
  `UpdateTokens` exist; the channel semaphore + `YieldSlot`/`UnyieldSlot` mechanics are
  in `internal/agent/tree.go`. 0034 landed its seeds as tree-level pieces — subtree
  accounting (`SubtreeActiveCounts`/`SubtreeSpawnCount`/`WorkflowRootOf` in
  `subtree.go`), `NewWorkflowStripPredicate` composed with the global predicate via
  `NewOrPredicate` (`strip.go`), and an atomic per-call total-quota reservation
  backstop — not as a proto-Scheduler type; no `Scheduler` component exists yet, so
  this change creates it and folds those pieces in, exactly the scope as written.
  ADR-0007 (scheduler as single admission authority) is already recorded. No scope
  changes; spec stands as written.
