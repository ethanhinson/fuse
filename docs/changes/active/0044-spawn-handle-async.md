---
id: 44
slug: spawn-handle-async
title: Spawn handle-async — location-transparent spawning behind a handle-returning contract
status: in-progress
priority: high
type: refactor
created: 2026-08-09
updated: 2026-08-10
depends_on: [43]
related: [23, 24, 34, 36, 42, 43]
discovered_from: [43]
adrs: [16, 17]
spec: docs/superpowers/specs/0044-spawn-handle-async.md
plan:
results:
trivial: false
auto_groomable:
branch: feat/spawn-handle-async
pr:
blocked_by:
claimed_at: 2026-08-10T06:42:26Z
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [0044-spawn-handle-async.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/0044-spawn-handle-async.md) |
| ADRs | [ADR-0016](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0016-subagent-spawn-tree-runtime.md), [ADR-0017](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0017-segment-store-fssink-subpackage-split.md) |
<!-- docket:artifacts:end -->

## Why

For fuse to fan out subagents horizontally — the "scale to another process or machine"
half of the agentic-loop-runtime vision — the spawn seam must be **location-transparent**:
`spawn` returns a handle the caller awaits, and the interface looks identical whether the
child runs in-process or remotely. Today the public seam is the opposite shape:

```go
type SpawnFunc func(ctx, SpawnRequest) (result string, err error)
```

A synchronous, string-returning call that a networked backend structurally cannot honor — a
child in another process can't return its finished result inline on the caller's goroutine.
Left as-is, horizontal scale is a rewrite; fixed, a networked backend is a new
implementation behind an unchanged seam.

The crux is that the async, handle-shaped contract **already exists internally** —
`Spawner.Spawn` already returns an `AgentHandle` with `Done`/`Wait()`/`Result()`, and
`SpawnDone` already splits `Result` from `Structured` (the 0024/0042 return_result work).
Only the public `SpawnFunc` adapter and the three cmd-site builders re-collapse that handle
into a blocking string. So this is "stop hiding the handle that's already there," not
"build async spawn" — the hard part is already de-risked.

This is the **second of three changes** toward the Runtime seam, and it **depends on 0043**:
a completed child's result is surfaced as a `spawn.done` event on 0043's EventStore stream,
wiring the `KindSpawnDone` kind that change defined.

## What changes

Change the public `SpawnFunc` from `(string, error)` to a **handle-returning** contract, and
migrate all three cmd-site child builders (`main.go` one-shot, `shell.go`, `research_probe.go`)
plus the `SpawnAgentTool` wiring in one PR — the deliberate "big-bang" signature change, no
compat wrapper (a lingering string-returning adapter would let the collapsed shape persist and
defeat the whole point). Because `Spawner.Spawn` already returns `AgentHandle`, the migration
is mechanically small: change the type, have the three adapters pass the handle through instead
of pre-awaiting it, have the tool await it, and emit the completion event.

The child's result then flows **two ways**: the handle (`Wait()`/`Result()`) stays the
in-process **control** path (pipelines rely on it; ADR-0016 slot-yield timing preserved), and
a `spawn.done` event is emitted to 0043's stream as the **observation** path (observers see
completion without holding a handle). Handle for control, event for observation — the runtime
philosophy made concrete.

The **model-facing `spawn_agent` tool contract is byte-unchanged**: the tool blocks internally
on `handle.Wait()` and returns the same result string (prose, budget line, quota line) to the
model. The async seam is exposed only to Go callers (the pipeline engine, and change 3's
Runtime), so this structural change shifts no model behavior — it's verifiable as "same
model-visible output, new Go-visible contract."

## Out of scope

- **The `Runtime` interface extraction and binding #2** — that is change 3
  (`runtime-interface-and-binding`). This change makes `spawn` handle-shaped; it does not name
  or expose a `Runtime`.
- **A networked spawn backend** — in-process only. The interface must merely *permit* a
  networked backend later; no remote transport is built here.
- **Model-facing async fan-out** — the model does not gain a spawn-then-poll tool; its
  `spawn_agent` contract is unchanged. Model-level async orchestration, if ever wanted, is a
  separate deliberate change.
- **Removing `handle.Wait()`/`Result()`** — they stay as the in-process control path; the
  event is an additional observation path, not a replacement.

## Open questions

- **Handle-type placement / import cycle.** `SpawnFunc` lives in `internal/tools`;
  `AgentHandle` in `internal/agent`. Returning the handle may reintroduce the import cycle
  `SpawnFunc` was created to break — resolve by moving `AgentHandle`/`SpawnDone` to an
  agent-free package (the ADR-0017 pattern) or defining a small handle interface in `tools`.
  The seam's shape (a handle) is fixed regardless.
- **`spawn.done` emission site** — Spawner (single choke point, already builds `SpawnDone`) vs
  the tool/adapter. Lean Spawner so in-process and future networked backends emit uniformly.
- **`spawn.start` pairing** — confirm the spawn boundary emits 0043's `KindSpawnStart` at
  admission so observers see the start/done pair; additive, belongs with the spawn seam.
- **Pipeline call-site audit** — confirm every pipeline consumer already uses
  `handle.Wait()`/`Result()` (unaffected) rather than a string-returning helper this change
  removes.
