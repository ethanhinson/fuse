---
id: 126
slug: 'fenced-loop-ownership-and-cross-replica-activity-in-the-loop'
title: 'Fenced loop ownership and cross-replica activity in the loop registry'
status: 'in-progress'
priority: 'high'
type: 'fix'
deliverable: 'pr'
created: '2026-11-03'
updated: '2026-11-03'
depends_on: []
stacked_on:
related: [47, 54, 106, 117, 120]
discovered_from: []
adrs: [79]
spec: 'docs/superpowers/specs/2026-11-03-fenced-loop-ownership-and-cross-replica-activity-in-the-loop-design.md'
plan: 'docs/superpowers/plans/2026-11-03-fenced-loop-ownership-and-cross-replica-activity-in-the-loop.md'
results:
auto_groomable:
hold:
branch_prefix:
branch: 'fix/fenced-loop-ownership-and-cross-replica-activity-in-the-loop'
pr:
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-11-03-fenced-loop-ownership-and-cross-replica-activity-in-the-loop-design.md](../../superpowers/specs/2026-11-03-fenced-loop-ownership-and-cross-replica-activity-in-the-loop-design.md) |
| Plan | [2026-11-03-fenced-loop-ownership-and-cross-replica-activity-in-the-loop.md](../../superpowers/plans/2026-11-03-fenced-loop-ownership-and-cross-replica-activity-in-the-loop.md) |
| ADRs | [ADR-0079](../../adrs/0079-loop-ownership-is-a-fenced-lease-with-compare-and-set-on-the.md) |
<!-- docket:artifacts:end -->

## Why

The registry records who owns a live loop but never checks it: `Heartbeat`, `SetLive` and `Register` overwrite `owner_node_id` unconditionally, so a replica that stalls past its lease and wakes after another replica resumed the loop re-takes ownership and both drive one stream. The idle reaper is also owner-local, so a loop that callers keep observing or steering through other replicas can be reaped as idle. Both break mode (b) silently.

## What changes

Ownership becomes a compare-and-set on the registry contract: a heartbeat or liveness flip from a node that is not the recorded owner is refused with a new sentinel, resume claims an expired lease atomically, and the runtime stops driving a loop the moment its heartbeat is refused. Activity from any replica (Observe, Send, Steer relayed through the stream) is recorded on the registry so the owner's idle reaper counts it. Both backends and the conformance suite gain the new cases; a two-runtime test proves the stale owner is fenced.

## Out of scope

Leader election, moving a live loop between replicas on purpose, changing the lease TTL defaults, any wire-protocol change.

## Reconcile log

### 2026-11-03

Unchanged against main 5eb0925: the registry contract, both backends and the inproc resume/renewer paths still match the spec pointers; no overlapping in-flight change (0120 stays out of scope).
