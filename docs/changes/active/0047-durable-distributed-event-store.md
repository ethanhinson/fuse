---
id: 47
slug: durable-distributed-event-store
title: Durable / distributed event store — survives restart and is shared across instances
status: proposed
priority: medium
type: feat
created: 2026-08-10
updated: 2026-08-10
depends_on: [46]
related: [43, 45, 46]
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

Once the event store is per-loop instance state (change 46), the persistence itself is still
per-session JSONL files scoped to a single process's local disk. A hosted service must let a
loop's durable event history **survive process restart** and be **shared across service
instances**, so a client can reattach across deploys and restarts and miss nothing — the durable
reattach story the loop-server's replay cursor already promises, extended beyond a single
process's lifetime. This is hosting milestone (3), durable / distributed persistence. Needs
brainstorming: the storage backend, the schema/keying by loop_id, retention, and how it plugs in
behind the existing `EventStore` interface without disturbing the per-loop ownership from 46.

## What changes

To be designed during grooming. At a sketch: a durable/distributed `EventStore` implementation
(beyond per-session local files) that persists each loop's stream keyed by loop_id, survives
restart, and is reachable from any instance — enabling reattach-across-deploys — behind the same
`event.EventStore` interface so the Runtime seam is unchanged.

## Out of scope

To be defined during grooming.

## Open questions

- Storage backend and its operational shape (embedded vs. external service).
- Schema / keying by loop_id, ordering guarantees, and retention.
- How it composes with the per-loop store ownership landed in change 46.
