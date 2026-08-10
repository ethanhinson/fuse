---
id: 30
slug: deglobalize-eventstore-multiloop-hosting
title: De-globalize the event store and segment sink; thread per-loop state as values so one process hosts N concurrent loops
status: Accepted
date: 2026-08-10
supersedes: [27]
reverses: []
relates_to: [25, 19]
change: 46
---

## Context

ADR-0027 (change 45) established that the in-process Runtime owns each loop's
`fsstore` event store as instance state, but kept the process-global holders in
`cmd/fuse` (`setActiveEventStore`/`currentEventStore` and the parallel
`setActiveSegmentSink`/`currentSegmentSink`; the ADR-0019 pattern) as a
single-loop compatibility bridge, wired via `Deps.InstallGlobalStore`. It
explicitly named a follow-up to de-globalize those holders before a loop-server
could host multiple concurrent loops: the shared global would clobber across
loops and the impl capped at one live loop per process.

This change (#46) is that follow-up. It is the keystone of the "make the seam
hostable" arc — a prerequisite for the remote-loop change (#48) and for
durable/distributed persistence (#47) — so a single `fuse loop-server` process
must host N concurrent loops, each with a fully isolated event stream, sharing no
mutable process state.

## Decision

The process-global event-store holder (`currentEventStore` /
`setActiveEventStore` / `activeEventStore`) and the parallel segment-sink holder
(`currentSegmentSink` / `setActiveSegmentSink` / `activeSegmentSink`) in
`cmd/fuse` are DELETED. `Deps.InstallGlobalStore` and `Deps.BuildChild` are
removed.

The per-loop event store now flows as an `event.EventStore` VALUE.
`runtime.Deps.BuildAgent` is reshaped into a per-loop factory:

```
func(store event.EventStore, tree *agent.AgentTree, modelID string, toolReg *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error)
```

`StartLoop` passes the loop's opened store and (for the loop-server) a per-loop
tree into `BuildAgent`, which returns the loop's child-builder — subsuming the
old separate `Deps.BuildChild`. This keeps `internal/runtime` free of any
`cmd/fuse` import (the policy-free-seam invariant; the
break-import-cycle-with-agent-free-subpackage learning).

- The **loop-server binding** — the ONLY true multi-loop binding — creates a
  fresh tree + fresh `fsstore` + fresh cloned tool registry per `StartLoop`, so N
  concurrent loops share NO mutable process state. Previously it built one shared
  tree at Deps-construction, capping the process at one live loop.
- The three **single-loop CLI bindings** (one-shot, shell, research-probe) thread
  the store to their eagerly-built child-builder / spawner closures via a
  per-Deps-INSTANCE `eventStoreHolder` (mutex-guarded, instance-scoped —
  replacing the deleted package global). Because these bindings are
  single-loop-per-process, no cross-loop clobber window exists.
- The **segment sink** is threaded as a plain value parameter through the
  agent-builder helpers into `EnableSummarization` (shell passes its own sink;
  other bindings pass nil).

## Consequences

- (+) Genuinely concurrent N-loop hosting in one process is now possible with no
  shared-global contention — unlocking the networked-loop (#48) and
  durable-persistence (#47) arc changes.
- (+) The seam stays policy-free / cmd-free: `internal/runtime` still imports no
  `cmd/fuse`, the store and tree flow as values, and `BuildAgent` inverts the
  dependency.
- (+) Single-loop behavior is byte-identical (one-shot `NoopStore` on empty
  `BaseDir`; the parity test is green).
- (−) `BuildAgent`'s signature is heavier — it now carries the store + tree and
  returns the child-builder.
- (−) The CLI bindings retain a small instance-scoped holder rather than pure
  value-threading — an accepted asymmetry, because those bindings build their
  closures eagerly before `StartLoop` opens the store.

Verification: N concurrent loops on one Runtime / one loop-server Deps each see an
isolated event stream (no cross-loop bleed, contiguous per-loop `Seq` from 1),
proven by `internal/runtime/multiloop_test.go` and
`cmd/fuse/loop_server_multiloop_test.go`, both under `go test -race`. Full suite
plus `-race ./...` green.

## Update

**2026-08-10 (change #47; see ADR-0031):** Change #47
(durable-distributed-event-store) makes a loop's EXISTENCE and liveness durable
and cross-instance-reachable via a backend-agnostic durable store + loop
registry. As part of that, the in-memory `r.loops` map on `inProcRuntime` — which
this ADR established as the per-loop, per-process source of truth for a loop's
existence — is DEMOTED to a cache/projection over the durable registry; the
durable registry becomes the source of truth for existence and
liveness/ownership.

This is an AMENDMENT, not a supersession. Every decision in this ADR still
stands: the process-global holders remain deleted, the per-loop store and tree
still flow as VALUES, `BuildAgent` still inverts the dependency and returns the
child-builder, and `internal/runtime` still imports no `cmd/fuse` (the
policy-free seam). #47 does not reverse any of that — it BUILDS ON it, adding a
durable layer beneath the value-threaded seam. Supersession would wrongly signal
that this ADR's value-threading decision was reversed, which it was not: only the
source of truth for loop existence moved (in-memory map → durable registry), and
a live loop is still owned and driven by exactly one instance's in-memory
`*loop`. No supersession needed.
