<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0044 — Spawn handle-async — location-transparent spawning behind a handle-returning contract](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0044-spawn-handle-async.md)**
<!-- docket:backlink:end -->

# Plan 0044 — Spawn handle-async

> Authoring note: the configured `plan` skill (`superpowers:writing-plans`) is unavailable in this
> environment, so this plan was authored inline by the implementer (auto-fallback per the docket
> Skill layer). Method is the implementer's; the artifact/stop-point (a plan file recorded in
> `plan:`) is unchanged.

Reconciled against `origin/main @ 6720354` (post-0043 merge). Spec:
`docs/superpowers/specs/0044-spawn-handle-async.md` (on `docket`). This plan is TDD-first: each
task writes/extends a failing test before the implementation, and the whole suite runs green
(`go test -race ./...`) at the gate.

## Design decisions settled here (from reconcile)

- **D-cycle (import cycle).** `internal/tools` must NOT import `internal/agent`. Returning
  `agent.AgentHandle` directly from `tools.SpawnFunc` reintroduces the cycle. **Resolution: define
  a minimal handle interface in `internal/tools`** that `*agent.AgentHandle` satisfies — the
  spec's option 2. Chosen over moving `AgentHandle`/`SpawnDone` to an agent-free package because
  the handle is the agent-dependent value itself (no separable schema/reader split as in ADR-0017
  / finding `break-import-cycle-with-agent-free-subpackage`), and a type-move would ripple through
  `spawn.go`, `internal/pipeline`, and many tests. The tool needs only the result string + error
  from `Wait()` — a tiny surface. **Record ADR.**
  - Interface shape (tools-side):
    ```go
    // SpawnResult is the tools-visible completion of a spawned child: the prose
    // result and the run error. It deliberately omits the structured value — only
    // Go callers (pipeline, Runtime) consume that, via *agent.AgentHandle directly.
    type SpawnResult struct { Result string; Err error }

    // SpawnHandle is the agent-free handle the tools seam awaits. *agent.AgentHandle
    // satisfies it; this keeps internal/tools free of an internal/agent import.
    type SpawnHandle interface { WaitResult() SpawnResult }
    ```
  - `*agent.AgentHandle` grows a `WaitResult() tools...`? No — that would make `agent` import
    `tools` (the reverse cycle). Instead the **cmd/fuse adapter** (`spawnFuncFrom`) returns a
    tiny local wrapper that holds the `agent.AgentHandle` and implements `tools.SpawnHandle` by
    calling `h.Wait()` and mapping `SpawnDone{Result,Err}` → `tools.SpawnResult`. The interface
    lives in `tools`; the adapter satisfying it lives in `cmd/fuse` (the composition root) — the
    same "root wires the heavy side" shape as ADR-0017, without a type move.
  - `SpawnFunc` becomes: `type SpawnFunc func(ctx, SpawnRequest) (SpawnHandle, error)`.

- **D-emit (emission site).** Relocate spawn lifecycle emission from the three cmd-site child
  builders (0043's `emitSpawnStart`/`emitSpawnDone`) **into the `Spawner`** (`internal/agent/
  spawn.go`), the single choke point every spawn passes through and where `SpawnDone{Result,
  Structured}` is already built. `internal/agent` already imports `internal/event`, so no new
  cycle. The `Spawner` gains an `EventStore` via a new `WithEventStore` option (nil ⇒
  `event.NoopStore{}` — inert, as today).
  - `spawn.start` (`KindSpawnStart`) emits at **admission** in `spawnLocal`, right after the node
    is created/added to the tree (truly before the child runs — stronger than 0043's post-build
    cmd-site emit).
  - `spawn.done` (`KindSpawnDone`) emits from the completion path in `spawnLocal`'s goroutine,
    right where `SpawnDone{Result, Err, Structured}` is assembled, before the channel send.
  - **Result sourcing:** the Spawner emits `SpawnDone.Result` (the child-builder's returned
    string, i.e. `childResult` output). 0043's cmd-site emit used `lastAssistantText(msgs)`.
    These differ only on max-turns/loop-detected (childResult prepends a "[stopped: …]" marker).
    **Byte-identity guard:** the projected session log (`projectEventToLog`) reads only
    NodeID/ParentID/Label/Depth/Kind from the payload — never Result — so the projected log stays
    byte-identical. The `Result`/`Structured` carried on the event is (arguably) more faithful.
    Documented deviation; asserted by test.
  - **Remove the now-redundant cmd-site emitters** (`emitSpawnStart`/`emitSpawnDone` calls in
    main.go, shell.go, research_probe.go) to avoid **double emission**. Keep `event_spawn.go`'s
    `projectEventToLog` / `startProjectedLogConsumer` (the observation/projection path is
    unchanged) and shell.go's direct `sessLog.Write` (independent, unchanged). The
    `emitSpawnStart`/`emitSpawnDone` funcs themselves move to the Spawner; delete the cmd/fuse
    copies and their unit test once the Spawner-level test covers emission.

- **D-pipeline.** `internal/pipeline` calls `sp.Spawn(...)` and consumes `h.Result()`/`h.Wait()`
  on `agent.AgentHandle` directly — never `tools.SpawnFunc`. Unaffected. No pipeline edits. A
  regression test asserts the handle contract is intact.

## Tasks (TDD, each ends green)

### Task 1 — `Spawner` emits spawn.start / spawn.done (agent side)
- **Test first** (`internal/agent/spawn_test.go` or a new `spawn_event_test.go`): construct a
  `Spawner` with `WithEventStore(rec)` (a recording `event.EventStore` test double) and a
  `WithChildBuilder` returning a fixed result; call `Spawn`, `handle.Wait()`; assert the store
  received exactly one `KindSpawnStart` (at admission, with ChildNodeID/Label/Task) and one
  `KindSpawnDone` (ChildNodeID/ParentID/Label/Depth/Result, and Structured when the child returned
  a captured return_result value). Assert a nil/absent store (default `NoopStore`) is inert (no
  panic, spawn still works). Assert ordering: start Seq < done Seq.
- **Implement:** add `eventStore event.EventStore` field + `WithEventStore` option to `Spawner`
  (default `event.NoopStore{}` in `NewSpawner`). Emit `KindSpawnStart` in `spawnLocal` right after
  `addNode`; emit `KindSpawnDone` in the goroutine just before `doneCh <- SpawnDone{...}`, sourcing
  Result/Err/Structured from the same values. Marshal `Structured` (`any`) → `json.RawMessage`
  (nil when absent). Best-effort: `_ = store.Append(...)`, never blocks (ADR-0016).
- **Guard:** emission must not alter slot-yield timing (finding
  `slot-cap-yield-while-blocked-on-children`) — emit is a synchronous, non-blocking `Append`, no
  new goroutine, no wait.

### Task 2 — `SpawnFunc` becomes handle-returning; tool blocks internally (tools side)
- **Test first** (`internal/tools/spawn_agent_test.go`): a fake `SpawnHandle` returning a known
  `SpawnResult{Result:"hi", Err:nil}`; a `SpawnFunc` returning it; assert `SpawnAgentTool.Execute`
  returns `Output == "hi" + budgetLine + quotaLine` (model-facing contract byte-unchanged: prose +
  budget + quota). Add a case where the handle's `WaitResult().Err != nil` → `IsError`, output
  `spawn_agent: <err>`, no budget line (matches current error posture). Add a case where
  `spawn(...)` itself returns an error (spawn refused, e.g. budget) → `IsError`, verbatim.
- **Implement:** change `type SpawnFunc = func(ctx, SpawnRequest) (SpawnHandle, error)`; add
  `SpawnHandle`/`SpawnResult` to `internal/tools`. In `Execute`, call `handle, err := t.spawn(...)`;
  on err return the current error result; else `done := handle.WaitResult()`; on `done.Err` return
  the error result; else `Result{Output: done.Result + t.budgetLine() + t.quotaWarning()}`. Keep
  `budgetLine`/`quotaWarning`/`TighterBudget` untouched. Update `NewSpawnAgentTool*` signatures
  only if the `spawn` field type changed (it did — same constructors, new closure type).

### Task 3 — cmd/fuse adapter returns the handle; wire Spawner event store (composition root)
- **Test first** (`cmd/fuse`): a table/behavioral test that `spawnFuncFrom` now returns a
  `tools.SpawnHandle` whose `WaitResult()` yields `{Result: done.Result, Err: done.Err}`, and that
  the parent slot is yielded across the wait then unyielded (preserve current
  `YieldSlot`/`UnyieldSlot` timing — the deadlock guard). Reuse/extend existing spawnFuncFrom
  coverage if present; else a focused test with a stub spawner.
- **Implement:**
  - `spawnFuncFrom` (run.go): keep the spawner build + `YieldSlot`; instead of pre-`Wait()`ing and
    returning a string, wrap the `agent.AgentHandle` in a small unexported `cmdSpawnHandle` that
    implements `tools.SpawnHandle.WaitResult()` by doing `sched.YieldSlot`/`h.Wait()`/
    `UnyieldSlot` at await time (so the slot-yield stays paired around the actual block, now
    performed when the tool awaits). Return `(cmdSpawnHandle{...}, nil)`. Careful: the yield must
    still happen around the wait — move the yield/unyield into `WaitResult()` so the block-time
    semantics are preserved (parent yields its slot only while actually blocked).
  - Add `agent.WithEventStore(currentEventStore())` to each `makeSpawner` in **all three** sites
    (main.go, shell.go, research_probe.go — enumerate by grep per finding
    `patch-every-cloned-child-builder`).
  - Remove the `emitSpawnStart(...)`/`emitSpawnDone(...)` calls from all three child builders
    (Spawner now owns emission). Delete `emitSpawnStart`/`emitSpawnDone` from `event_spawn.go` and
    their unit test (`event_spawn_test.go`) IF nothing else references them; keep
    `projectEventToLog` + `startProjectedLogConsumer` + `lastAssistantText`. Re-grep to confirm no
    orphan references.
  - Confirm `spawn.start` Task field: Spawner has `opts.Task` — carried into `SpawnStartPayload`.

### Task 4 — pipeline regression + full-suite gate
- **Test:** an `internal/pipeline` test (extend existing) asserting a spawned step's
  `h.Result()`/`h.Wait()` still return the same `SpawnDone{Result, Structured}` — the handle
  control path is intact and unaffected by the SpawnFunc change. Assert ADR-0016 slot-yield: a
  parent awaiting a child in a slot-pressured pool does not deadlock (existing engine tests cover
  this; run under `-race`).
- **Gate:** `go test -race ./...` green. `grep` confirms no `(string, error)` spawn adapter
  survives (D3 acceptance): `SpawnFunc` is handle-returning, `spawnFuncFrom` returns a handle, no
  compat wrapper.

## Verification (maps to spec)
- Model-visible unchanged: Task 2 asserts identical `Output` (prose+budget+quota). Optionally a
  gateway-double check (finding `verify-tool-loop-at-gateway-seam`) if a fast harness exists;
  otherwise the unit assertion on `Execute` output is the gate.
- Handle contract intact: Task 4.
- spawn.start + spawn.done on the stream carrying Result/Structured, visible to a Subscribe()r with
  no handle: Task 1.
- No compat wrapper remains: Task 4 grep gate.

## Out of scope (unchanged)
Runtime interface extraction (#3); any networked backend; model-facing async fan-out; removing
`handle.Wait()`/`Result()`.
