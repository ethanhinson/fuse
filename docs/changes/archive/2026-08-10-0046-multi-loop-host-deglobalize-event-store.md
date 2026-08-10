---
id: 46
slug: multi-loop-host-deglobalize-event-store
title: De-globalize the event store + multi-loop host — one process hosts N loops keyed by loop_id
status: done
priority: high
type: refactor
created: 2026-08-10
updated: 2026-08-10
depends_on: [45]
related: [43, 44, 45]
discovered_from: [45]
adrs: [25, 27, 30]
spec: docs/superpowers/specs/0046-multi-loop-host-deglobalize-event-store.md
plan: docs/superpowers/plans/2026-08-10-multi-loop-host-deglobalize-event-store.md
results: docs/results/2026-08-10-multi-loop-host-deglobalize-event-store-results.md
trivial: false
auto_groomable:
branch: feat/multi-loop-host-deglobalize-event-store
pr: https://github.com/ethanhinson/fuse/pull/49
blocked_by:
claimed_at: 
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [0046-multi-loop-host-deglobalize-event-store.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/0046-multi-loop-host-deglobalize-event-store.md) |
| Plan | [2026-08-10-multi-loop-host-deglobalize-event-store.md](https://github.com/ethanhinson/fuse/blob/feat/multi-loop-host-deglobalize-event-store/docs/superpowers/plans/2026-08-10-multi-loop-host-deglobalize-event-store.md) |
| Results | [2026-08-10-multi-loop-host-deglobalize-event-store-results.md](https://github.com/ethanhinson/fuse/blob/feat/multi-loop-host-deglobalize-event-store/docs/results/2026-08-10-multi-loop-host-deglobalize-event-store-results.md) |
| PR | [#49](https://github.com/ethanhinson/fuse/pull/49) |
| ADRs | [ADR-0025](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0025-eventstore-ordering-backpressure.md), [ADR-0027](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0027-runtime-owns-loop-eventstore-global-holder-bridge.md), [ADR-0030](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0030-deglobalize-eventstore-multiloop-hosting.md) |
<!-- docket:artifacts:end -->

## Why

Fuse is meant to be not just a coding agent but a **hostable agent-loop runtime** that any
AI app can target — and the chosen deployment shape is **fuse as a standalone hosted
service**, with the user's other apps as thin network clients over it. A hosted service is
inherently **N concurrent loops per process**. But the Runtime seam that 0045 shipped, while
*designed* for N loops (keyed by `tree.RootID()`), rests on two process-globals that 0045
consciously deferred de-globalizing:

- the **process-global event-store + segment-sink holders** (the ADR-0027 bridge:
  `setActiveEventStore`/`currentEventStore` and the parallel segment-sink pair), a single
  live slot that a second concurrent loop would clobber, causing cross-loop store bleed; and
- **per-session-global Seq allocation** (ADR-0025), which explicitly assumes one process ⇒
  one session.

Both ADRs name this change as their required follow-up. ADR-0027: *"A follow-up change is
required to de-globalize the holders and revisit Seq allocation before a loop-server can host
multiple concurrent loops."* Until these globals become **per-loop instance state**, no
networked or multi-tenant hosting is possible. This is the **keystone** that unlocks the two
hosting milestones the user cares about — (1) a remote loop over the network and (3) durable /
distributed persistence — and it is the last purely-internal, single-tenant, in-process step
before transport and auth (later arc changes) can build on top.

## What changes

- Move **event-store and segment-sink ownership** from the process-global holders to **per-loop
  instance state** on the in-process Runtime, so the child-builder read path resolves each loop's
  own store/sink instead of a single process global. The `Deps.InstallGlobalStore` bridge hook
  (ADR-0027) is retired or reduced to a documented no-op; `internal/runtime` still never imports
  `cmd/fuse`.
- Make `loop.start` in `fuse loop-server` support **N live loops keyed by loop_id** in one process
  (today it caps at one), with `loop.send` / `loop.observe` / replay dispatching by loop_id to the
  correct per-loop store.
- Guarantee **per-loop Seq allocation** — each loop's store is the sole allocator for its own
  stream, with no cross-loop Seq bleed and an isolated `Replay(from Seq)` cursor per loop.
- **Revisit / supersede ADR-0027 and ADR-0025** to record that the global-holder bridge is retired
  and Seq is per-loop-store (the one-process-one-session premise no longer holds).

Detailed design is in the linked spec.

## Out of scope

- **Network transport** — still in-process; WS/HTTP over the seam is a later arc change (C).
- **Durable / distributed event store** — per-session JSONL stays; surviving restart / sharing
  across instances is a later arc change (B).
- **Auth / multi-tenancy** — stays single-tenant, all loops in one trust domain; loop_id ownership
  and per-tenant isolation are a later arc change (D).
- **Changing the `Runtime` interface** — the seam is already N-loop-shaped; this change makes the
  implementation honor it, not reshape the seam.

## Open questions

- Can the `Deps.InstallGlobalStore` hook and the `currentEventStore()`/`currentSegmentSink()`
  accessors be deleted outright, or must a no-op shim remain for any surviving reader?
- How do the `cmd/fuse` child-builder closures receive the per-loop store — constructor param,
  context value, or a per-loop factory closure created at `StartLoop`? (Lean: per-loop factory.)
- ADR-0025 action — supersede vs. a dated `## Update` amendment, depending on how much the Seq
  allocation model actually changes.
- `loop_id` uniqueness / collision and lifecycle for `loop.send`/`observe` to an unknown or
  already-finished loop.

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->

### 2026-08-10 — reconcile against origin/main (7c4bf4d, #45 merged)

Verified the change + spec 0046 against current `origin/main` code. **The design is sound and
accurate; scope holds. No kill, no fundamental invalidation.** Findings that sharpen the build:

- **Confirmed the seam is already N-loop-shaped.** `internal/runtime/inproc.go` owns each loop's
  store as instance state in `loops map[string]*loop` keyed by `tree.RootID()`; `Observe`/`Attach`/
  `Send`/`Spawn` already dispatch by loop_id to the per-loop store. `internal/loopserver/server.go`'s
  `loop.start` already registers the loop in that map and returns its loop_id; `loop.send`/
  `loop.observe` already dispatch by loop_id. So the loopserver layer is *not* structurally capped at
  one loop — the cap is purely the process-global read path underneath.

- **The single blocking global is the child-builder / spawner READ path**, not the store write side.
  All four `runtime.Deps` builders in `cmd/fuse` (one-shot `runtime_binding.go`, shell, research-probe,
  and loop-server `loop_server.go`) wire their `BuildAgent` closure, `BuildChild` child-builder, and
  every `makeSpawner` to `currentEventStore()` — a process-global (`activeEventStore` + RWMutex in
  `run.go`) installed via `Deps.InstallGlobalStore: setActiveEventStore`, which `StartLoop` calls at
  loop-start time. A second concurrent `StartLoop` **clobbers** that one global slot, so the first
  loop's agents then emit into the second loop's store. This is the exact ADR-0027 hazard.

- **De-globalization requires threading the per-loop store as a value, not a closure capture.** The
  `Deps` struct is built once per binding, but `StartLoop` runs per loop and is where the store is
  opened. So the store must flow into `BuildAgent` / `BuildChild` / the spawner wiring as an explicit
  parameter at StartLoop time (the spec's "lean: per-loop factory closure created at StartLoop"), so
  each loop's agents resolve *their own* store. This is a deliberate, called-out adjustment to the
  `Deps.BuildAgent` shape (spec non-goal #5 permits a deliberate exception); the `Runtime` *interface*
  (StartLoop/Send/Spawn/Observe/Attach) is untouched. `InstallGlobalStore` + the `currentEventStore()`
  accessor are then removed (no surviving reader) or reduced to a documented no-op — build-time call.

- **Segment sink is a parallel global with the same hazard, handled asymmetrically today.**
  `activeSegmentSink` (RWMutex in `run.go`) is read at `run.go:350` (`EnableSummarization` inside
  `buildAgentCore`) and set directly in `shell.go:171` via `setActiveSegmentSink` — there is **no
  `Deps` hook** for it (unlike the event store). The loop-server binding does not set a segment sink
  at all. Spec goal is to migrate BOTH; the build must thread the per-loop segment sink through the
  same per-loop path (or scope it per loop) so summarization resolves the owning loop's sink. Because
  only the shell installs one today, the multi-loop-server path is the load-bearing case; shell stays
  single-loop-per-process in practice, so its segment-sink wiring must stay byte-identical.

- **Existing pin to update:** `cmd/fuse/event_store_holder_test.go` directly exercises
  `setActiveEventStore`/`currentEventStore`; it will be removed or rewritten with the global. Other
  callers of these accessors: `run.go` (defs + `currentSegmentSink()` at :350), `shell.go`,
  `two_bindings_parity_test.go`, `segment_gateway_test.go`, `segment_wiring_test.go`.

- **ADR actions (settle at build via docket-adr):** supersede **ADR-0027** (the global-holder bridge
  is retired). For **ADR-0025**, Seq is already per-`fsstore`-instance, so the allocation model does
  not change — a dated `## Update` amendment (one-process-one-session premise retired; per-loop Seq
  proven isolated) is the likely action rather than a supersession. Final call at build time.

- No adjacent follow-up work to auto-capture (AUTO_CAPTURE disabled anyway). Scope unchanged; spec
  needs no body edit — findings above are build-directing detail, not scope drift.
