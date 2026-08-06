<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0033 — Strip spawn_agent from tool schemas at the concurrency cap and on budget exhaustion](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/archive/2026-08-06-0033-spawn-tool-stripping.md)**
<!-- docket:backlink:end -->

# 0033 — Spawn tool stripping: reversible at the concurrency cap, permanent at budget exhaustion

## Problem

When fuse's spawn limits refuse a `spawn_agent` call, the refusal happens at the worst
possible point: *after* the model has committed to a delegation plan and written the task
prompts. Observed live (deepseek-flash research run, 2026-08-05): two depth-2 agents fired
22 `spawn_agent` calls after the 16-spawn budget was exhausted. Every call was approved by
the permission gate, then refused by the budget backstop (`ErrSpawnBudgetExhausted`,
`internal/agent/spawn.go:94`). The parent agents paid three times — the delegation prompts
became dead context, 22 near-identical error results landed on top, and the file-reading
work then happened inline in the parents' own context windows, exactly what delegation was
meant to avoid. The permission log showed 22 "allowed" spawns that never existed, which
also made the agent tree look incomplete.

Survey of peer harnesses (research session, 2026-08-05): Claude Code withholds the Agent
tool from subagents at its depth limit; Grok Build strips the spawn tool from a child's
toolset at max depth ("Stripped task tool from child at max depth"). Removing the tool
from the schema — so the model never plans delegation it can't have — is the consensus
mechanism. Codex bounds concurrency with residency slots that free on exit but has no
lifetime budget; Grok Build's workflow subsystem pairs 16 concurrent with a 128 spawn
budget; Claude Code's Workflow pairs min(16, cores−2) concurrent with a 1000 lifetime
backstop. The two-brake shape (reversible width brake + permanent lifetime brake) is the
strongest combination, and fuse already has both brakes — they just push back with errors
instead of schema changes, and the width brake (a silent unbounded queue) pushes back not
at all.

## Design

Two brakes, two strip behaviors, one mechanism. `spawn_agent` is omitted from the tool
schemas an agent sees when either brake is engaged; the schema list is rebuilt every turn
(`internal/agent/loop.go:156` calls `a.tools.Schemas()` fresh per inference request), so
stripping and restoration are both just conditional schema assembly.

### Brake 1 — active-agent cap (reversible)

- `MaxConcurrentSpawns` is raised **8 → 16** and becomes configurable
  (`agents.max_concurrent`, default 16). It keeps its existing meaning: the semaphore
  bound on concurrently *running* children (`internal/agent/tree.go`), with the
  yield/unyield mechanics unchanged.
- New behavior: while the tree's active child count — running + pending, i.e.
  `AgentTree.ActiveCounts()` (`tree.go:322`) — is at or above the cap, every agent's next
  turn omits `spawn_agent` from its schemas. As children finish and the count drops below
  the cap, the tool reappears. This is the "tool comes back as subagents exit" behavior.
- Rationale: today the pending queue is unbounded and silent — a model can commit 22
  spawns and learn nothing. Stripping between turns paces the model to slot availability
  without refusing anything. A single turn can still batch more spawns than free slots
  (the schema was present when the turn started); those queue exactly as today. Accepted:
  the queue is the race backstop, not the primary interface.

### Brake 2 — lifetime spawn budget (permanent)

- `agents.max_spawns` default is raised **16 → 64**. Semantics unchanged: total children
  ever created, append-only (`AgentTree.SpawnBudget()`, `tree.go:223`), never freed on
  exit. Reference points: Grok Build workflows default 128; Claude Code Workflow backstop
  1000; fuse's observed legitimate run used 16. 64 is high enough that a real wide run
  never feels it and low enough to stop a runaway model cheaply.
- When `used >= max` (and `max > 0`), `spawn_agent` is stripped from every agent's
  subsequent turns, permanently for the session. `max_spawns: 0` (unset) means no budget
  and no permanent strip, as today.

### Depth stripping (static)

- A child created at `Depth == MaxDepth` never receives `spawn_agent` at all: the tool is
  excluded from its registry at construction time (the wiring in `cmd/fuse` that builds a
  child's tool registry), not stripped per turn. Mirrors Grok Build / Claude Code.

### What stays

- The call-time errors (`ErrMaxDepthExceeded`, `ErrSpawnBudgetExhausted`) remain as
  backstops for two races: an in-flight turn that already saw the schema, and a model
  hallucinating a tool not in its schema. Their messages already redirect the model
  ("proceed with the results you already have and do not spawn again") — unchanged.
- The budget-line injection on successful spawn results
  (`internal/tools/spawn_agent.go:121`) remains the early-warning layer.
- No injected notice when the tool disappears (silent strip, Claude Code style — decided
  at brainstorm). The last budget line plus the backstop error messages carry enough
  signal.
- Pending-queue semantics, `YieldSlot`/`UnyieldSlot`, cancellation, and the permission
  gate are untouched. The gate benefit is automatic: a model that never sees the tool
  never generates doomed approval entries.

### Mechanism sketch

The agent loop needs a per-turn predicate deciding whether `spawn_agent` is visible.
Suggested shape: the agent (or its registry wrapper) holds an optional
`stripSpawn func() bool` closure injected at construction, consulting the shared
`*AgentTree` (`SpawnBudget()` + `ActiveCounts()` — both cheap snapshots). `Schemas()`
filtering happens in the loop or via a registry decorator — implementer's choice; the
registry already supports `Subset()` (`internal/tools/registry.go:102`). The predicate
must be race-safe (tree methods already lock) and must not cache across turns.

Config plumbing: `agents.max_concurrent` joins `agents.max_spawns` in
`internal/config/schema.go` / `loader.go`; `MaxConcurrentSpawns` becomes the default for
the new knob rather than a hard const; the semaphore size is set at tree construction
(`NewAgentTree`).

## Acceptance

1. With active children ≥ cap, the next inference request for every live agent (root
   included) carries no `spawn_agent` schema; when the count drops below cap, the schema
   returns on the following turn.
2. With the lifetime budget exhausted, no subsequent request in the session carries the
   schema, regardless of active count.
3. A child at `MaxDepth` never has `spawn_agent` in its registry.
4. A hallucinated `spawn_agent` call while stripped still gets the existing budget/depth
   error result (backstop intact).
5. Defaults: `max_concurrent` 16, `max_spawns` 64; both overridable in config; existing
   configs with explicit `max_spawns` keep their value.
6. The deepseek-flash scenario replayed (per-file fan-out at depth 2 with budget spent)
   produces zero refused spawn attempts and zero dead permission-log entries after the
   strip engages.

## Out of scope

- Refusing (instead of queueing) spawns when the batch overflows free slots — the queue
  stays.
- Ghost/"refused" nodes in the agent tree UI, or permission-gate short-circuiting.
- Any change to remote/subagent result plumbing, `spill`, or context pruning.
- Making `MaxDepth` configurable.
