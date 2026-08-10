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
claimed_at: 2026-08-10T21:45:26Z
pr:
blocked_by:
reconciled: true
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

## Reconcile log

### 2026-08-10 — build reconcile (implementer)

Re-validated the spec against the **real current tree** (0046 has since merged; the three packages
the spec warned "were not yet in the local working tree" now exist). Anchored to seam SHAPES per
ADR-0024/0025/0030, not line numbers. Findings:

- **Seam shapes confirmed present and matching the spec.** `internal/event/store.go` defines
  `EventStore { Append(Event) error; Subscribe() (<-chan Event, func()); Replay(from Seq) ([]Event, error) }`
  plus `NoopStore` — exactly the spec's D1 anchor. `internal/event/fsstore.FSEventStore` writes
  `<baseDir>/<sessionID>/events.jsonl`, allocates `Seq` under its mutex (`s.seq++; e.Seq = s.seq`),
  and fans out non-blocking drop-newest-with-gap — ADR-0024/0025 intact. `internal/runtime`
  (`inProcRuntime`) holds `loops map[string]*loop`; `Observe`/`Attach`/`Send`/`Spawn` all resolve
  loop_id **only** via `lookup()` against that in-memory map (`ErrLoopNotFound`) — the exact cold-process
  gap 0047 fixes. `internal/loopserver.serveObserve` **already implements subscribe-before-replay +
  dedup-at-watermark over the JSON-RPC wire** (the `replay-live-handoff-dedup-at-watermark` discipline)
  for the in-proc path — a real asset: 0047 extends this same discipline to the Postgres pub/sub tail
  rather than inventing it.
- **Signatures that 0047 changes (real, current):** `Runtime.Observe(loopID string)` and
  `Attach(loopID string, from Seq)` carry **neither** `context.Context` **nor** `tenant_id` today;
  `LoopConfig` has no `tenant_id`; `loopserver` params (`observeParams`, `startParams`) have no
  tenant field. D2 (tenant-first-class) and D5 (context threading) are the shape changes. `cmd/fuse`'s
  `buildLoopServerRuntimeDeps` wires `BaseDir: session.DefaultLogDir()` and is the **only** multi-loop
  binding — the composition root where a Postgres backend gets selected.
- **Environment:** Go 1.26.5, Docker daemon running locally, **no** Postgres/pgx/testcontainers deps
  in `go.mod` yet. Confirms OQ1's constraint is live: a bare `go test ./...` must stay green with no PG.

**Scope unchanged** — one change / one PR, all of D1–D5. **No obsolescence, no fundamental
invalidation.** The six spec open questions are resolved below (also in the spec's reconcile log) and
carried into the plan.

**Six open questions resolved:**

1. **Testing Postgres without a standing DB** → **build-tagged integration suite** (`//go:build pgstore`)
   using `testcontainers-go` for an ephemeral Postgres. A bare `go test ./...` (no tag) never compiles
   the pg files, so it stays green with **no Postgres and no Docker** — the non-negotiable bare-checkout
   constraint. The one shared behavioral suite runs both backends: fsstore always; Postgres only under
   `-tags pgstore` (and skips cleanly if the container cannot start).
2. **Postgres schema/keying** → `events(tenant_id, loop_id, seq, ts, kind, node_id, parent_id, depth,
   turn, payload jsonb)`, PK `(tenant_id, loop_id, seq)`; per-loop Seq allocated under a
   **transaction-scoped per-loop advisory lock** (`pg_advisory_xact_lock(hashtext(tenant||loop))`) via
   `seq = COALESCE(MAX(seq),0)+1` scoped to `(tenant_id, loop_id)` — a **per-loop** total order, not a
   global sequence (honors ADR-0025). `Replay(from)` = `WHERE tenant_id=$1 AND loop_id=$2 AND seq>$3
   ORDER BY seq`. Registry table `loops(tenant_id, loop_id, owner_node_id, live bool, created_at,
   updated_at)` PK `(tenant_id, loop_id)`. Retention: none in 0047 (append-only, documented follow-up),
   matching fsstore.
3. **fsstore tenant partitioning** → **subdirectory-per-tenant**:
   `<baseDir>/<tenant_id>/<loop_id>/events.jsonl`; resolution/replay never cross the tenant prefix.
   Empty/default `tenant_id` maps to a reserved default segment so single-tenant local behavior is
   preserved.
4. **Reconcile ADR-0030 (supersede vs amend) + liveness** → the in-memory `r.loops` map becomes a
   **cache/projection over the durable registry** (durable registry is source of truth for existence +
   liveness/ownership; the live `*loop` is still owned/driven by one instance). **ADR decision recorded
   at Step 6 via docket-adr** — leading direction **amend ADR-0030 with a dated `## Update`** (the
   value-threading + policy-free-seam decisions ADR-0030 made all still hold; 0047 only makes existence
   durable), escalating to a superseding ADR only if the registry seam materially reshapes the Runtime
   value-threading. ADR-0024/0025 **preserved, not superseded**.
5. **Non-blocking Append under Postgres** → Append does the **synchronous durable INSERT** (durability
   first), then hands the `NOTIFY` to a **decoupled bounded async publisher** (one goroutine, bounded
   queue, drop-with-gap on overflow) so Append never blocks on NOTIFY delivery or on a slow subscriber —
   preserving ADR-0025/ADR-0016's never-wedge-a-slot guarantee. Subscriber fan-out stays drop-newest-
   with-gap.
6. **Observability hook shape (D5)** → add a **bare `context.Context`** as the first parameter to the
   durable-store ops and the pub/sub hop (**no OTEL import/exporter**), so 0051 can attach an exporter
   without an interface change; expose the `(tenant_id, loop_id, node_id)` labeling triple in
   consumer-readable form on telemetry-relevant ops. `node_id` stays the per-event envelope value
   (already present) plus an owning-instance `node_id` at store construction. No `/metrics` surface.
