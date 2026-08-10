---
id: 44
slug: spawn-handle-async
title: Spawn handle-async — location-transparent spawning behind a handle-returning contract
status: done
priority: high
type: refactor
created: 2026-08-09
updated: 2026-08-10
depends_on: [43]
related: [23, 24, 34, 36, 42, 43]
discovered_from: [43]
adrs: [16, 17, 26]
spec: docs/superpowers/specs/0044-spawn-handle-async.md
plan: docs/superpowers/plans/0044-spawn-handle-async-plan.md
results: docs/results/2026-08-10-spawn-handle-async-results.md
trivial: false
auto_groomable:
branch: feat/spawn-handle-async
pr: https://github.com/ethanhinson/fuse/pull/47
blocked_by:
claimed_at: 
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [0044-spawn-handle-async.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/0044-spawn-handle-async.md) |
| Plan | [0044-spawn-handle-async-plan.md](https://github.com/ethanhinson/fuse/blob/feat/spawn-handle-async/docs/superpowers/plans/0044-spawn-handle-async-plan.md) |
| Results | [2026-08-10-spawn-handle-async-results.md](https://github.com/ethanhinson/fuse/blob/feat/spawn-handle-async/docs/results/2026-08-10-spawn-handle-async-results.md) |
| PR | [#47](https://github.com/ethanhinson/fuse/pull/47) |
| ADRs | [ADR-0016](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0016-subagent-spawn-tree-runtime.md), [ADR-0017](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0017-segment-store-fssink-subpackage-split.md), [ADR-0026](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0026-handle-returning-spawn-seam-agent-free-interface.md) |
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

## Reconcile log

### 2026-08-10 — reconciled against origin/main @ 6720354 (post-0043 merge)

Verified the change against the current code; the spec is sound and stays authoritative.
Findings that sharpen (but do not invalidate) the plan:

1. **Dependency 0043 is `done` (PR #46 merged, merge 6720354).** origin/main now carries
   `internal/event` (envelope, `KindSpawnStart`/`KindSpawnDone`, `SpawnStartPayload`/
   `SpawnDonePayload`, `EventStore`). Build-ready confirmed.

2. **Single shared `SpawnFunc` adapter, not three.** Change 0026 already consolidated the three
   cmd-site adapters into one `spawnFuncFrom` (`cmd/fuse/run.go:671`), used by main.go (one-shot),
   shell.go, and research_probe.go. It is the sole place that pre-collapses the handle
   (`Spawn` → `YieldSlot` → `handle.Wait()` → `UnyieldSlot` → `(done.Result, done.Err)`). The
   migration therefore edits **one** adapter + the `SpawnFunc` type + the tool, not three adapters.
   The three cmd sites still each build their own child builder (`emitSpawn*` clone sites remain
   three) — the D3 "big-bang, three sites" intent holds, but the adapter surface is singular.

3. **Import cycle is real.** `internal/tools` does NOT import `internal/agent` (ask_user.go /
   blackboard.go only reference it in comments explaining the deliberate non-import). Returning
   `agent.AgentHandle` from `tools.SpawnFunc` reintroduces the cycle `SpawnFunc` was created to
   break. Resolution chosen for planning: **define a minimal handle interface in `internal/tools`**
   that `*agent.AgentHandle` satisfies (the less-invasive of the spec's two options; moving
   `AgentHandle`/`SpawnDone` out of `internal/agent` would ripple through spawn.go + pipeline +
   many tests). `SpawnDone.Structured` is NOT needed by the tool (only Go callers use it), so the
   tools-side interface can stay tiny. An ADR will record this.

4. **Pipeline is unaffected — confirmed.** `internal/pipeline/engine.go` and `synthesize.go` call
   `sp.Spawn(...)` and consume via `h.Result()` / `h.Wait()` directly on `agent.AgentHandle`; they
   never touch `tools.SpawnFunc`. The signature change does not reach them.

5. **`spawn.start`/`spawn.done` emission ALREADY EXISTS at the cmd-site child builders (0043).**
   `cmd/fuse/event_spawn.go` defines `emitSpawnStart`/`emitSpawnDone`, called inside all three child
   builders (main.go:226/228, shell.go:358/366, research_probe.go:229/231), plus a projected
   log-consumer proven byte-identical to the direct session log. The spec's D2 open question
   ("emit from Spawner vs adapter") is thus really "**relocate 0043's cmd-site emission into the
   Spawner** (single choke point) vs leave it where 0043 put it." Per the spec's lean and the
   change brief, the plan will move emission into the `Spawner` (`spawnLocal`): emit `spawn.start`
   at admission and `spawn.done` from the same completion that builds `SpawnDone{Result,Structured}`.
   The `Spawner` gains an `EventStore` via a `WithEventStore` option — no new import cycle, since
   `internal/agent` already imports `internal/event`. **Byte-identity guard:** 0043's projected
   session log uses only event metadata (NodeID/ParentID/Label/Depth/Kind), so relocation keeps the
   projection byte-identical; but note `SpawnDone.Result` (from `childResult`, which prepends a
   "[stopped: …]" marker on max-turns/loop) differs from 0043's `lastAssistantText(msgs)` on those
   edge cases — the plan must decide and record how the relocated `spawn.done` `Result` is sourced
   so no observation regresses. This relocation, and removing the now-redundant cmd-site emitters,
   is the one place the plan must proceed carefully.

Scope unchanged. Runtime interface extraction (#3) and any networked backend remain out of scope.

Follow-ups surfaced (auto-capture disabled → reported here, not minted): 0043 left a trivial
follow-up to delete the direct `sessLog.Write` once the projected log is proven equivalent; this
change may make that deletion cleaner but does not own it — leave as a separate change.
