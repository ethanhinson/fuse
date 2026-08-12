---
id: 51
slug: loop-observability-otel-metrics
title: Observability for the loop — OTEL traces + Prometheus metrics + Grafana + structured logs
status: in-progress
priority: medium
type: feat
created: 2026-08-10
updated: 2026-08-11
depends_on: [46]
related: [43, 46, 47, 48]
discovered_from: [46]
adrs: []
spec: docs/superpowers/specs/0051-loop-observability-otel-metrics.md
plan: docs/superpowers/plans/2026-08-12-loop-observability-otel-metrics.md
results:
trivial: false
auto_groomable:
branch: feat/loop-observability-otel-metrics
pr:
blocked_by:
reconciled: true
claimed_at: 2026-08-12T03:39:09Z
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [0051-loop-observability-otel-metrics.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/0051-loop-observability-otel-metrics.md) |
| Plan | [2026-08-12-loop-observability-otel-metrics.md](https://github.com/ethanhinson/fuse/blob/feat/loop-observability-otel-metrics/docs/superpowers/plans/2026-08-12-loop-observability-otel-metrics.md) |
<!-- docket:artifacts:end -->

## Why

A hosted, multi-tenant agent-loop service is unobservable-by-feel: once N loops run per process
across instances, you cannot reason about latency, cost, error rates, or fan-out without telemetry.
The user is prioritizing observability across metrics, deep traces, and structured logs while
keeping production monitoring infrastructure outside Fuse's operational responsibility.

The load-bearing architectural insight: fuse's **event stream (change 43)** is already a structured,
per-loop, typed telemetry feed (turn.start / model.call.start-end / tool.call-result /
spawn.start-done / error, with per-loop Seq + node IDs). So observability is a **consumer /
projection over the existing seam**, not instrumentation smeared through the code — the same
policy-free discipline that kept the Runtime seam clean. Change 47 designs in the hooks
(trace-context propagation through store + pub/sub; tenant_id / loop_id / node_id labeling) so this
change is a projection, not a retrofit.

## What changes

- An extensible in-process observability projection over committed loop events, with narrow
  provider-neutral hooks for timings the stream cannot represent.
- Prometheus `/metrics` with curated tenant/model/tool dimensions, deterministic cardinality
  budgets, observable overflow, and per-tenant reliability, quota, and misuse alert examples.
- OTEL nested spans and W3C trace-context propagation from SDK callers through API, loop, model,
  tool, spawn, store, and pub/sub boundaries.
- Structured JSON logs to stdout or an optional concurrency-safe file sink, including atomic
  reopen for host/container-owned rotation and authenticated live debug-level controls.
- Source-controlled Grafana dashboards and a local evaluation Compose stack containing
  Prometheus, Grafana, OTEL Collector, and Tempo.
- Complete documentation demonstrating Grafana metric → Tempo trace → correlated logs →
  authenticated full-turn replay.

## Out of scope

- The telemetry HOOKS themselves (trace-context threading, tenant/loop/node labeling) — those land
  in change 47's store + pub/sub seam so this change can consume them.
- Any change to the `Runtime` seam — observability is a consumer/binding, not a seam change.
- Production hosting or lifecycle management for Prometheus, Grafana, Tempo, Loki, or Collector.
- Direct Loki shipping or a bundled Loki service; external collectors may consume stdout/files.
- Jaeger-specific integration, which is a separate follow-up.
- Routine telemetry capture of prompts, responses, or tool payloads; full turns remain in the
  authenticated replay API/SDK.

## Open questions

None. The linked spec records the settled boundaries, defaults, trade-offs, and assumptions.

## Reconcile log

### 2026-08-12 — reconcile against origin/main (164fbaf)

Verified the change and linked spec against the current integration branch after changes 43, 46,
47, 48, 49, 50, 52, 53, 56, and 59 landed. The design remains buildable and is neither obsolete nor
fundamentally invalidated. Current code provides the durable, context-carrying
`event.DurableStore` and `LoopRegistry` seams, consumer-readable `StreamKey{Tenant, Loop}` plus
`Event.NodeID`, the policy-free multi-loop runtime, edge-authenticated tenant/owner identity, the
Connect binding, and Go/TypeScript SDK surfaces the observability projection needs.

The concrete implementation anchors are now `internal/event` (including fsstore/pgstore),
`internal/runtime`, `internal/connectloop`, `cmd/fuse/loop_serve_net.go`, and `sdk/{go,ts}`. The
event envelope does not yet persist W3C trace carrier fields and committed event delivery does not
carry operation timing by itself; those remain intentional work in this change, using
repository-owned carrier/observer types so core runtime and event packages stay vendor-neutral.
The existing event payloads contain sensitive full-turn data, reinforcing the settled rule that
routine logs and metrics must project metadata only rather than serializing `Event.Payload`.

No adjacent follow-up was auto-captured: auto-capture is disabled, and no independently valuable
work outside the settled scope was discovered during reconcile.
