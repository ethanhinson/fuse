---
id: 47
slug: durable-distributed-event-store
title: Durable / distributed event store — survives restart and is shared across instances
status: in-progress
priority: medium
type: feat
created: 2026-08-10
updated: 2026-08-10
depends_on: [46]
related: [43, 45, 46, 48, 49, 51]
discovered_from: [45]
adrs: []
spec: docs/superpowers/specs/2026-08-10-durable-distributed-event-store-design.md
plan:
results:
trivial: false
auto_groomable:
branch: feat/durable-distributed-event-store
claimed_at: 2026-08-10T21:43:26Z
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-10-durable-distributed-event-store-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-10-durable-distributed-event-store-design.md) |
<!-- docket:artifacts:end -->

## Why

Change 46 made the event store per-loop instance state, so one process hosts N concurrent loops —
but persistence stayed **process-local**: a loop's history is a per-session `events.jsonl` on the
starting process's disk, and the live registry of which loops exist is purely the in-memory
`r.loops` map. Live verification proved the gap this leaves: a **fresh** `fuse loop-server` process
returns `runtime: loop not found` when a client attaches to a loop_id a **prior** process started —
even though that loop's `events.jsonl` is still on disk — because every lookup resolves loop_id only
against the in-memory map, which a cold process boots empty. So the flagship hosting scenario,
"attach to a running or finished loop from your phone after the server redeployed," does not work.

This is the **durable-persistence milestone (hosting milestone 3)** of the "make the seam hostable"
arc: a loop's history *and its very existence* must survive a process restart and be reachable from
any instance. It is the storage-level foundation the networked binding (48), auth/multi-tenancy
(49), and observability (51) build on.

## What changes

A backend-agnostic **durable EventStore + loop-registry seam** — `Append` / `Replay(from)` /
`Subscribe` plus loop_id resolution independent of any process's memory — with **two
implementations**: the existing `fsstore` retained for local/dev, and **Postgres** as the deployable
backend (the two-implementations discipline that keeps the seam honest). `tenant_id` is a
**first-class parameter** of every entry point, embedded in every key and query, enforced in fuse's
data layer and portable across both backends (single-tenant in practice, boundary present from day
one). The **loop registry becomes durable-store-backed** so a cold process resolves
`(tenant_id, loop_id)` → durable stream + liveness/ownership without the loop being in memory,
reconciling ADR-0030. A **shared pub/sub layer** (Postgres `LISTEN`/`NOTIFY`) delivers live
cross-instance tail, which combined with durable `Replay` gives both halves of cross-instance
reattach. Observability **hooks** are designed in now (trace-context threaded through every op and
the pub/sub hop; `tenant_id`+`loop_id`+`node_id` labeling exposed) with **no emission** — full
observability is change 51. Existing invariants preserved: events born plaintext (ADR-0024), store
is sole Seq allocator (ADR-0025), `Append` never blocks the loop (ADR-0025/ADR-0016). Full design in
the linked spec.

## Out of scope

- **No network transport / new binding** — that is change 48. 0047 changes what the store and
  registry are made of, not how they are exposed over the wire.
- **No auth / tenant identity** — change 49. 0047 provides the storage-level `tenant_id` boundary;
  who a tenant is and how identity is proven is 49.
- **No observability emission** — OTEL spans, a Prometheus `/metrics` endpoint, Grafana, Loki are
  change 51. 0047 only threads the hooks (D5).
- **No client SDK** — change 50.
- **No `Event` envelope / Kind change** and **no Seq-allocation-model change** — the wire format and
  single-total-order-per-loop stay as ADR-0024/ADR-0025 define them.

## Open questions

Detailed open questions live in the spec's *Open questions (resolve at build reconcile)*. The
load-bearing ones for planning:

- How the Postgres implementation is tested without a standing DB (dockerized/ephemeral Postgres,
  testcontainers, or a build-tagged integration test) so `go test ./...` stays green on a bare
  checkout.
- Reconcile with ADR-0030: supersede vs. amend, and how liveness/ownership is represented in the
  durable registry vs. the in-memory `r.loops` map (leading direction: the map becomes a cache over
  the durable registry).
- How the Postgres durable write + `LISTEN`/`NOTIFY` publish preserve the non-blocking-`Append`
  guarantee (ADR-0025/ADR-0016).
