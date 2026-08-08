---
id: 23
slug: agent-blackboard
title: Shared result blackboard for inter-agent communication
status: done
priority: high
type: feat
created: 2026-08-06
updated: 2026-08-08
depends_on: [12]
related: [12, 24]
discovered_from: [12]
adrs: []
spec: docs/superpowers/specs/2026-08-08-agent-blackboard-design.md
plan: docs/superpowers/plans/0023-agent-blackboard.md
results: docs/results/2026-08-08-agent-blackboard-results.md
trivial: false
auto_groomable:
branch: feat/agent-blackboard
claimed_at: 
pr: https://github.com/ethanhinson/fuse/pull/25
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-08-agent-blackboard-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-08-agent-blackboard-design.md) |
| Plan | [0023-agent-blackboard.md](https://github.com/ethanhinson/fuse/blob/feat/agent-blackboard/docs/superpowers/plans/0023-agent-blackboard.md) |
| Results | [2026-08-08-agent-blackboard-results.md](https://github.com/ethanhinson/fuse/blob/feat/agent-blackboard/docs/results/2026-08-08-agent-blackboard-results.md) |
| PR | [#25](https://github.com/ethanhinson/fuse/pull/25) |
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
- **Directed-message convention (folded in from killed change 0025)** — a thin, poll-based
  layer over the blackboard for the debate/refine pattern: a sender writes to a per-agent inbox
  key namespace (`inbox/<target>/<seq>`) and the target polls its own inbox by glob
  (`blackboard_keys("inbox/<self>/*")` + read). This is a **convention plus a thin helper**, not
  a new inbox data structure — it reuses the blackboard's store, provenance, and tree view.
  Crucially it fits fuse's spawn-and-collect model: an agent polls its inbox **during its own
  turns** (no mid-run injection, no agent-loop change). No blocking, no delivery guarantees.

## Out of scope

- Persistence across sessions — no write-back to disk.
- Access control (any agent can write any key) — simplicity first; ACLs are a follow-up.
- Value size limits — bounded implicitly by context budget; no hard cap.
- Smarter wait liveness (tree-idle / producer-death wake) — timeout-only for v1; a follow-up.
- **Real mid-run message injection / live inter-agent debate** — the directed-message
  convention is poll-based (agents check their inbox during their own turns); injecting a
  message into a *running* child mid-turn is a genuine agent-loop change, deferred.
- **ACP (Agent Client Protocol) external-harness interop** — a separate initiative (two changes:
  fuse-as-ACP-agent and fuse-as-ACP-client), not this change.

## Design decisions

Design settled through an interactive brainstorm on 2026-08-08 and captured in the linked
spec. Four decisions fixed the shape: (1) `blackboard_wait` yields its scheduler slot and
requires a timeout; (2) the full Blackboard tree tab is in scope; (3) wait liveness is
timeout-only for v1; (4) each entry records its writing agent. **Change 0025
(agent-to-agent messaging) was killed and folded in here** (2026-08-08): a directed-message
inbox is functionally a per-agent blackboard key, so it becomes a thin poll-based convention
over this store rather than a separate primitive. Downstream change **0026** (workflow
composition) builds on this substrate. Scope is deliberately kept tight.

## Reconcile log

### 2026-08-08 — reconcile before build (docket-implement-next)

Re-read the change + spec against current `main` code, `depends_on`/`related`, and the
learnings ledger. Verdict: **build-ready, no scope change; one spec correction recorded
below.** No fundamental invalidation.

**Verified against current code (all spec anchors accurate):**
- `AgentTree` ownership hooks — `NewAgentTree` / `NewAgentTreeWithConcurrency`
  (`internal/agent/tree.go:248`/`256`), one tree per session.
- Slot-yield API — `AgentTree.YieldSlot(node)` (`tree.go:416`) and
  `UnyieldSlot(ctx, node) bool` (`tree.go:427`), delegating to the `Scheduler`
  (`scheduler.go:881`/`899`). Signatures match the spec's `Wait` contract exactly.
- `AgentNode.ID` / `AgentNode.Label` (`tree.go:91`/`93`) available for provenance.
- Tool seam — `tools.Tool` interface + `tools.Result` (`internal/tools/registry.go`);
  `spawn_agent.go` is the modeling reference for `NewXTool` / `Name` / `Description` /
  `Parameters` / `Execute`. Confirmed the blackboard tool must carry per-node provenance,
  so — exactly like `spawn_agent` — it is **re-registered per child** bound to `childNode`,
  not merely inherited through `childToolRegistry`'s Clone/Subset of the parent registry.
- Race-safe display pattern — `NodeView` / `AgentNode.Snapshot()` (`tree.go:182`) is the
  model for the required `Blackboard.Snapshot()`.
- `depends_on: [12]` is satisfied — change 0012 (subagent-ux) is `done`
  (`archive/2026-08-05-0012-subagent-ux.md`). `related: [24]` (structured-delegation) is
  still `proposed` and independent — no coupling. Killed #0025 fold-in confirmed.

**Spec correction (wiring sites) — the one drift.** The spec's `## Tool wiring` names the
child-builder sites as `cmd/fuse/run.go`, `shell.go`, `research_probe.go` and asks to
"re-grep for a fourth (`workflow.go`)". Grepping the current tree for `WithChildBuilder`
(and confirmed by learning `patch-every-cloned-child-builder`) the actual agent entry
points with a root registration **and** a child-builder closure are **`cmd/fuse/main.go`
(the one-shot `run()` path), `cmd/fuse/shell.go`, and `cmd/fuse/research_probe.go`**.
`run.go` holds only the shared registry helpers (`defaultToolRegistry`,
`buildSessionRegistryNoMCP`, `childToolRegistry`) — no builder closure of its own;
`workflow.go` holds only budget/quota helpers (`budgetFor`, `quotaWarningFor`) — no child
builder. The spec's own instruction ("enumerate the sites by grep at build time, never
from this list") governs and is honored: the plan will re-grep and wire all three
entry-point sites (root + child) plus the shared helper path. No behavioral scope change —
just the corrected site enumeration.

**Learnings pulled for the plan:** `slot-cap-yield-while-blocked-on-children` (D1's
mandatory yield + the saturation regression), `patch-every-cloned-child-builder` (grep the
sites), `verify-tool-loop-at-gateway-seam` (model-sees-tool via a scripted
`LLM_GATEWAY_URL` double), `teatest-final-frame-via-finalmodel-view` +
`sanitize-untrusted-bytes-fixed-width-tui` (Blackboard tab render + untrusted-value
sanitization).
