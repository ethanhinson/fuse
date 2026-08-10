<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0047 — Durable / distributed event store — survives restart and is shared across instances](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0047-durable-distributed-event-store.md)**
<!-- docket:backlink:end -->

# Spec 0047 — Durable, distributed event store: cross-process / cross-instance loop reattach

## Problem

Change 0046 (ADR-0030, merged commit 22480d5) de-globalized the event store so one
`fuse loop-server` process now hosts N concurrent loops, each keyed by `tree.RootID()`
(the loop_id) in an in-memory `loops map[string]*loop` on `internal/runtime.inProcRuntime`,
each with its own `fsstore` event store and a provably isolated Seq stream. That unlocked
concurrent hosting — but it left persistence **process-local**. A loop's durable history is a
per-session `events.jsonl` file on the starting process's local disk (`internal/event/fsstore`,
ADR-0024/ADR-0025), and the live registry of *which loops exist* is purely the in-memory
`r.loops` map (ADR-0030).

**The load-bearing bug (live-verified):** a **fresh** `fuse loop-server` process returns
`runtime: loop not found` when a client attaches to a loop_id that a **prior** process started —
even though that loop's `events.jsonl` still sits on disk — because `inProcRuntime.Attach` and
every lookup path resolves loop_id **only** against the in-memory `r.loops` map, which a
cold-started process boots empty. Two things are missing simultaneously: (1) the durable stream is
tied to one process's local filesystem, not a shared store any instance can read; and (2) even if
the bytes were shared, nothing outside the starting process's memory can *resolve* a loop_id to
its stream. So the flagship hosting scenario — "attach to a running or finished loop from your
phone after the server redeployed" — does not work. **0047 fixes exactly this.**

fuse is becoming a standalone hostable agent-loop runtime, with the user's apps as thin network
clients over the seam. This change is the **durable-persistence milestone (hosting milestone 3)**
of the "make the seam hostable" arc: it makes a loop's history and its very existence survive a
process restart and be reachable from any instance. It depends on 0046 (merged) and precedes the
networked-binding (0048), auth/multi-tenancy (0049), and client-SDK (0050) arc changes.

## Goals

- Define a **backend-agnostic durable EventStore + loop-registry seam** whose `Append` /
  `Replay(from Seq)` / `Subscribe` and **loop_id resolution** are independent of any single
  process's memory or local disk, so a cold process can find and replay a loop it never started.
- Ship **two implementations of the seam** proving it honest: the existing `fsstore` retained as
  the local/dev backend, and **Postgres** as the deployable backend — the same "prove the seam
  with two implementations" discipline that kept the Runtime seam honest across 0045/0046.
- Make **loop_id resolution durable-store-backed**: `(tenant_id, loop_id) → durable stream +
  liveness/ownership` is resolvable from the store, so `Attach`/lookup no longer depend on the
  loop being present in the in-memory `r.loops` map — eliminating the `runtime: loop not found`
  gap for cross-process reattach.
- Make **tenant_id a first-class parameter** of every store and registry entry point, embedded in
  every key and every query/WHERE clause, so the storage-level isolation boundary exists from day
  one and is portable across both backends.
- Deliver **live cross-instance tail**: any instance can live-tail a loop running on another
  instance via a shared pub/sub layer (Postgres `LISTEN`/`NOTIFY`), combined with durable
  `Replay`, so reattach works from any instance with **no gap and no dup**.
- Preserve the existing invariants: events **born plaintext**, independent of the segment store
  (ADR-0024); the store is the **sole Seq allocator** with a single total order and a clean
  `Replay(from Seq)` cursor (ADR-0025); and `Append` **never blocks the loop** on a slow/absent
  subscriber (ADR-0025/ADR-0016).
- **Record a new/superseding ADR** for the durable-store seam, the tenant model, and the
  registry-durability decision, reconciling ADR-0030 (which put the store on the Runtime instance).

## Non-goals (explicit — belong to later arc changes)

This is deliberately **one change / one PR**: the durable-store interface, the Postgres
implementation, the retained `fsstore`, the `tenant_id` isolation boundary, and the pub/sub live
cross-instance tail land **together**. The human explicitly chose a single change over splitting,
because the durable seam, its second implementation, and the tenant keying are one coherent
storage-model decision — proving the seam with two backends and enforcing tenant isolation are not
separable from defining the seam itself without shipping a boundary that later has to be redrawn.

- **No network transport / no new binding.** 0047 changes what the store and registry are made of;
  it does not introduce the WS/HTTP networked binding over the seam. That is change 0048.
- **No auth / tenant identity.** 0047 provides the **storage-level** tenant boundary (`tenant_id`
  threaded and enforced in the data layer); *who a tenant is* and how identity is proven is change
  0049. 0047 may run **single-tenant in practice** (one trust domain, as the loop-server does
  today), but the boundary exists from day one because isolation cannot be safely retrofitted into
  a shared event store later.
- **No client SDK.** Change 0050.
- **No change to the `Event` envelope or Kind set** (`internal/event/event.go`). Payloads,
  discriminants, and the JSONL wire format are unchanged; only where/how the stream is persisted
  and resolved changes.
- **No Seq-allocation-model change.** The store remains the sole Seq allocator with one total order
  per loop stream (ADR-0025); Postgres must honor the same contract, not introduce a new ordering
  scheme.
- **No observability emission.** OTEL span emission, a Prometheus `/metrics` endpoint, Grafana
  dashboards, and Loki log shipping are **out of scope for 0047** and land in change **0051** (built
  after 0048). 0047 only **designs in the hooks** (D5): a `context.Context` / trace-context that
  threads every durable-store op and the pub/sub hop, and the `tenant_id` + `loop_id` + `node_id`
  labeling triple exposed in a consumer-readable form. No OTEL dependency, exporter, or metrics
  surface is added here. Observability is a **consumer over the event stream** (change 0043) plus
  these hooks — a projection, not a loop retrofit — keeping the Runtime seam policy-free.

## Design

The seam under change is `internal/event/store.go`:

```
type EventStore interface {
    Append(Event) error
    Subscribe() (<-chan Event, func())
    Replay(from Seq) ([]Event, error)
}
```

plus the `NoopStore` graceful-degradation default. Today this seam is per-loop instance state on
`inProcRuntime`, opened as an `fsstore.FSEventStore` writing `<baseDir>/<sessionID>/events.jsonl`,
and loop_id resolution lives entirely in the in-memory `r.loops` map. 0047 keeps the
`Append`/`Subscribe`/`Replay` shape, but (a) generalizes what backs it, (b) adds a durable
registry alongside it, and (c) threads `tenant_id` through both.

### 1. The durable-store + loop-registry seam (D1, D2)

**D1 — Pluggable durable store, proven by two implementations.** Define a backend-agnostic durable
store seam whose contract is exactly `Append` / `Replay(from Seq)` / `Subscribe`, plus a
**loop-registry** facet — loop_id resolution independent of any single process's memory. The seam
is satisfied by two implementations: `fsstore` retained as the local/dev backend (extended for
tenant partitioning and durable resolution), and a new **Postgres** implementation (the deployable
one, and the natural home for the D3 pub/sub layer). Proving the seam with two backends is the same
discipline that kept the Runtime seam honest in 0045/0046 (the `two-bindings-one-seam` /
`parity-test-feeds-each-side-its-own-production-source` pattern): both implementations pass **one**
behavioral test suite.

The tension to resolve at build, called out here so it is not lost:

- The durable seam **must not regress ADR-0024** — events are **born plaintext** and the event
  stream is independent of the segment store; a Postgres row's `payload` column is the plaintext
  JSONL body, not a compressed or segment-coupled form.
- The durable seam **must not regress ADR-0025** — the store is the **sole Seq allocator** per loop
  stream (`s.seq++; e.Seq = s.seq` under the store lock, callers pass `Seq == 0`), delivering a
  single total order and a clean `Replay(from Seq)` cursor. The Postgres allocator must produce the
  same per-loop total order (e.g. a per-loop monotonic `seq` under a transaction/advisory-lock
  boundary, **not** a global sequence), and `Replay(from)` must remain a simple `seq > from` cursor
  scoped to `(tenant_id, loop_id)`.
- **Non-blocking `Append` (ADR-0025 / ADR-0016).** `Subscribe` delivery is non-blocking per
  subscriber (full buffer drops the newest event and marks a gap; it never back-pressures
  `Append`), and `Append` **must never wedge an agent's scheduler slot**. A Postgres implementation
  that writes durably **and** publishes to `LISTEN`/`NOTIFY` on the `Append` path must preserve
  this guarantee — how the synchronous durable write plus the notify are kept off the loop's
  critical path (async publish, bounded queue, or a decoupled writer) is an open question flagged
  below.

**D2 — Tenant isolation designed in now, app-enforced.** `tenant_id` is a **first-class parameter**
of the seam: `StartLoop` / `Attach` / `Observe` and every registry lookup carry it, and it is
embedded in **every key and every query/WHERE clause**. Enforcement lives in **fuse's data layer**
(app-enforced, not delegated to a database role model), so it is portable across both backends:

- **Postgres:** every read/write is scoped by `tenant_id` in the `WHERE` clause and the key; a
  lookup for `(tenant_X, loop_id)` can never return a row owned by `tenant_Y`.
- **fsstore:** tenant isolation via **keyspace partitioning** — a tenant-scoped subdirectory
  (e.g. `<baseDir>/<tenant_id>/<loop_id>/events.jsonl`); resolution and replay never cross the
  tenant prefix.

0047 may run single-tenant in practice, but the boundary is present from day one because it
**cannot be safely retrofitted** into a shared event store later. Full auth/tenant identity is
change 0049; 0047 provides the storage-level boundary 0049 builds on.

### 2. Durable loop registry, reconciling ADR-0030 (D4)

**D4 — the loop registry becomes durable-store-backed.** Today loop_id → loop resolution is the
in-memory `r.loops` map on `inProcRuntime` (ADR-0030) — the exact thing a cold process boots empty,
producing `runtime: loop not found`. 0047 makes `(tenant_id, loop_id)` resolvable **from the
durable store**: resolve to the loop's durable stream plus **liveness/ownership** info (is a loop
live, and on which instance), so a process that never started a loop can still find it, replay its
history, and — via D3 — live-tail it.

This must be reconciled with ADR-0030, which deliberately put the store **on the Runtime instance**
as per-loop state. A **new/superseding ADR is required** (supersede vs. amend is an open question
below): the in-memory `r.loops` map most likely becomes a **cache/index over the durable
registry** rather than the source of truth — a live loop is still owned and driven by one
instance's in-memory `*loop`, but its *existence and location* are recorded durably.

**Hazard to design against (learning `deglobalize-holder-also-per-instance-the-shared-graph`,
change 46).** De-globalizing a holder is only half the job: anything wired **once at construction
and shared** is a second hidden global with no `set…`/`current…` accessor to grep for. The durable
registry must **not re-introduce a shared-mutable-state hazard** — e.g. a single shared connection
pool or a cached registry snapshot mutated across instances must not become the new cross-instance
clobber. Audit by key: two concurrent starts, even across instances, must not collide on the same
`(tenant_id, loop_id)`, and each instance's view of liveness must be derived from the durable
registry, not from a construction-time shared object. Registry writes (loop created, loop live on
instance I, loop finished) are the ownership record; the in-memory map is a per-instance projection
of it.

### 3. Live cross-instance tail via shared pub/sub (D3)

**D3 — shared pub/sub for live cross-instance tail.** `Replay` gives durable history from any
instance; it does not give the **live** tail of a loop *currently running on another instance*.
0047 adds a **shared pub/sub layer** so instance B can live-tail a loop running on instance A:
Postgres **`LISTEN`/`NOTIFY`** is the chosen mechanism (it fits D1's Postgres backend and needs no
extra infrastructure). Combined with durable `Replay`, this delivers **both halves of
cross-instance reattach** — durable history replay **plus** live tail — from any instance.

**Design hazard, load-bearing (learning `replay-live-handoff-dedup-at-watermark`, change 45).** An
observer that both subscribes-to-live **and** replays double-delivers any event that lands between
the two steps: reordering only converts the duplicate into a *lost* event — you cannot order your
way out of it. The discipline is **subscribe first (drop nothing), then dedup at the replay
watermark**: track the highest `Seq` the replay emitted and drop any live event with `Seq <= last`.
Do **not** pause appends or lock across the handoff. 0047 must preserve this
**subscribe-before-replay + dedup-at-watermark** discipline **over the wire** for the cross-instance
pub/sub tail: an observer on instance B subscribes to the loop's `NOTIFY` channel *before* issuing
`Replay(from_seq)` against Postgres, then deduplicates live notifications at the replay watermark.
A `from_seq` reattach must **resume with no gap and no dup**. (Two adjacent obligations from the
same learning travel with this: durable `Replay` must open its own reader so Attach-after-completion
still works, and a send/observe against a finished or unknown loop must return a **distinguishable**
error rather than silently stranding the caller — the durable registry's liveness state is what
makes that distinction resolvable cross-process.)

### 4. Observability hooks designed in now, implementation deferred (D5)

**D5 — thread the observability seams now; emit nothing yet.** The user is prioritizing
observability (OTEL / Prometheus / Grafana / Loki) for the hosted loop, but **full** observability
is its own later change **0051** (built after 0048). 0047 must **design in the hooks** — they are
cheap to add while the durable seam is being defined and expensive to retrofit into a shared,
multi-tenant store — while adding **no** OTEL dependency, exporter, or emission in this change. Two
seam-level constraints:

- **Trace-context propagation.** Every durable-store interface op (`Append`, `Replay`, `Subscribe`,
  loop_id resolution) and the pub/sub notify/listen path must **carry and propagate OpenTelemetry
  trace context** — concretely, a `context.Context` that a future OTEL exporter (0051) can read —
  so cross-instance operations can later be stitched into **one distributed trace**. A loop started
  on instance A and live-tailed from instance B must be **correlatable**: the trace context must
  survive the `LISTEN`/`NOTIFY` hop (pairing with D3) and the durable write (pairing with D1's
  Postgres backend). 0047 adds **no OTEL import or exporter** — it only ensures a `context.Context`
  threads through every interface op and the pub/sub path, and that the interface shape does not
  block later stitching.

- **Tenant + loop + node labeling.** Because `tenant_id` is already first-class (D2), the
  store/pub-sub operations must expose the **`tenant_id` + `loop_id` + `node_id`** triple in a form
  a future metrics/tracing consumer (0051) can label spans and Prometheus series by tenant/model
  **without a schema change later**. `node_id` (already on the `Event` envelope,
  `internal/event/event.go`) folds into the labeling triple.

**Architecture stance.** Observability is a **consumer/projection over the existing typed event
stream** (change 0043's events) plus these internal-timing/trace-context hooks — **not** a retrofit
into the loop. This keeps the Runtime seam **policy-free** (the same invariant ADR-0030 protects):
0047's store/pub-sub seam is designed so **0051 can plug in as a consumer**, exactly as the
session-log projection and `loop.observe` already do, rather than by threading telemetry policy
into the loop itself.

### 5. ADR bookkeeping

A new ADR records the **durable-store seam** (the backend-agnostic contract + the two-implementation
discipline), the **tenant model** (`tenant_id` first-class, app-enforced, portable across backends),
and the **registry-durability decision** that reconciles ADR-0030 (the in-memory map becomes a cache
over the durable registry). Whether ADR-0030 is **superseded or amended** (dated `## Update`) is
settled at build time by `docket-adr`; ADR-0024 and ADR-0025 are **preserved, not superseded** — the
durable seam is designed to honor them, and the ADR should state that explicitly (plaintext birth,
sole Seq allocator, non-blocking Append) so the Postgres backend cannot silently regress them.

## Verification

- **Cross-process durable replay (LOAD-BEARING — the whole point).** A **cold** `fuse loop-server`
  process (empty in-memory `r.loops` registry) resolves a `(tenant_id, loop_id)` started by a
  **different** process, replays its full history from Postgres, tenant-scoped — the exact scenario
  that returns `runtime: loop not found` today. The test must reproduce the motivating bug (attach
  from a fresh process to a loop a prior process started) and assert it now **succeeds**.
- **Cross-instance live tail.** Instance B live-tails a loop running on instance A via pub/sub
  (`LISTEN`/`NOTIFY`): B receives events A emits, with **no gap and no dup**, and **resumes
  correctly after a `from_seq` reattach** — asserting the subscribe-before-replay + dedup-at-watermark
  discipline over the wire, with a concurrent append forced into the handoff gap (a plain sequential
  test cannot see the double-delivery).
- **Tenant isolation.** A store call scoped to tenant X **never** returns tenant Y's events or
  loops; every read path carries and enforces `tenant_id`; a **cross-tenant loop_id lookup FAILS**
  (a lookup of tenant X's loop_id under tenant Y resolves to nothing, not to X's stream).
- **Pluggable seam proven by two implementations.** `fsstore` (local) and Postgres **both** satisfy
  the durable-store seam and pass the **same** behavioral test suite (mirrors
  `two-bindings-one-seam` / the 0045–0046 discipline; each side fed its own production source per
  `parity-test-feeds-each-side-its-own-production-source`).
- **Local/dev path still works with no Postgres.** `fuse` runs locally against `fsstore` with **no
  Postgres required**; single-loop and multi-loop local behavior is unchanged.
- **-race green.** `go test -race ./...` passes, including the cross-instance tail and tenant tests
  (the load-bearing gate, since cross-instance liveness + pub/sub introduce new concurrency).
- **Non-blocking Append preserved under Postgres.** A test asserts the Postgres `Append` +
  `NOTIFY` path never back-pressures the loop (ADR-0025/ADR-0016) — a slow/absent subscriber drops
  the newest event and marks a gap rather than wedging the agent scheduler slot.
- **Observability hooks present, no emission (D5).** Assert (structurally / by construction) that
  every durable-store op and the pub/sub notify/listen path threads a `context.Context` capable of
  carrying trace context, and that `tenant_id` + `loop_id` + `node_id` are exposed on
  telemetry-relevant ops in a consumer-readable form — **without** any OTEL import, exporter, or
  `/metrics` surface in 0047. A cross-instance op (loop on A, tail from B) must be **correlatable**
  once 0051 plugs an exporter in, i.e. the trace context survives the `LISTEN`/`NOTIFY` hop.
- **ADR recorded** for the durable-store seam, the tenant model, and the registry-durability
  decision reconciling ADR-0030 (via `docket-adr` at build).

Any live-model verification turns in this change **must** use a **non-Anthropic** local gateway (a
cheap gateway model via `LLM_GATEWAY_URL`) — never Claude/Anthropic — per project verification
policy. The load-bearing verifications here are storage/concurrency tests, not model-quality tests,
so live-model turns should be minimal.

## Dependencies

- **Depends on 0046** (`multi-loop-host-deglobalize-event-store`, ADR-0030, merged commit 22480d5)
  — 0047 makes durable the per-loop store and loop_id registry that 0046 established as per-loop
  instance state / in-memory map. **Reconcile note (2026-08-10):** the build reconcile confirmed
  `internal/event`, `internal/event/fsstore`, `internal/runtime`, and `internal/loopserver` are all
  present in the working tree now (0046 merged) and their seam shapes match this spec's anchors
  (verified against real symbols, not line numbers). See *Reconcile* at the end of this spec.
- **Blocks / precedes** 0048 (networked binding), 0049 (auth/multi-tenancy — builds on 0047's
  storage-level tenant boundary), and 0050 (client SDK) in the "make the seam hostable" arc.

## Open questions — RESOLVED at build reconcile (2026-08-10)

All six are settled here and carried into the plan. (See *Reconcile* below for the code re-validation
they rest on.)

1. **Testing Postgres without a standing DB → build-tagged integration suite.** The Postgres backend
   and its behavioral tests live behind a `//go:build pgstore` tag and use `testcontainers-go` to spin
   an ephemeral Postgres. A bare `go test ./...` (no tag) **never compiles or runs** the pg files, so it
   stays green with **no Postgres and no Docker present** — the non-negotiable bare-checkout constraint.
   The **one** shared behavioral suite is run against fsstore unconditionally and against Postgres only
   under `-tags pgstore`; the pg path **skips cleanly** (via `t.Skip`) if the container runtime is
   unavailable, so even `-tags pgstore` never hard-fails on a Docker-less box.
2. **Postgres schema/keying → settled.** `events(tenant_id text, loop_id text, seq bigint, ts
   timestamptz, kind text, node_id text, parent_id text, depth int, turn int, payload jsonb)` with
   **PK `(tenant_id, loop_id, seq)`** (also the `Replay(from)` cursor index). Per-loop Seq is allocated
   under a **transaction-scoped per-loop advisory lock** (`pg_advisory_xact_lock` keyed on
   `(tenant_id, loop_id)`) as `seq = COALESCE(MAX(seq),0)+1` scoped to that loop — a **per-loop** total
   order (**not** a global sequence), honoring ADR-0025. `Replay(from)` =
   `WHERE tenant_id=$1 AND loop_id=$2 AND seq>$3 ORDER BY seq`. Registry table
   `loops(tenant_id text, loop_id text, owner_node_id text, live bool, created_at timestamptz,
   updated_at timestamptz)` PK `(tenant_id, loop_id)`. **Retention/rotation: none in 0047**
   (append-only, a documented follow-up), matching fsstore's current no-rotation behavior.
3. **fsstore tenant partitioning → subdirectory-per-tenant.**
   `<baseDir>/<tenant_id>/<loop_id>/events.jsonl`; resolution and replay never cross the tenant prefix.
   An empty/default `tenant_id` maps to a reserved default segment so single-tenant local behavior is
   byte-preserved. Loop-id resolution under a tenant is a directory list under `<baseDir>/<tenant_id>/`.
4. **Reconcile ADR-0030 (supersede vs amend) + liveness → map becomes a cache; ADR recorded at build.**
   The in-memory `r.loops` map becomes a **cache/projection over the durable registry**: the durable
   registry is the source of truth for a loop's *existence* and *liveness/ownership*, while a **live**
   loop is still owned and driven by exactly one instance's in-memory `*loop`. Registry writes (created,
   live-on-instance-I, finished) are the ownership record; each instance derives liveness from the
   durable registry, **not** from a construction-time shared object (guarding the
   `deglobalize-holder-also-per-instance-the-shared-graph` hazard — no shared cached snapshot mutated
   across instances; two concurrent starts on the same `(tenant_id, loop_id)` collide on the PK, not
   silently clobber). **The ADR is recorded via `docket-adr` at Step 6; leading direction is to AMEND
   ADR-0030** with a dated `## Update` (its value-threading + policy-free-seam decisions still hold; 0047
   only makes *existence* durable), escalating to a **superseding** ADR only if the durable-registry seam
   materially reshapes the Runtime value-threading contract. **ADR-0024 and ADR-0025 are preserved.**
5. **Non-blocking Append under Postgres → synchronous durable INSERT, decoupled async NOTIFY.** Append
   performs the durable INSERT synchronously (durability first), then hands the `NOTIFY` to a
   **decoupled bounded async publisher** (a single goroutine draining a bounded queue; overflow drops
   with a gap marker, never blocks). Append thus never blocks on `NOTIFY` delivery or on a slow/absent
   subscriber, preserving ADR-0025/ADR-0016's never-wedge-a-scheduler-slot guarantee. Subscriber fan-out
   remains drop-newest-with-gap. The durable write itself is bounded and off any subscriber's critical
   path.
6. **Observability hook shape (D5) → bare `context.Context`, no OTEL.** A **bare `context.Context`** is
   the first parameter of the durable-store ops (`Append`, `Replay`, `Subscribe`/resolution) and threads
   the pub/sub `LISTEN`/`NOTIFY` hop — **no OTEL import, exporter, or `/metrics` surface** in 0047 — so
   0051 can attach an exporter without an interface change. The `(tenant_id, loop_id, node_id)` labeling
   triple is exposed on telemetry-relevant ops in a consumer-readable form; `node_id` stays the per-event
   envelope value (already on `internal/event.Event`) plus an owning-instance `node_id` captured at store
   construction.

## Assumptions

Judgment calls made in place of asking (the brainstorm is settled; these are authoring-level choices
the build reconcile may adjust):

- **Registry as a facet of the durable-store seam, not a separate top-level seam.** The spec treats
  loop_id resolution + liveness/ownership as part of the durable-store contract (D1/D4 woven
  together) rather than defining an independent registry interface, matching the settled design's
  framing that the registry becomes "durable-store-backed." Whether it is a distinct Go interface or
  additional methods on the store is left to build.
- **In-memory `r.loops` map becomes a cache/projection over the durable registry** (rather than
  being deleted). The settled design says the map "likely becomes a cache over the durable
  registry"; I authored that as the leading direction and flagged the exact liveness/ownership
  representation as an open question.
- **fsstore tenant partitioning = subdirectory-per-tenant** as the leading shape
  (`<baseDir>/<tenant_id>/<loop_id>/events.jsonl`), since the settled design named
  "tenant-scoped subdirectories" as the example — but kept as an open question because the exact
  layout was flagged for planning.
- **`tenant_id` threads through the same entry points 0046/0045 already expose** (`StartLoop` /
  `Attach` / `Observe` / registry lookups); I did not invent new entry points, and did not specify
  the Go type of `tenant_id` (string vs. typed) — left to build.
- **ADR-0024 and ADR-0025 are preserved, not superseded** — the durable seam is authored to honor
  them, and only ADR-0030 is in play for supersede-vs-amend. This follows from the settled tension
  ("must not regress ADR-0024/ADR-0025").
- **Postgres `LISTEN`/`NOTIFY`** is taken as the settled pub/sub mechanism (the design named it as
  chosen); alternative pub/sub layers are out of scope, though the non-blocking-Append integration
  is an explicit open question.
- **Observability is hooks-only in 0047 (D5).** I authored trace-context propagation and the
  `tenant_id`/`loop_id`/`node_id` labeling triple as **seam constraints** (a threaded
  `context.Context` + consumer-readable labels), with **no** OTEL import, exporter, or `/metrics`
  surface — full observability is change 0051 as a consumer over the event stream. I did **not**
  choose between a no-op propagation shim and a bare `context.Context` parameter, nor whether
  `node_id` is a construction value or a per-op arg — both are flagged as open questions.
- **Live-model verification is minimized**, since the load-bearing verifications are
  storage/concurrency tests; where a live turn is needed the non-Anthropic gateway policy applies.

## Reconcile (2026-08-10, build reconcile pass)

Re-validated against the **real current tree** (0046 merged; the packages the original spec said "were
not yet in the local working tree" are now present). Anchored to seam SHAPES, not line numbers.

**Confirmed matching anchors (no drift):**

- `internal/event/store.go` — `EventStore { Append(Event) error; Subscribe() (<-chan Event, func());
  Replay(from Seq) ([]Event, error) }` + `NoopStore`. This is the D1 seam verbatim.
- `internal/event/event.go` — `Event` envelope carries `Seq, TS, NodeID, ParentID, Depth, Turn, Kind,
  Payload`; `node_id` already present (folds into the D5 labeling triple). Payloads plaintext
  `json.RawMessage` (ADR-0024).
- `internal/event/fsstore/store.go` — `FSEventStore` writes `<baseDir>/<sessionID>/events.jsonl`,
  Seq allocated under `s.mu` (`s.seq++; e.Seq = s.seq`), non-blocking drop-newest-with-gap fan-out
  (`Dropped()` counter). ADR-0024/0025 intact. `NewFSEventStore(baseDir, sessionID)` is the extension
  point for D2 tenant partitioning (subdir-per-tenant).
- `internal/runtime/inproc.go` — `inProcRuntime.loops map[string]*loop`; `Observe`/`Attach`/`Send`/
  `Spawn` resolve loop_id **only** via `lookup()` against that in-memory map (`ErrLoopNotFound`,
  `ErrLoopFinished`). This is precisely the cold-process gap D4 fixes. `Runtime.Observe(loopID string)`
  and `Attach(loopID string, from Seq)` today carry **neither** `context.Context` **nor** `tenant_id` —
  the D2/D5 signature changes land here and on `LoopConfig`.
- `internal/loopserver/server.go` — `serveObserve` **already** implements subscribe-before-replay +
  dedup-at-watermark over the JSON-RPC wire (the `replay-live-handoff-dedup-at-watermark` discipline),
  with `observeParams{LoopID, FromSeq}` and gap flagging. 0047 **extends this same discipline** to the
  Postgres pub/sub tail; `reattach_test.go` is the existing proof for the in-proc path.
- `cmd/fuse/loop_server.go` — `buildLoopServerRuntimeDeps` wires `BaseDir: session.DefaultLogDir()`
  and is the **only** multi-loop binding; it is the composition-root seam where a Postgres backend is
  selected. Runtime closes each loop's store on run completion; `Replay` opens its own reader, so
  Attach-after-completion works (an obligation D3 must preserve cross-process).

**Environment:** Go 1.26.5; Docker daemon running locally; **no** Postgres/pgx/testcontainers deps in
`go.mod` — confirming OQ1's constraint (bare `go test ./...` must stay green with no PG). No obsolescence
and no fundamental invalidation; scope stays one change / one PR (D1–D5). All six open questions resolved
above.
