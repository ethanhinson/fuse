---
id: 20
slug: mcp-progress-streaming
title: MCP `$/progress` notifications and streaming tool results
status: proposed
priority: medium
type: feat
created: 2026-08-06
updated: 2026-08-08
depends_on: [19]
related: [18, 21]
discovered_from: [19]
adrs: []
spec: docs/superpowers/specs/2026-08-08-mcp-progress-streaming-design.md
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
| Spec | [2026-08-08-mcp-progress-streaming-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-08-mcp-progress-streaming-design.md) |
<!-- docket:artifacts:end -->

## Why

Long-running MCP tool calls (code execution, multi-step retrievals, build commands) are currently opaque — the caller blocks until the tool completes, with no intermediate progress. The MCP 2025-03-26 spec formalizes the `$/progress` notification contract (was previously a convention) and introduces `$/stream` for continuous streaming of partial results. Implementing these would let fuse show real-time progress for long-running MCP tools in the TUI (progress bars in the agent tree, streaming partial results into the transcript), and would enable the MCP server side to stream incremental results back to clients like Claude Code and Cursor.

## What changes

- **Inbound notification route in both read pumps** (the foundational fix): today
  `StdioClient.readPump` and `httpClient.readSSEPump` are pure id-keyed response demultiplexers
  and **silently drop every id-less server notification**. Both grow a "no id → notification
  router" branch. Nothing downstream can receive progress until the pump does this first.
- **Feature-generic notification router** on the manager, keyed by notification `method`, so
  `$/progress`, `$/stream`, and later change 0021's `notifications/resources/updated` share one
  seam.
- **`$/progress` receive → agent tree**: the client mints a `progressToken` per call and injects
  it into the `tools/call` `_meta`; matched notifications emit a new `ProgressEvent` on the
  `AgentNode`, rendered as a progress bar on the running tool node.
- **`$/stream` hybrid buffering**: chunks feed a bounded ring buffer for a live rolling TUI line,
  but the agent **loop receives the concatenated complete result** — the loop's contract is
  unchanged and spill/prune apply to the final value.
- **fuse MCP server emits `$/progress`**: `Server.handleCall` threads a progress callback into
  tool execution that writes id-less `$/progress` frames back over the transport when the call
  supplied a token.
- **Capability-gated, fail-open**: send a token / expect streaming only when
  `Supports("streaming")` (from change 0019) is true; a non-advertising server gets today's
  blocking behavior, never an error (ADR-0010 posture).

## Out of scope

- `$/stream` on the client side for fuse's own tool results (web_search, bash) — only MCP-tool results stream.
- Streaming partial child agent prose into the parent (agent-runtime concern, not MCP).
- Progressive partial results **into the agent loop** — the loop receives the complete
  concatenated result; progressive delivery is deferred (tool-result-contract blast radius).

## Design decisions

Design settled through an interactive brainstorm on 2026-08-08 and captured in the linked spec.
Four decisions fixed the shape: (1) **both** `$/progress` and `$/stream`, on **both** the client
(receive) and fuse's MCP server (emit) sides; (2) **capability-gated, fail-open** on
`Supports("streaming")` — a non-advertising server degrades silently to blocking; (3) **hybrid
buffering** — live stream to the TUI, complete concatenated result to the agent loop; (4) a
**feature-generic notification router** that change 0021 reuses. The load-bearing task is the
read-pump notification route: fuse's pumps drop id-less frames today (learning
`mcp-read-pumps-drop-inbound-notifications`), so this must be verified against the real
`fuse mcps list --live` seam, not the in-package fake. Dependency **0019** (capability
negotiation) is `done` and merged, supplying the `notify` path and `Supports(key)` accessor.
