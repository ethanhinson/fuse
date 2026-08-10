<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0044 — Spawn handle-async — location-transparent spawning behind a handle-returning contract](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0044-spawn-handle-async.md)**
<!-- docket:backlink:end -->

# Spec 0044 — Spawn handle-async: location-transparent spawning behind a handle-returning contract

## Problem

Fuse's subagent spawn seam is synchronous at exactly the layer that must become
location-transparent for the runtime to scale horizontally. The tools→agent injection
type collapses an already-async internal handle back into a blocking string:

```go
// internal/tools/spawn_agent.go:35
type SpawnFunc func(ctx context.Context, req SpawnRequest) (result string, err error)
```

That signature — synchronous, returns a finished `string` — is the shape that a networked
spawn backend **cannot** honor: a child that runs in another process or on another machine
cannot return its finished result inline on the calling goroutine. To fan out horizontally
(the runtime's #3 concern), `spawn` must return a *handle* the caller can await, with the
result arriving asynchronously — and the interface must look identical whether the child
runs in-process or remotely. Getting this wrong means horizontal scale is a rewrite; getting
it right means a networked backend is a new implementation behind an unchanged seam.

The good news is that **the async, handle-shaped contract already exists internally** — only
the public `SpawnFunc` adapter hides it:

- `Spawner.Spawn(ctx, SpawnOpts) (AgentHandle, error)` (`internal/agent/spawn.go:217`)
  already returns a handle, not a string.
- `AgentHandle` (`spawn.go:110`) carries `Done <-chan SpawnDone`, a memoized `Wait()
  SpawnDone` (`spawn.go:145`), and `Result() (any, error)` (`spawn.go:155`).
- `SpawnDone` (`spawn.go:97`) already splits `Result string` from `Structured any` — the
  work from changes 0024/0042 already separated "the child ran" from "here is its typed
  result."

So the internal machinery is already async and handle-shaped. It is the **public
`SpawnFunc` type** and its three cmd-site adapters that re-collapse the handle to a blocking
string. This change stops hiding the handle rather than building async spawn — the crux the
`return_result`/`SpawnDone` line of work already de-risked.

This is the **second of three changes** toward the `Runtime` seam. It **depends on change
0043** (the EventStore stream): a completed child's result is surfaced as a `spawn.done`
event on that stream, so this change wires 0043's `KindSpawnDone` kind.

1. `runtime-eventstore-seam` (0043, done first) — the typed event stream.
2. **`spawn-handle-async` (this change)** — `SpawnFunc` → handle-returning; child result
   rides `spawn.done`.
3. `runtime-interface-and-binding` — extract the named `Runtime` and stand up binding #2.

## Scope of this change (hard boundaries)

**In scope.** Change the public `SpawnFunc` from `(string, error)` to a handle-returning
contract; migrate the three cmd-site child builders and the `SpawnAgentTool` wiring in one
PR (the "big-bang" signature change); emit a `spawn.done` event (0043's `KindSpawnDone`) at
child completion, carrying `SpawnDone{Result, Structured}`.

**Out of scope — named later changes / deliberate non-goals.**

- **The `Runtime` interface extraction and binding #2.** That is change 3
  (`runtime-interface-and-binding`). This change makes `spawn` handle-shaped; it does not
  name or expose a `Runtime`.
- **A networked spawn backend.** In-process only. The interface must merely *permit* a
  networked backend later (a handle a caller awaits, a serializable request) — no remote
  transport is built here.
- **Model-facing tool semantics.** The `spawn_agent` tool's contract to the *model* is
  byte-unchanged (see the decision below): the model still receives a text result. The
  model does not gain a poll/await tool.
- **Removing `handle.Wait()`/`Result()` as the in-process control path.** They stay — the
  event is an *observation* path, not a replacement for programmatic control.

## Decisions

### D1 — `SpawnFunc` becomes handle-returning; the tool blocks internally

Change the public seam to return the handle that `Spawner.Spawn` already produces:

```go
// internal/tools/spawn_agent.go — the seam, after this change
type SpawnFunc func(ctx context.Context, req SpawnRequest) (AgentHandle, error)
```

(The exact handle type placement — whether `AgentHandle` moves to an agent-free package to
keep `tools` free of an `agent` import, mirroring the ADR-0017 split — is a planning
question, see *Open questions*. The seam's *shape* is settled: a handle, not a string.)

**The model-facing `spawn_agent` tool keeps its current contract.** The tool internally
calls `handle.Wait()` and returns the `Result` string to the model exactly as today — the
budget line, quota line, and prose result are all unchanged (`spawn_agent.go` result
assembly). The async seam is exposed to **Go callers** (the pipeline engine, and change 3's
`Runtime`); the model's tool behavior does not change at all. This keeps the big-bang
signature change **purely structural**: no model behavior shifts, so the change is testable
as "same model-visible output, new Go-visible contract."

Rationale: fanning the async handle all the way out to the *model* (a spawn-then-poll tool
protocol) is real scope — it changes how models orchestrate — and belongs to the runtime's
higher surfaces, not this structural change. The handle serves in-process Go orchestration
now; model-level async fan-out, if ever wanted, is a separate deliberate change.

### D2 — child result flows two ways: the handle (control) and `spawn.done` (observation)

On child completion, the result reaches consumers through **both** paths, neither removed:

- **Handle (in-process control).** `handle.Wait() SpawnDone` and `handle.Result() (any,
  error)` remain the programmatic path. The pipeline engine and any Go caller that needs the
  child's value awaits the handle. This preserves the ADR-0016 slot-yield timing that
  `Wait()` currently guarantees (a waiting parent yields its scheduler slot; the event
  stream must not alter that).
- **`spawn.done` event (observation).** At completion, emit a `spawn.done` event (0043's
  `KindSpawnDone`) carrying `SpawnDone{Result, Structured}` (plus the envelope's `NodeID`/
  `ParentID`/`Seq`). Observers — the TUI, an external binding, a durability layer — see the
  completion on the one typed stream without holding a handle.

The split is the runtime's core philosophy made concrete: **the handle is for control, the
event is for observation.** One completion, two consumers, neither coupling the other. The
handle path must not read *from* the stream (that would couple in-process control to a
best-effort observation channel and risk the `Wait()` timing guarantee); emission is a
best-effort side effect of the same completion the handle already delivers.

### D3 — big-bang migration of the three cmd-site builders in one PR

All three `makeSpawnFunc` adapters migrate together in one change:

- `cmd/fuse/main.go:166-228` (one-shot)
- `cmd/fuse/shell.go:243-345` (interactive shell)
- `cmd/fuse/research_probe.go:138-254` (research-probe headless)

Each currently builds a `SpawnFunc` closure that calls `Spawner.Spawn`, then **awaits the
handle and returns the string** — pre-collapsing the handle. After this change each returns
the handle directly; the *tool* (D1) is the single place that awaits. Big-bang (not
incremental behind a compat wrapper) is the deliberate choice: a lingering `(string, error)`
wrapper would let the collapsed shape persist and defeat the location-transparency the
change exists to establish. One PR, one clean end state, no compat cruft.

Because the internal `Spawner.Spawn` already returns `AgentHandle`, the migration is
mechanically small: the edits are (1) the `SpawnFunc` type, (2) the three adapters stop
awaiting and pass the handle through, (3) the tool awaits (D1), (4) `spawn.done` emission
(D2). No new async machinery is written.

## Open questions (resolve during planning)

- **Handle-type placement / import cycle.** `SpawnFunc` lives in `internal/tools`;
  `AgentHandle` lives in `internal/agent`. Returning the handle from a `tools` type may
  reintroduce the very import cycle `SpawnFunc` was created to break (`spawn_agent.go:32-34`).
  Resolve by moving `AgentHandle`/`SpawnDone` to an agent-free package (the ADR-0017 pattern),
  or by defining a small handle interface in `tools` that `agent.AgentHandle` satisfies. Pick
  in planning; the seam's shape (a handle) is fixed regardless.
- **`spawn.done` emission site.** Emit from the `Spawner` (one authoritative site, sees every
  spawn) or from the tool/adapter (closer to the model-facing result assembly)? Lean Spawner —
  it is the single choke point every spawn passes through, and it already owns `SpawnDone`
  construction (`spawn.go:409-411`), so both in-process and future networked backends emit
  uniformly.
- **`spawn.start` pairing.** 0043 defines `KindSpawnStart` too. Confirm the spawn boundary
  emits `spawn.start` at admission (before the child runs) so observers see the pair; this is
  additive and belongs here since this change owns the spawn seam.
- **Pipeline call-site audit.** The pipeline engine sets `SpawnOpts.Expects` and consumes
  results. Confirm every pipeline consumer already uses `handle.Wait()`/`Result()` (so it is
  unaffected) rather than any string-returning helper that this change removes.

## Verification

- **Model-visible unchanged:** a driven `spawn_agent` run produces the **same** model-facing
  result string (prose, budget line, quota line) before and after — the tool still blocks
  internally (D1).
- **Handle contract intact:** pipeline/Go callers awaiting `handle.Wait()`/`Result()` get the
  same `SpawnDone{Result, Structured}` as today; ADR-0016 slot-yield timing is preserved (a
  waiting parent still yields its slot; no deadlock regression).
- **`spawn.done` on the stream:** a spawn emits a `spawn.done` event (and a paired
  `spawn.start`) on the 0043 EventStore, carrying the child's `Result`/`Structured`, visible
  to a `Subscribe()`r that holds no handle.
- **No compat wrapper remains:** grep confirms no `(string, error)` spawn adapter survives —
  the three cmd-site builders and the tool are the only consumers, all migrated.
