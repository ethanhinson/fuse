---
id: 26
slug: handle-returning-spawn-seam-agent-free-interface
title: Handle-returning spawn seam via an agent-free interface in internal/tools
status: Accepted
date: 2026-08-10
supersedes: []
reverses: []
relates_to: [16, 17]
change: 44
---

## Context

Change 0044 (spawn-handle-async) reshapes the public spawn seam so the tool
layer can return a handle rather than a settled string: `tools.SpawnFunc` moves
from `(string, error)` to the handle-returning `(tools.SpawnHandle, error)`.
The internal `agent.Spawner.Spawn` already returns an `agent.AgentHandle`.

The forces that make this non-obvious are the package boundary and the import
cycle it exists to prevent:

- `tools.SpawnFunc` lives in `internal/tools`; `agent.AgentHandle` and
  `agent.SpawnDone` live in `internal/agent`.
- `internal/agent` imports `internal/tools` (the registry). So returning
  `agent.AgentHandle` directly from a `tools`-package type would reintroduce the
  exact import cycle `SpawnFunc` was originally created to break
  (`tools → agent → tools`).

The spec left this as an open question with two options on the table:

1. **Move `AgentHandle`/`SpawnDone` to a new agent-free package** — the
   ADR-0017 split pattern that change 0043's `internal/event` followed.
2. **Define a small handle interface in `internal/tools`** that
   `agent.AgentHandle` satisfies via a thin adapter.

## Decision

Choose **option 2**: a minimal agent-free `tools.SpawnHandle` interface
(`WaitResult() tools.SpawnResult`) plus a `tools.SpawnResult{Result string; Err
error}` value type, both defined in `internal/tools`. A thin adapter in the
composition root `cmd/fuse` (`cmdSpawnHandle`) wraps `agent.AgentHandle` and
satisfies `tools.SpawnHandle`, performing the scheduler slot yield / wait /
unyield inside `WaitResult()`. `internal/tools` still does not import
`internal/agent`.

Rationale for choosing option 2 over option 1:

- The handle *is* the agent-dependent value itself. Unlike change 0030's
  segment case (ADR-0017), there is no separable "schema + reader" vs "writer"
  split — so the agent-free-subpackage pattern does not map cleanly here.
- Moving `AgentHandle`/`SpawnDone` out of `internal/agent` would ripple through
  `internal/agent/spawn.go`, `internal/pipeline`, and many tests.
- The tool needs only the prose result string plus error from the handle
  (`SpawnResult`) — a tiny behavioral surface. `SpawnDone.Structured` (the
  structured-delegation value) stays agent-side; only Go callers (pipeline,
  future Runtime) consume it, via `agent.AgentHandle` directly.

## Consequences

Enables:

- The location-transparent handle-returning seam (D1), with the model-facing
  `spawn_agent` contract byte-unchanged — the tool awaits `WaitResult()`
  internally.
- A future networked spawn backend becomes a new implementation behind the same
  `tools.SpawnHandle` interface.

Costs / gives up:

- A small adapter type in `cmd/fuse` per await site.
- The interface is behavioral (not the concrete handle), so a caller wanting
  the structured value must reach for `agent.AgentHandle` directly rather than
  through `tools.SpawnHandle`.

Relates to ADR-0016 (subagent spawn tree runtime — the slot-yield timing is
preserved inside `WaitResult()`) and ADR-0017 (the segment-store/fssink
subpackage split — the pattern considered and deliberately **not** chosen here).
