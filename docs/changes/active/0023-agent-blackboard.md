---
id: 23
slug: agent-blackboard
title: Shared result blackboard for inter-agent communication
status: proposed
priority: high
type: feat
created: 2026-08-06
updated: 2026-08-06
depends_on: [12]
related: [12, 24]
discovered_from: [12]
adrs: []
spec:
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
<!-- docket:artifacts:end -->

## Why

fuse's subagent model (change 0012) is spawn-and-collect: a parent spawns children via `spawn_agent`, waits for results, and consumes them. There is no shared memory between concurrently running agents — a child cannot see what another child found, a parent cannot push mid-run guidance, and the only coordination mechanism is serial dependency (spawn B after A completes). This limits the patterns fuse can express: ensemble methods (multiple agents exploring different hypotheses and collaborating), debate (agents critiquing each other's intermediate results), and producer/consumer pipelines (one agent writes to a shared space another reads from) are all impossible. A **shared result blackboard** — a thread-safe key-value store visible to all agents in a session — would enable these patterns while keeping the existing spawn/collect architecture intact.

## What changes

- **`internal/agent/blackboard.go`** — new `Blackboard` type: a thread-safe (`sync.RWMutex`) map of `string` keys to structured values (JSON-encodable `any`). Key methods:
  - `Put(key string, value any)` — upsert a value.
  - `Get(key string) (any, bool)` — read a value.
  - `Delete(key string)` — remove a key.
  - `Keys(pattern string) []string` — glob-match keys for discovery.
  - `Wait(ctx context.Context, key string) (any, error)` — block until a key is set (for producer/consumer patterns).
  - `Snapshot() map[string]any` — consistent snapshot for tree display.
- **`blackboard` tool**: a new built-in tool (alongside `spawn_agent`) that exposes the blackboard to the model:
  - `blackboard_write(key, value)` — structured write (value is a JSON string that gets parsed).
  - `blackboard_read(key)` — read a key.
  - `blackboard_wait(key, timeout)` — blocking read with timeout (for coordination).
  - `blackboard_keys(pattern)` — discovery.
  - `blackboard_delete(key)` — removal.
- **Integration with `AgentTree`**: the blackboard is owned by the root `AgentTree` and passed to each `AgentNode` at creation. Children access the root's blackboard transparently (no need to pass handles).
- **Visualization in the agent tree overlay**: a new "Blackboard" tab in the tree detail pane shows current keys/values with agent-wrote indicators.
- **Session scoping**: the blackboard is local to a session (in-memory, no persistence). Lifespan equals the agent tree lifespan (one user turn or one-shot run).

## Out of scope

- Persistence across sessions — no write-back to disk.
- Access control (any agent can write any key) — simplicity first; ACLs are a follow-up.
- Value size limits — bounded implicitly by context budget; no hard cap.

## Research notes (input for the brainstorm)

The blackboard pattern is well-established in multi-agent systems: it originated in the Hearsay-II speech recognition system (1970s) and is now used in frameworks like LangGraph (shared state dict), CrewAI (shared memory), and Google ADK (AgentStore). The key design insight is that the blackboard eliminates the need for direct agent-to-agent messaging — agents communicate by reading and writing structured data to a shared space. The `Wait` operation is the crucial enabler for producer/consumer patterns: agent A spawns agent B (the producer) and agent C (the consumer), C calls `blackboard_wait("analysis_result")`, B processes data and writes to that key, C unblocks and uses the result. This is more robust than passing handles or channels because it works across any number of agents and survives agent restarts (within a session). The glob-keyed `Keys` method lets agents discover what data is available without prior agreement on key names — the research skill's facets could each write to a `facet/<name>` key, and a synthesizer agent could discover and aggregate them.
