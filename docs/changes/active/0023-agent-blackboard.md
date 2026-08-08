---
id: 23
slug: agent-blackboard
title: Shared result blackboard for inter-agent communication
status: proposed
priority: high
type: feat
created: 2026-08-06
updated: 2026-08-08
depends_on: [12]
related: [12, 24]
discovered_from: [12]
adrs: []
spec: docs/superpowers/specs/2026-08-08-agent-blackboard-design.md
plan:
results:
trivial: false
auto_groomable:
branch:
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-08-agent-blackboard-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-08-agent-blackboard-design.md) |
<!-- docket:artifacts:end -->

## Why

fuse's subagent model (change 0012) is spawn-and-collect: a parent spawns children via `spawn_agent`, waits for results, and consumes them. There is no shared memory between concurrently running agents — a child cannot see what another child found, a parent cannot push mid-run guidance, and the only coordination mechanism is serial dependency (spawn B after A completes). This limits the patterns fuse can express: ensemble methods (multiple agents exploring different hypotheses and collaborating), debate (agents critiquing each other's intermediate results), and producer/consumer pipelines (one agent writes to a shared space another reads from) are all impossible. A **shared result blackboard** — a thread-safe key-value store visible to all agents in a session — would enable these patterns while keeping the existing spawn/collect architecture intact.

## What changes

- **`Blackboard` type** (`internal/agent/blackboard.go`) — a thread-safe, session-scoped
  key→value store owned by the root `AgentTree` and shared by every agent in the session.
  Values are JSON-encodable structured data; each entry records **which agent wrote it** (for
  discovery and the tree view). Methods: `Put` / `Get` / `Delete` / `Keys` (glob discovery) /
  `Wait` (blocking coordination) / `Snapshot` (race-safe read for display).
- **`blackboard` built-in tool** — exposes the store to the model as five operations:
  `blackboard_write` / `blackboard_read` / `blackboard_wait` / `blackboard_keys` /
  `blackboard_delete`. Written values are JSON strings the tool parses; malformed input is a
  tool error, never a crash.
- **`blackboard_wait` yields its scheduler slot and requires a timeout.** A blocked waiter
  releases its concurrency slot (reusing `AgentTree.YieldSlot`/`UnyieldSlot`) so the
  producer/consumer pattern cannot deadlock the pool — the failure mode that bit change 0012.
  Every wait is bounded by a required timeout; there are no infinite waits.
- **AgentTree ownership** — one blackboard per session, lifespan equal to the agent tree
  (one turn / one-shot run). All descendants share the root blackboard, so a nested spawn sees
  the same keys.
- **Tool wiring in every agent builder** — root and all cloned child builders in `cmd/fuse`.
  Unlike `spawn_agent`, the blackboard tool is always wired (shared-state access is not a
  spawn capability), but an explicit `tools`-subset exclusion is still honored.
- **Blackboard tab** in the agent-tree overlay — current keys/values with agent-wrote
  indicators.

## Out of scope

- Persistence across sessions — no write-back to disk.
- Access control (any agent can write any key) — simplicity first; ACLs are a follow-up.
- Value size limits — bounded implicitly by context budget; no hard cap.
- Smarter wait liveness (tree-idle / producer-death wake) — timeout-only for v1; a follow-up.
- Direct agent-to-agent messaging — that is change 0025, which builds on this.

## Design decisions

Design settled through an interactive brainstorm on 2026-08-08 and captured in the linked
spec. Four decisions fixed the shape: (1) `blackboard_wait` yields its scheduler slot and
requires a timeout; (2) the full Blackboard tree tab is in scope; (3) wait liveness is
timeout-only for v1; (4) each entry records its writing agent. Downstream changes **0025**
(agent-to-agent messaging) and **0026** (workflow composition) build on this substrate, so the
scope is deliberately kept tight.
