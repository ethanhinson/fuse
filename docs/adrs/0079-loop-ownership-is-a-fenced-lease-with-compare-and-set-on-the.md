---
id: 79
slug: 'loop-ownership-is-a-fenced-lease-with-compare-and-set-on-the'
title: 'Loop ownership is a fenced lease with compare-and-set on the loop registry'
status: 'Accepted'
date: '2026-11-03'
supersedes: []
reverses: []
relates_to: []
change: 126
---

## Context

Before this change, Heartbeat/SetLive/Register overwrote owner_node_id unconditionally, so a replica paused past its lease could re-take a loop another replica had resumed, giving two writers on one stream; the idle reaper only counted owner-local activity, so loops watched by callers through other replicas could be reaped.

## Decision

The LoopRegistry contract fences ownership: Heartbeat, SetLive and Register from a node that is not the recorded owner of a live record with an unexpired lease return event.ErrNotOwner; Claim atomically takes an expired or not-live record (pgstore: one UPDATE ... WHERE (owner=$node OR NOT live OR lease_expiry IS NULL OR lease_expiry < $now) RETURNING, with the writer's clock as $now; fsstore: under the store mutex). Each runtime mints its own node id (Deps.NodeID) rather than using the loop id. A refused heartbeat ends the loop locally: the renewer cancels the session, seals the sink, drops the loop from the cache, and appends one error{reason:"ownership_lost"} only while the node still owns the record and the sink was not already sealed by a cancel. Activity is registry-visible: non-owner Observe/Send/Steer call Registry.Touch, which advances LastActivity (new nullable last_activity column / sidecar field) without touching owner or lease; the owner's idle reaper re-arms while LastActivity+IdleTTL is in the future.

## Consequences

Clock skew between replicas bounds fence precision (lease comparisons use the writer's wall clock); a stale owner may still append events between its last successful heartbeat and the refused one (bounded by the renew interval); every non-owner interaction costs one registry write.

## Alternatives considered

Leader election / an external lock service (rejected: heavyweight, out of scope); monotonic fencing tokens on every event append (rejected for now: wire/storage change, larger blast radius); keep owner-local idle accounting (rejected: reaps loops in active use behind a load balancer).
