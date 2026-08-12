---
id: 40
slug: provider-neutral-composite-observer
title: Provider-neutral composite Observer owns production observability
status: Accepted
date: 2026-08-12
supersedes: []
reverses: []
relates_to: [39]
change: 51
---

## Context

Change #51 adds production observability across tracing, metrics, and structured logs for a
multi-loop agent runtime. Those signals span root and child agents, the spawn tree, durable store
and pub/sub boundaries, and the Connect binding. Letting each package choose and construct a
telemetry provider would couple core runtime behavior to vendor SDKs, fragment operation timing,
and make it easy for an event projection and direct instrumentation to count the same work twice.

The event stream remains the durable record and can support asynchronous projections under
ADR-0039, but it cannot by itself be the timing authority for every operation. The runtime also
needs a safe default that has no operational dependency on an observability backend.

## Decision

Fuse will expose one repository-owned, provider-neutral composite `observe.Observer` contract that
combines tracing and metrics. Core event, runtime, and agent packages depend only on that contract
and its no-op implementation; they do not import or construct OpenTelemetry or Prometheus types.

Concrete OpenTelemetry and Prometheus adapters are created only at composition boundaries. The
same observer instance is threaded from the root runtime through child agents and spawners, store
and pub/sub hooks, and Connect operations. These paths must use that instance rather than creating
provider-specific observers or parallel instrumentation.

The observer is the authoritative path for operation timing. Event-stream projections may enrich
observability from committed events, but must not emit an overlapping operation timing or counter
that double-counts work already observed through the composite observer.

## Consequences

- All production telemetry can share trace context, labels, metrics, and timing semantics while
  the core runtime remains runnable with no-op observability.
- Provider selection, exporter setup, and adapter-specific lifecycle stay at executable and
  transport composition boundaries, making those integrations replaceable and testable.
- Propagation becomes an explicit dependency across root/child-agent, spawning, persistence,
  pub/sub, and Connect code paths; missing propagation is a correctness gap rather than an
  optional enhancement.
- Implementations must define event projections against the observer's authoritative signals so
  dashboards do not overstate work through duplicate observations.
