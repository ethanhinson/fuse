---
id: 9
slug: queue-bound-visibility-global-pool-only
title: Queue-bound visibility governs the global pool only; workflow pools retain 0034's strip-at-Concurrent
status: Accepted
date: 2026-08-06
supersedes: []
reverses: []
relates_to: [7, 8]
change: 36
---

## Context

Change 0036 folds the scheduler in as the admission backend, and its spec pulls in
two directions that meet head-on for workflow pools:

- **Acceptance 3** redefines `spawn_agent` visibility: the tool is visible iff
  admission would *grant or queue within bound*. This moves the schema strip from
  "at the concurrency cap" to "at the queue bound", so the model can commit to
  spawns that will *queue* rather than being stripped the instant the active cap is
  full.
- **Acceptance 6** requires the existing 0033 and 0034 acceptance suites to keep
  passing with the scheduler as the backend.

These pull apart specifically for **workflow pools**. 0034's contract strips a
workflow subtree's `spawn_agent` at subtree `running + pending >= pool.Concurrent`,
and queued waiters materialize *pending* tree nodes. So honoring queue-to-bound
visibility inside a workflow pool would count those pending waiters as headroom and
break 0034's strip-at-`Concurrent` semantics — an Acceptance 3 vs Acceptance 6
conflict that cannot be satisfied simultaneously for workflow-scoped policy.

## Decision

The queue-to-bound visibility shift applies to the **global / implicit session pool
only**.

- **Global pool:** the active-cap now yields verdict `Queued` — the schema stays
  visible and the spawn parks in the fair queue. The strip engages only at the pool
  **queue bound** (`ceil(queue_bound × slots)`, verdict `OverBound`), and visibility
  returns as the queue drains. This is the new queue semantics Acceptance 3 asks for.
- **Workflow pools:** deliberately retain 0034's tighter behavior — strip at subtree
  `running + pending >= pool.Concurrent`. A workflow pool's queue headroom is
  therefore reachable only by racing calls, which the call-time backstop denies with
  a new error identity, `ErrQueueBoundExceeded` (necessarily new — no queue bound
  existed before 0036).

This resolves the Acceptance 3 / Acceptance 6 tension in favor of **backward
compatibility for workflow-scoped policy** while granting the global pool the new
queue semantics.

## Consequences

- Exactly **one deliberate 0033-visible behavior change**: the global cap now queues
  (schema visible, spawn parks) instead of stripping.
- **Zero 0034-visible changes**: workflow-pool strip-at-`Concurrent`, sibling
  isolation, permanent budget/total/quota terms, and static depth semantics are all
  unchanged.
- A new error identity `ErrQueueBoundExceeded` enters the surface — unavoidable, as
  no queue bound existed before this change — raised by the call-time backstop when a
  workflow pool's queue headroom is reached by racing calls.
- The asymmetry (global pool queues to bound; workflow pools strip at `Concurrent`)
  is a permanent, documented divergence between the two pool kinds, and readers of
  the scheduler must not assume uniform visibility semantics across them.

Related: ADR-0007 (single admission authority) and ADR-0008 (rate-gate semantics),
both from change 0036. Evidence: `internal/agent/scheduler.go` `Visible`/`Admit`
doc comments, `TestVisibleActiveCapQueuesNotStrips`, `TestVisibleQueueBoundFlip`,
and review finding O-1.
