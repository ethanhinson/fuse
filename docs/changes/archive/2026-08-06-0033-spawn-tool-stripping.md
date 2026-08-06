---
id: 33
slug: spawn-tool-stripping
title: Strip spawn_agent from tool schemas at the concurrency cap and on budget exhaustion
status: done
priority: high
type: feat
created: 2026-08-06
updated: 2026-08-06
depends_on: []
related: [24, 26]
discovered_from: []
adrs: []
spec: docs/superpowers/specs/0033-spawn-tool-stripping.md
plan: docs/superpowers/plans/0033-spawn-tool-stripping-plan.md
results: docs/results/2026-08-06-spawn-tool-stripping-results.md
trivial: false
auto_groomable:
branch: feat/spawn-tool-stripping
pr: https://github.com/ethanhinson/fuse/pull/19
claimed_at: 
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [0033-spawn-tool-stripping.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/0033-spawn-tool-stripping.md) |
| Plan | [0033-spawn-tool-stripping-plan.md](https://github.com/ethanhinson/fuse/blob/main/docs/superpowers/plans/0033-spawn-tool-stripping-plan.md) |
| Results | [2026-08-06-spawn-tool-stripping-results.md](https://github.com/ethanhinson/fuse/blob/main/docs/results/2026-08-06-spawn-tool-stripping-results.md) |
| PR | [#19](https://github.com/ethanhinson/fuse/pull/19) |
<!-- docket:artifacts:end -->

## Why

When spawn limits refuse a `spawn_agent` call, the model has already paid for the
delegation: the task prompts are written, the permission gate has approved the calls, and
the refusal error lands after the fact. Observed live in a deepseek-flash research run: 22
spawns approved by the gate then killed by the exhausted 16-spawn budget, leaving the
parent agents to do 22 children's worth of file reading inline in their own context
windows — the worst of both worlds for context isolation, plus a confusing permission log
full of "allowed" spawns that never existed. Every peer harness that bounds spawning
(Claude Code, Grok Build) removes the spawn tool from the model's schema at the limit
instead of erroring at call time, so the model plans inline work from the start.

## What changes

Strip `spawn_agent` from per-turn tool schemas when a brake is engaged, restore it when
the brake releases:

- **Active cap (reversible):** raise `MaxConcurrentSpawns` 8 → 16, make it configurable
  (`agents.max_concurrent`); while running+pending children ≥ cap, the tool vanishes from
  every agent's next turn and returns as children exit.
- **Lifetime budget (permanent):** raise default `agents.max_spawns` 16 → 64; at
  exhaustion the tool is stripped for the rest of the session.
- **Depth (static):** a child at `MaxDepth` never gets the tool in its registry.
- Call-time errors, budget-line injection, queue/yield semantics all stay as backstops.

Design detail and acceptance criteria in the linked spec.

## Out of scope

- Refusing instead of queueing over-batched spawns; the pending queue stays.
- Tree-UI ghost nodes for refused spawns; permission-gate short-circuiting.
- Making `MaxDepth` configurable.

## Reconcile log

### 2026-08-06

Re-read the change body + spec against current `internal/agent`, `internal/tools`,
`internal/config`, and `cmd/fuse` code, the cited related changes (24 structured-delegation,
26 agent-workflow-composition — both still `proposed`/unbuilt, no overlap), and recent ADRs.
Verdict: the design is **accurate and build-ready as written**; no scope change. Verified
anchors against HEAD:

- `internal/agent/spawn.go`: `ErrMaxDepthExceeded` defined at :12 (checked at :87);
  `ErrSpawnBudgetExhausted` defined at :18 (checked at :94). The spec's `spawn.go:94`
  reference is the *check* site and is accurate.
- `internal/agent/loop.go:159` rebuilds tool schemas via `a.tools.Schemas()` per inference
  request (spec cites :156 — off by a few lines, non-blocking).
- `internal/agent/tree.go`: `SpawnBudget()` at :223, `ActiveCounts()` at :339 (spec cites
  :322 — off, non-blocking), `MaxDepth = 5` (:13), `MaxConcurrentSpawns = 8` (:19).
- `internal/tools/registry.go`: `Subset()` at :102, `Schemas()` at :61.
- `internal/tools/spawn_agent.go`: budget line appended at :115 via `budgetLine()`.
- `internal/config/schema.go`: `AgentsConfig` at :115-117, `MaxSpawns` default 16; no
  `agents.max_concurrent` yet (this change adds it).
- `cmd/fuse/shell.go`: registers `spawn_agent` for children at :164 (root at :237), with
  `childNode.Depth` available at construction — the hook for static depth stripping.

Minor line-number drift in the spec is left as-is (informational; anchors resolve by
symbol). No follow-up work surfaced (auto-capture disabled anyway). Proceeding to plan.
