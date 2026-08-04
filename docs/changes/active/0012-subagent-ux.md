---
id: 12
slug: subagent-ux
title: First-Class Subagent UX — Spawn, Tree Visualization & Inspect
status: proposed
priority: high
type: feat
created: 2026-08-04
updated: 2026-08-04
depends_on: [10]
related: [10, 11]
discovered_from: []
adrs: []
spec: docs/superpowers/specs/2026-08-04-subagent-ux-design.md
plan:
results:
trivial: false
auto_groomable: false
branch:
claimed_at:
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-04-subagent-ux-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-04-subagent-ux-design.md) |
<!-- docket:artifacts:end -->

## Why

Every serious AI agent harness eventually hits the same wall: a single-agent loop can't parallelize work, can't isolate risky operations, and can't scale to large codebases. The leaders (Claude Code, LangGraph, OpenHands) all have subagent primitives, but their UX is uniformly poor — a flat spinner, no inspection, no observability. Claude Code's own users have 20+ open issues for subagent visibility, all closed as not planned. This change makes subagents a first-class UX primitive in fuse: spawnable by the model or by skills, observable live in a spatial tree, inspectable per-node, and dispatchable to remote containerized runtimes with structured git-based write-back.

Change 0011 (deep-research) depends on this change's `agent.Spawn`/`SpawnGroup` API for its query fan-out; the subagent architecture unblocks that work.

## What changes

- `spawn_agent` built-in tool — LLM-callable; spawns a child agent and returns its result.
- `agent.Spawn(ctx, SpawnOpts) (*AgentHandle, error)` — programmatic API for skill/code-driven orchestration; `SpawnGroup`/`Join` for fan-out.
- `AgentTree` + `AgentNode` data model — ULID-keyed, thread-safe, append-only events, JSONL session log.
- Spatial ASCII tree TUI (`/agents` + `Tab`) — 3-zone layout with box-drawing edges, `☁` prefix for remote nodes, `●◐○✓✕` status glyphs, 40/60 tree/detail split, drill-down event transcript.
- Inline depth-1 summaries in the main conversation transcript (running / done / error states).
- Remote execution over SSE (`RemoteExecutor` / `SSERemoteExecutor`) — dispatch to a containerized runtime, stream `AgentEvent`s back live, reconnect via `Last-Event-ID`.
- `IntentPlugin` system — pluggable interface resolving container image, git clone spec, write-back branch, and injected files before dispatch; `DocketIntentPlugin` and `OpenSpecIntentPlugin` as reference implementations.
- Secrets management — `SecretsStore` (where secrets live) + `EncryptionProvider` (how they are encrypted); `EnvSecretsStore` (default), `SopsSecretsStore` (SOPS-encrypted file, age/KMS/PGP via SOPS's own backend); optional age-encrypted bundle transport for container secret injection.
- `MaxDepth = 5` hard limit, enforced synchronously before node creation.
- Permission snapshot-clone for local children; remote agents own their own permission gate.
- Tool scoping via `Registry.Subset` — `spawn_agent` force-included; unknowns dropped with a node event.

## Out of scope

- Streaming partial child prose into the parent transcript — the parent sees only the final tool result.
- Cross-session resume of a remote job after the local process exits.
- Compact-depth toggle in the explore view (truncation row format is spec'd for forward-compat; toggle is deferred).
- `git_token` migration to `SecretsStore` in `IntentPluginConfig` — flagged as a follow-on cleanup (assumption 37 in the spec).
- Cost accuracy beyond a static per-model pricing table.

## Open questions

None — design fully specified in the linked spec.
