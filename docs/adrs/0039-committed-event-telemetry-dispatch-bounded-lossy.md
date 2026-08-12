---
id: 39
slug: committed-event-telemetry-dispatch-bounded-lossy
title: Committed-event telemetry dispatch is bounded, non-blocking, and lossy under saturation
status: Accepted
date: 2026-08-12
supersedes: []
reverses: []
relates_to: [25, 38]
change: 51
---

## Context

Change #51 projects observability from the exact store-assigned envelope that a durable event
store has committed (ADR-0038). Those projections may emit metrics, traces, and structured logs,
whose collectors and exporters can be slow or temporarily unavailable. Running projection work
inline in append would put that latency and failure mode on the loop's critical path.

The loop must remain able to append durable events and make forward progress even when routine
telemetry cannot keep up. An unbounded buffer merely postpones that failure as memory growth, while
a synchronous or back-pressuring projection can stall loop execution. Durable committed events
remain available for replay, so routine live telemetry need not be lossless to preserve the
authoritative record.

## Decision

Committed-event telemetry is handed off from the append path to a finite, non-blocking in-process
dispatcher queue served by a bounded worker set. The append path only attempts enqueue; it never
waits for projection work, exporter I/O, or dispatcher capacity.

When the queue is saturated, the dispatcher drops the incoming observation and increments bounded
drop/error metrics. Projection failures are likewise recorded through bounded metrics and are not
returned to, or allowed to interrupt, durable append or loop execution. The durable event store is
the source of truth; replay is the recovery path for consumers that require complete history.

Dispatcher shutdown stops new work and completes deterministically within its caller-provided
context bound. It may abandon outstanding routine observations rather than indefinitely delay
process or loop shutdown.

## Consequences

- A slow or failed telemetry destination cannot consume unbounded memory, goroutines, append
  latency, or loop availability.
- Routine telemetry can contain observable gaps under load; operators must interpret the
  dispatcher drop/error metrics alongside projections rather than treating them as a lossless
  audit trail.
- Durable event replay remains the complete, authoritative history and can reconstruct a missed
  observation when that completeness is needed.
- Implementations must keep queue capacity, worker count, metric labels, and shutdown waiting
  bounded, and must test saturation and context-expired shutdown without regressing append
  availability.
