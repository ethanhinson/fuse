<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0046 — De-globalize the event store + multi-loop host — one process hosts N loops keyed by loop_id](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0046-multi-loop-host-deglobalize-event-store.md)**
<!-- docket:backlink:end -->

# Implementation Plan — 0046 De-globalize the event store + multi-loop host

## Goal

Remove the two process-globals that keep the already-N-loop-shaped `internal/runtime`
seam structurally single-loop-per-process, so one `fuse loop-server` process can host N
concurrent loops each with a fully isolated event stream:

1. **Event-store read path** — the `cmd/fuse` child-builder / spawner closures and the
   `BuildAgent` closure currently resolve the loop's store via the process-global
   `currentEventStore()` (installed at `StartLoop` time via `Deps.InstallGlobalStore:
   setActiveEventStore`). A second concurrent `StartLoop` clobbers that one slot. Thread
   the **owning loop's store** to those closures as a value so each loop's agents resolve
   *their own* store; retire `InstallGlobalStore` + `currentEventStore()`/`setActiveEventStore`.
2. **Segment-sink read path** — the parallel process-global `currentSegmentSink()` (read in
   `run.go` `EnableSummarization`, set directly in `shell.go`). Migrate it to per-loop the
   same way so N concurrent loops do not clobber each other's summarization sink.
3. Prove **per-loop Seq allocation** is isolated (each loop's `fsstore` is already its own
   sole Seq allocator) with an N-concurrent-loop `-race` test.
4. ADR bookkeeping: supersede ADR-0027 (the global-holder bridge is retired); amend
   ADR-0025 (one-process-one-session premise retired; Seq is per-loop-store).

Out of scope (do NOT implement): network transport, durable/distributed store, auth /
multi-tenancy, client SDK, any change to the `Runtime` *interface* method set
(StartLoop/Send/Spawn/Observe/Attach). The `Deps.BuildAgent` shape MAY change (a deliberate,
called-out exception per spec non-goal #5) to carry the per-loop store.

## Architecture

```
        cmd/fuse (composition root — ONLY importer of internal/runtime)
   one-shot / shell / research-probe (binding #1)     loop-server (binding #2)
        │  each Deps builder closes over renderer/gate/tools           │
        └──────────── construct Deps + call StartLoop ─────────────────┘
                                    │
              internal/runtime.Runtime  (policy-free seam, unchanged interface)
                 owns each loop's store as instance state in loops[loopID]
                 passes THAT store into BuildAgent / BuildChild per loop
                                    │
         internal/agent   internal/event   internal/event/fsstore
```

The leak is on the **read side**: each loop already owns its `fsstore` (write side), but the
`cmd/fuse` closures read the store back through a single process-global. The fix inverts that
last dependency: the Runtime, which already holds the per-loop store, hands it to the
binding's construction closures explicitly. `internal/runtime` stays free of any `cmd/fuse`
import (learning `break-import-cycle-with-agent-free-subpackage`); the store flows as an
`event.EventStore` value, not by importing cmd.

**Site enumeration is by grep at build time, never from this list** (learning
`patch-every-cloned-child-builder`): the four `runtime.Deps` builders today are
`buildOneShotRuntimeDeps` (`runtime_binding.go`), `buildShellRuntimeDeps`
(`runtime_binding.go`), `buildResearchProbeRuntimeDeps` (`runtime_binding.go`), and
`buildLoopServerRuntimeDeps` (`loop_server.go`). Re-grep for `currentEventStore(`,
`currentSegmentSink(`, `setActiveEventStore`, `setActiveSegmentSink`,
`InstallGlobalStore` at fix time to confirm the count.

## Tech Stack

- Go 1.26, module `github.com/ethanhinson/fuse`.
- Tests: `go test ./...`; race gate `make test-race` (`go test -race ./...`) — **load-bearing**,
  the whole change removes shared mutable process state under concurrency.
- No new third-party dependencies.

## Global Constraints

- TDD, bite-sized: each code step is (failing test → see it fail → minimal impl → pass →
  commit). Each task ends in an independently testable deliverable.
- Policy-free seam preserved: `internal/runtime` imports NO renderer/TUI/MCP/CLI type and
  NO `cmd/fuse` — grep+`go list -deps` asserted (existing `policy_free_test.go` /
  `import_direction_test.go` must stay green).
- Single-loop behavior byte-identical: one-shot still `NoopStore` on `BaseDir == ""`; shell,
  research-probe, and single-loop loop-server event streams unchanged. The existing
  `two_bindings_parity_test.go` byte-equivalence guard must stay green.
- The race proof is a test that drives N loops **concurrently**, not sequentially (learning
  `race-invisible-to-race-detector-without-concurrent-test`); a sequential N-loop test would
  stay green even with the global still in place.

## Decision Notes

- **D-1 (how the per-loop store reaches the closures).** `Deps` is built once per binding,
  but `StartLoop` runs per loop and is where the store is opened. So the store cannot be a
  closure capture over a global; it must flow as a parameter. Chosen shape:
  `Deps.BuildAgent func(store event.EventStore, modelID string, toolReg *tools.Registry)
  (*agent.Agent, string, error)` and the binding's child-builder / spawner wiring resolves
  the store from a per-loop factory the Runtime seeds at StartLoop. Concretely, the Runtime
  passes the loop's `store` into `BuildAgent`, and for spawns the Runtime already passes
  `lp.store` into its own `agent.NewSpawner(... WithEventStore(lp.store) ...)` (see
  `inproc.go` Spawn) — so the child-builder closures must take the loop store from the
  `agent`-level context they already receive, or the binding threads it via a small per-loop
  holder created at StartLoop instead of the process global. Final concrete shape is settled
  in Task 2 against the real closures; the invariant is: **no process-global, store resolved
  per loop, no `cmd/fuse` import into `internal/runtime`.**
- **D-2 (segment sink).** Same treatment, but only `shell.go` installs one today and only
  `run.go` `EnableSummarization` reads it; loop-server installs none. Migrate the read to
  resolve the per-loop sink (nil when the binding installs none). Shell stays
  single-loop-per-process, so its behavior must stay byte-identical.
- **D-3 (ADR-0025 amend vs supersede).** Seq is already per-`fsstore`-instance; the
  allocation model does not change. Lean: a dated `## Update` amendment recording the
  one-process-one-session premise is retired and Seq is proven per-loop isolated. Final call
  by `docket-adr` at review.

## Tasks

### Task 1 — N-concurrent-loop isolation test at the runtime seam (RED first)

Add `internal/runtime/multiloop_test.go`: construct ONE `Runtime` (via `New(Deps{...})` with
a scripted completer, as `inproc_test.go` does) and start **N ≥ 3** loops concurrently
(goroutines), each with a real per-loop `fsstore` under one `BaseDir`. Drive distinct events
into each loop, then assert via `Attach`/`Replay` that:
- each loop's stream contains ONLY its own events (no cross-loop bleed),
- each loop's Seq run is contiguous and monotonic from 1 (per-loop allocator),
- no two loops share a Seq origin.

This test passes at the **runtime seam** even before the cmd-level fix (the seam already owns
per-loop stores) — its role is the permanent regression guard and the `-race` proof at the
seam. Commit. (The cmd-level clobber is proven separately in Task 4.)

### Task 2 — Thread the per-loop store into `Deps.BuildAgent`; delete `InstallGlobalStore`

- Change `Deps.BuildAgent` to receive the loop's `event.EventStore` (D-1). Update `inproc.go`
  `StartLoop` to pass the store it opened into `BuildAgent` and to STOP calling
  `InstallGlobalStore`; delete the `InstallGlobalStore` field.
- Update all four `cmd/fuse` Deps builders (enumerate by grep): the `BuildAgent` closure uses
  the passed-in `store` for `a.SetEventSink(store)` instead of `currentEventStore()`.
- Update the runtime unit tests (`inproc_test.go`, `inproc_task3_test.go`, `runtime_test.go`)
  to the new `BuildAgent` signature.
- Run `go test ./internal/runtime/... ./cmd/fuse/...`; green. Commit.

### Task 3 — Thread the per-loop store into the child-builder / spawner closures

The child-builder closures and every `makeSpawner` in the four builders resolve the store via
`currentEventStore()`. Rework each builder so the child-builder + spawner wiring closes over
the **loop's store** rather than the global. Because the store is opened at StartLoop (after
Deps is built), route it through the same per-loop seam as Task 2 (a per-loop factory the
Runtime seeds, or the store the Runtime already passes into its own Spawner for `Spawn`, and a
binding-local per-loop holder for the root-loop's own fan-out — settled here against real
code). Then:
- Delete `setActiveEventStore` / `currentEventStore` from `run.go` and remove
  `cmd/fuse/event_store_holder_test.go` (or rewrite it to assert the per-loop path).
- Re-grep to confirm zero remaining `currentEventStore(` / `setActiveEventStore` references.
- `go test ./cmd/fuse/... ./internal/...`; green. Commit.

### Task 4 — Multi-loop `loop.start` proof in loop-server (concurrent, -race)

Add a loopserver-level (or `cmd/fuse`-level) test that starts N loops via `loop.start` on ONE
server/runtime **concurrently**, drives events into each, and asserts each `loop.observe` /
`loop.attach` sees ONLY its own loop's events with monotonic per-loop Seq. This is the test
that would have gone RED with the global still in place (learning
`race-invisible-to-race-detector-without-concurrent-test`). If a real-binary check is needed,
use a scripted `LLM_GATEWAY_URL` httptest double (learning `verify-tool-loop-at-gateway-seam`).
Run under `-race`. Commit.

### Task 5 — Migrate the segment-sink global (D-2)

- Thread the per-loop segment sink through the same per-loop path; make `EnableSummarization`
  resolve the owning loop's sink (nil when none installed). Update `shell.go` to install its
  sink per-loop rather than into the process global; delete `setActiveSegmentSink` /
  `currentSegmentSink` and the `activeSegmentSink` global.
- Keep shell behavior byte-identical (single loop in practice). Update
  `segment_gateway_test.go` / `segment_wiring_test.go` as needed.
- Re-grep to confirm zero remaining `currentSegmentSink(` / `setActiveSegmentSink` references.
- `go test ./...`; green. Commit.

### Task 6 — Full suite + race gate + verification

- `go test ./...` green.
- `make test-race` (`go test -race ./...`) green — including the Task 1 and Task 4 concurrent
  tests. This is the load-bearing gate.
- Confirm `policy_free_test.go` and `import_direction_test.go` still green (seam stayed
  policy-free and `cmd/fuse`-free).
- Confirm `two_bindings_parity_test.go` still green (single-loop byte-equivalence preserved).
- Commit any final test/doc touch-ups.

## Verification checklist (maps to spec 0046 Verification)

- [ ] N concurrent loops, isolated streams — Task 1 (seam) + Task 4 (loop.start), both `-race`.
- [ ] No shared-global contention — the `currentEventStore`/`currentSegmentSink` globals are
      deleted (grep proves zero references); Task 1/4 concurrent tests prove no clobber.
- [ ] Single-loop behavior unchanged — parity test green; one-shot still NoopStore on empty
      BaseDir; shell/research-probe streams unchanged.
- [ ] `go test ./...` and `go test -race ./...` both green.
- [ ] ADR-0027 superseded, ADR-0025 amended (recorded via docket-adr at review).
