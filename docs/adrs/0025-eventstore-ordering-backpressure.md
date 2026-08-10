---
id: 25
slug: eventstore-ordering-backpressure
title: EventStore ordering and back-pressure — store-allocated Seq, non-blocking drop-newest-with-gap subscriber delivery
status: Accepted
date: 2026-08-09
supersedes: []
reverses: []
relates_to: [16, 19, 24]
change: 43
---

## Context

Change #43's EventStore (`internal/event` interface, `internal/event/fsstore` JSONL impl) must give a single total order for Replay AND must never block the loop, which calls `Append` inline at every state transition. Two coupled decisions were non-obvious:

1. Seq allocation across the spawn tree: per-session-global (one monotonic counter, single total order) vs per-node (consumer merges streams).
2. Subscriber back-pressure when a consumer's channel is full: drop-oldest, drop-newest, or unbounded buffer. This must never block the loop or wedge an agent's scheduler slot (ADR-0016's slot-yield/no-deadlock invariant: a subscriber that back-pressured Append could stall a slot-holding goroutine).

## Decision

Seq is per-session-global, allocated INSIDE the store under its lock during Append (`s.seq++; e.Seq = s.seq`). Callers pass Seq=0; the store is the sole allocator. Because the holder is process-global and one-process-one-session (ADR-0019), the per-session store instance is the natural monotonic allocator — this yields a single total order and a clean `Replay(from Seq)` cursor with no cross-node merge.

Subscriber delivery is NON-BLOCKING with a bounded per-subscriber buffer and drop-newest-with-a-gap-marker: each `Subscribe()` gets its own buffered channel (cap 256); `Append` fans out with a non-blocking `select { case ch <- e: default: dropped++ }`. A full buffer drops the newest event for THAT subscriber and bumps a gap counter (visible via `Dropped()`), never back-pressuring `Append`, the loop, or any other subscriber. The durable JSONL write is independent of subscribers, so a dropped live event is still fully recoverable via `Replay`.

## Consequences

- (+) A single total Seq order gives a deterministic Replay cursor and de-dup without a cross-node merge step.
- (+) ADR-0016 is preserved: no subscriber can ever wedge an agent's scheduler slot, because Append never blocks on delivery. Verified by a non-blocking-send regression test under -race (fill a subscriber's buffer, burst many Appends, assert prompt return + a recorded gap) and a reader-vs-writer (Replay-during-Append) race test.
- (+) Durability is decoupled from liveness: a slow consumer loses live events (visibly, via the gap marker) but never loses history — Replay reads the full JSONL.
- (−) A slow live subscriber sees a gap rather than every event; it must reconcile via Replay(from lastSeq) if it needs completeness. This is the deliberate trade over blocking the loop.
- (−) The single global counter assumes one process per session (ADR-0019); a future multi-session-per-process or networked store would revisit Seq allocation.
