---
id: 46
slug: multi-loop-host-deglobalize-event-store
title: De-globalize the event store + multi-loop host — one process hosts N loops keyed by loop_id
status: in-progress
priority: high
type: refactor
created: 2026-08-10
updated: 2026-08-10
depends_on: [45]
related: [43, 44, 45]
discovered_from: [45]
adrs: [25, 27]
spec: docs/superpowers/specs/0046-multi-loop-host-deglobalize-event-store.md
plan:
results:
trivial: false
auto_groomable:
branch: feat/multi-loop-host-deglobalize-event-store
pr:
blocked_by:
claimed_at: 2026-08-10T18:52:53Z
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [0046-multi-loop-host-deglobalize-event-store.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/0046-multi-loop-host-deglobalize-event-store.md) |
| ADRs | [ADR-0025](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0025-eventstore-ordering-backpressure.md), [ADR-0027](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0027-runtime-owns-loop-eventstore-global-holder-bridge.md) |
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
