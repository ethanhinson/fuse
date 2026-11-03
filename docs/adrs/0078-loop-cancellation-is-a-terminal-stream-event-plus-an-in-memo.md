---
id: 78
slug: 'loop-cancellation-is-a-terminal-stream-event-plus-an-in-memo'
title: 'Loop cancellation is a terminal stream event plus an in-memory seal'
status: 'Accepted'
date: '2026-11-03'
supersedes: []
reverses: []
relates_to: [31, 47]
change: 117
---

## Context

The networked loop server gained CancelLoop. Callers need a cancelled loop never to run again, across replicas, without adding new registry state.

## Decision

A cancelled loop is marked by a terminal `loop.cancelled` event (the highest seq of its stream) and, in the owning process, an in-memory seal. The registry only flips `live`.

Resume refuses by replaying the stream: it refuses on a `loop.cancelled`, or on a `loop.cancel.request` with no `loop.cancelled` after it (an unanswered relayed request is as good as a cancel; the refusing replica seals it).

Cross-replica cancel relays through the durable stream like `steer.request`; the owner's relay pump also honours a foreign `loop.cancelled`. In a narrow replica race two `loop.cancelled` events may exist; readers take the first.

## Consequences

- The stream stays the single source of truth (the durable event store decision, change 0054 D1/D5).
- No schema migration; both registry backends and all fakes are untouched.
- A replica that still caches a finished loop sealed elsewhere answers Send with "loop finished" rather than "loop cancelled" (it never relaunches).

## Alternatives considered

- A `cancelled` column / `MarkCancelled` on LoopRegistry: needs a migration, every fake grows a method, and it duplicates the stream's fact.
- Terminal events for idle reaps and drains: rejected, those loops are resumable by design.
- Direct replica-to-replica RPC: replicas do not know each other's addresses, and the relay pattern already exists.
