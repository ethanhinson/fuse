---
id: 31
slug: durable-distributed-event-store-loop-registry
title: Durable, backend-agnostic event store + durable loop registry — existence and history survive restart and are reachable from any instance
status: Accepted
date: 2026-08-10
supersedes: []
reverses: []
relates_to: [24, 25, 27, 30]
change: 47
---

## Context

ADR-0030 (change #46) de-globalized the event store and put each loop's history
AND its very existence on the Runtime instance as per-loop in-memory state: the
`r.loops` map on `inProcRuntime`. That made N-loop hosting real, but existence
and history lived only in one process's memory. Two gaps remained, both on the
"make the seam hostable" arc (#47 in the #46 → #47 → #48 sequence):

1. **Durability.** A process restart lost every loop's history and the very fact
   that the loop existed. `fsstore` persisted a loop's `events.jsonl`, but the
   registry of which loops exist / who owns a live one was in-memory only.
2. **Reachability across instances.** A loop started by one process could not be
   resolved, replayed, or live-tailed from another — the concrete symptom was a
   cross-process `runtime: loop not found`. Any deployable, distributed
   loop-server needs a loop's existence and history to be a shared, durable fact,
   not per-process memory.

This had to be solved WITHOUT regressing the seam's hard-won invariants:
ADR-0030's value-threading / policy-free seam, ADR-0024's plaintext-independent
event store, and ADR-0025's store-owned Seq + never-blocking `Append`. It also
had to leave room for full observability (change #51) without importing any of it
now. Single-tenant is the practical reality, but a distributed deployable store
needs a tenancy boundary present from day one rather than retrofitted.

## Decision

A backend-agnostic **durable EventStore + loop-registry seam** in `internal/event`,
proven by two implementations, with tenancy first-class and the in-memory loop
map demoted to a cache over the durable registry.

**(a) The seam and the two-implementation discipline.** `internal/event` gains a
`DurableStore` (context- and tenant-scoped `Append`/`Subscribe`/`Replay`, keyed
by `StreamKey{Tenant, Loop}`) and a `LoopRegistry`
(`Register`/`SetLive`/`Resolve`/`List` for existence + liveness/ownership), plus
`TenantID`/`LoopID`/`DefaultTenant`/`NormalizeTenant` and `ErrLoopUnknown`. The
seam is proven by TWO backends behind ONE shared behavioral conformance suite
that runs against both:

- **fsstore** (retained local/dev), extended with subdirectory-per-tenant
  partitioning `<baseDir>/<tenant>/<loop>/events.jsonl` and a
  filesystem-backed registry.
- A build-tagged (`//go:build pgstore`) **Postgres** backend (deployable), using
  `LISTEN`/`NOTIFY` as the shared cross-instance pub/sub for live tail.

The conformance suite is the contract: neither backend may silently diverge, and
neither may silently regress ADR-0024 or ADR-0025 (see *Honored invariants*).

**(b) The tenant model.** `tenant_id` is a first-class parameter of every
store/registry entry point, embedded in every key and every WHERE clause, and
enforced in fuse's data layer — **app-enforced**, so it is portable across
backends rather than leaning on a backend-specific isolation feature (e.g.
Postgres RLS). Single-tenant in practice today; the boundary is present from day
one so multi-tenant is not a later retrofit through every call site.

**(c) Registry durability — the in-memory map becomes a cache.** The loop
registry is now durable-store-backed: `(tenant_id, loop_id)` resolves from the
durable store, so a cold process can resolve + replay + live-tail a loop it never
started — closing the cross-process `runtime: loop not found` gap. The
`r.loops` map on `inProcRuntime` is **DEMOTED from source of truth to a
cache/projection** over the durable registry. The durable registry is the source
of truth for existence and for liveness/ownership; a *live* loop is still owned
and driven by exactly one instance's in-memory `*loop` (durability makes
existence shared, not execution).

**Honored invariants (must not silently regress).** This ADR explicitly HONORS,
and does not undo, three prior decisions — stated here so the Postgres backend
cannot quietly drift:

- **ADR-0024** — events are born PLAINTEXT and the event store stays independent
  of the segment store; the Postgres backend persists the same plaintext event
  record, not a compressed or segment-derived one.
- **ADR-0025** — the store is the SOLE per-loop Seq allocator and `Append` NEVER
  blocks the loop. Postgres allocates per-loop `Seq` via a **transaction-scoped
  per-loop advisory lock** (per-loop total order, explicitly NOT a global
  sequence), and `Append` does a synchronous durable INSERT then hands `NOTIFY`
  to a decoupled bounded async publisher — so `Append` never blocks (preserving
  ADR-0025/ADR-0016).

**Observability hooks, no observability (change #51 boundary).** A bare
`context.Context` threads every durable op and the pub/sub hop, and the
`(tenant_id, loop_id, node_id)` triple is exposed consumer-readable — with NO
OTEL import, exporter, or metrics surface. The seam is instrumentable later
without a re-cut; the instrumentation itself is change #51.

## Consequences

- (+) A loop's existence AND history survive a process restart and are reachable
  from any instance — a cold process resolves, replays, and live-tails a loop it
  never started, fixing the cross-process `runtime: loop not found` gap.
- (+) The store is genuinely backend-agnostic: fsstore for local/dev, Postgres
  for deployment, one conformance suite proving parity — a new backend is a new
  implementation behind the same seam, not a rewrite.
- (+) Cross-instance live tail works via Postgres `LISTEN`/`NOTIFY` shared
  pub/sub, with `Append` still synchronous-durable-then-async-notify so it never
  blocks the loop.
- (+) Tenancy is present from day one and portable across backends (app-enforced,
  in every key and WHERE clause), so multi-tenant is not a retrofit.
- (+) ADR-0030's decisions are BUILT ON, not undone: the store/tree still flow as
  values, `BuildAgent` still inverts the dependency, and `internal/runtime` still
  imports no `cmd/fuse`. Only the source of truth for existence/liveness moved
  from the in-memory map to the durable registry (the map is now a cache).
- (+) Observability (#51) can hook the threaded context and the exposed
  `(tenant_id, loop_id, node_id)` triple with no seam re-cut and no OTEL
  dependency incurred now.
- (−) A durable store is now on the hot path of every `Append` — a synchronous
  durable write (JSONL flush or Postgres INSERT) per event, the deliberate cost
  of durability. The async publisher keeps this off the loop's critical path but
  the durable write itself is synchronous by design.
- (−) Two backends plus a conformance suite are more surface to maintain than one
  in-memory map; the build-tagged Postgres path needs a database to exercise its
  tag.
- (−) The in-memory-map-as-cache demotion adds a coherence concern (cache vs
  durable source of truth) that the single-source-of-truth in-memory design of
  ADR-0030 did not have; liveness/ownership must be read through the durable
  registry, not the local cache, when correctness depends on it.
- (−) App-enforced tenancy means every new store/registry call site must carry
  and honor `tenant_id`; the discipline is on the code, not on a backend feature.
