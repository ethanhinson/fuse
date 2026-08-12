---
id: 38
slug: committed-event-observability-projections
title: Committed-event observability projections consume the exact store-assigned envelope
status: Accepted
date: 2026-08-12
supersedes: []
reverses: []
relates_to: [25, 31]
change: 51
---

## Context

The durable event-store backends allocate committed-event identity internally: the store assigns
the per-loop sequence number and commit timestamp as it durably accepts an event. Routine
observability projections need that exact committed identity, together with the tenant and loop,
to label metrics, correlate traces, and emit structured logs. Projecting the caller's pre-append
copy would expose unset or stale `Seq` and `TS` values, while separately replaying or subscribing
to reconstruct the committed event would introduce live/replay ordering and duplication races.

The common `event.DurableStore` contract must remain compatible for existing implementations and
callers. At the same time, observability-enabled composition must not silently accept a backend
that cannot provide the committed envelope it requires.

## Decision

Committed-event observability projections consume the exact store-assigned envelope through the
optional, repository-owned `event.CommittedDurableStore` capability and its
`AppendCommitted` operation. `fsstore` and `pgstore` implement this capability so projection
occurs from the event returned after durable commit, never from the caller's pre-append copy.

`event.DurableStore` remains the ordinary compatible contract. Composition that enables
observability requires and checks for `CommittedDurableStore` at startup, rejecting an enabled
backend that lacks the capability rather than falling back to replay/live reconstruction.

## Consequences

- Observability receives the authoritative tenant, loop, sequence, and timestamp identity that
  the durable backend committed, making routine telemetry correlate with replayed history.
- The event package gains one additive, repository-owned optional capability; existing
  `DurableStore` implementations remain source-compatible until selected for observability.
- `fsstore` and `pgstore` must preserve their store-assigned envelope through `AppendCommitted`.
- Enabling observability against an unsupported durable backend fails at composition startup,
  trading early explicit configuration failure for correctness and avoiding replay/live races.
