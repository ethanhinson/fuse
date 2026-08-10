---
id: 27
slug: runtime-owns-loop-eventstore-global-holder-bridge
title: Runtime owns the loop event store as instance state; process-global holders kept as a single-loop compatibility bridge
status: Superseded by ADR-30
date: 2026-08-10
supersedes: []
reverses: []
relates_to: [25, 19, 16]
change: 45
---

## Context

The new `internal/runtime` seam (change 45) needs to own a loop's
`event.EventStore` so a Runtime instance can host a loop and expose Observe
(Subscribe) / Attach (Replay). Historically the event store and segment sink are
installed via process-global lock-guarded holders in `cmd/fuse`
(`setActiveEventStore`/`currentEventStore`,
`setActiveSegmentSink`/`currentSegmentSink`; the ADR-0019 pattern).

ADR-0025 established that the per-session-global Seq allocator assumes ONE
PROCESS ⇒ ONE SESSION, and its own consequences note that a
multi-session-per-process store "would revisit Seq allocation." So the spec's
open question — should Runtime own the holders as instance state (needed for a
server hosting more than one loop) or keep globals and defer de-globalization —
could not be answered "full multi-loop" without reopening the Seq-allocation
design, which is out of scope for this change.

## Decision

The in-process Runtime (`internal/runtime.inProcRuntime`) OWNS each loop's
`fsstore` event store as INSTANCE state, keyed by loopID (`tree.RootID()`) in a
`loops map[string]*loop`. StartLoop opens the store (or `event.NoopStore{}` when
`Deps.BaseDir == ""`, preserving byte-identical one-shot behavior), wires the
agent's event sink to THAT store, and closes it when the loop's run goroutine
completes.

To keep the existing package-global readers (child-builder closures in `cmd/fuse`
that call `currentEventStore()`) working during single-loop operation, the
binding passes a `Deps.InstallGlobalStore func(event.EventStore)` hook (wired to
`setActiveEventStore`), which StartLoop calls — so `internal/runtime` NEVER
imports `cmd/fuse`, yet the global bridge still installs.

The interface is designed for N loops (loopID-keyed maps), but the
IMPLEMENTATION remains single-loop-per-process for this change, because a true
multi-loop host requires revisiting ADR-0025's process-global Seq allocator.
Full de-globalization / a multi-loop Seq allocator is explicitly a LATER change.

## Consequences

- (+) The Runtime is a real composition seam that owns loop lifecycle + durable
  event history, enabling Observe/Attach without a process global as the source
  of truth.
- (+) `internal/runtime` stays free of any cmd-layer import (the
  InstallGlobalStore hook inverts the dependency); the policy-free seam
  invariant holds.
- (+) One-shot behavior stays byte-identical (NoopStore when BaseDir is empty; no
  new event log).
- (−) The process-global holders survive as a compatibility bridge, so genuinely
  concurrent multi-loop hosting in one process is NOT yet supported — it would
  clobber the shared global and collide on the per-session Seq allocator
  (ADR-0025). The loopID-keyed map makes the N-loop shape real in the interface
  but the impl caps at one live loop per process.
- (−) A follow-up change is required to de-globalize the holders and revisit Seq
  allocation before a loop-server can host multiple concurrent loops.
