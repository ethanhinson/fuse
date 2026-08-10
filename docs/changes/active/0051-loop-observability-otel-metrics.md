---
id: 51
slug: loop-observability-otel-metrics
title: Observability for the loop — OTEL traces + Prometheus metrics + Grafana + Loki, as a projection over the event stream
status: proposed
priority: medium
type: feat
created: 2026-08-10
updated: 2026-08-10
depends_on: [46]
related: [43, 46, 47, 48]
discovered_from: [46]
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

A hosted, multi-tenant agent-loop service is unobservable-by-feel: once N loops run per process
across instances, you cannot reason about latency, cost, error rates, or fan-out without telemetry.
The user is prioritizing observability — the full three-pillar stack: **OTEL traces (spans per
turn/tool/spawn), Prometheus metrics, Grafana dashboards, and structured logs → Loki**.

The load-bearing architectural insight: fuse's **event stream (change 43)** is already a structured,
per-loop, typed telemetry feed (turn.start / model.call.start-end / tool.call-result /
spawn.start-done / error, with per-loop Seq + node IDs). So observability is a **consumer /
projection over the existing seam**, not instrumentation smeared through the code — the same
policy-free discipline that kept the Runtime seam clean. Change 47 designs in the hooks
(trace-context propagation through store + pub/sub; tenant_id / loop_id / node_id labeling) so this
change is a projection, not a retrofit.

## What changes

To be designed during grooming. At a sketch:
- An observability consumer that subscribes to the event stream and emits an **OTEL span tree**
  mirroring the loop (turn → model.call → tool.call → spawn as child spans; trace context across
  spawn and across instances via the 47 hooks).
- A **Prometheus `/metrics`** endpoint: loops active, turns, tokens in/out, tool calls, model
  latency, errors, spawn depth/fan-out, queue depth — labeled by tenant / model.
- **Grafana dashboards** (JSON) + a compose stack (Prometheus + Grafana + Loki + OTEL collector).
- **Loki** shipping of the event stream as structured logs for correlation with traces/metrics.
- **Hybrid sourcing:** event-stream projection for the loop-shaped spans, plus a little inline
  instrumentation for internal timings the stream doesn't capture (e.g. 47 durable-store / pub-sub
  latency).

## Out of scope

- The telemetry HOOKS themselves (trace-context threading, tenant/loop/node labeling) — those land
  in change 47's store + pub/sub seam so this change can consume them.
- Any change to the `Runtime` seam — observability is a consumer/binding, not a seam change.

## Open questions

- Where the observability projection runs (in-process per loop-server vs a sidecar consumer of the
  durable stream) and how it scales with multi-instance hosting.
- OTEL exporter/collector wiring and the Go OTEL SDK dependency boundary (kept out of the leaf
  `internal/event` package — introduced only at the consumer/binding layer).
- Metric cardinality control for tenant/model/loop labels at scale.
- Whether Loki log shipping is same-change or a fast-follow after traces + metrics land.
- Sequencing: build after the networked binding (48) so there is a real hosted service to observe.
