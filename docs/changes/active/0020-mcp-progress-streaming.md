---
id: 20
slug: mcp-progress-streaming
title: MCP `$/progress` notifications and streaming tool results
status: in-progress
priority: medium
type: feat
created: 2026-08-06
updated: 2026-08-08
depends_on: [19]
related: [18, 21]
discovered_from: [19]
adrs: []
spec: docs/superpowers/specs/2026-08-08-mcp-progress-streaming-design.md
plan: docs/superpowers/plans/2026-08-08-mcp-progress-streaming-plan.md
results:
trivial: false
auto_groomable:
branch: feat/mcp-progress-streaming
pr:
claimed_at: 2026-08-08T09:28:00Z
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-08-mcp-progress-streaming-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-08-mcp-progress-streaming-design.md) |
| Plan | [2026-08-08-mcp-progress-streaming-plan.md](https://github.com/ethanhinson/fuse/blob/feat/mcp-progress-streaming/docs/superpowers/plans/2026-08-08-mcp-progress-streaming-plan.md) |
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

## Reconcile log

### 2026-08-08 — reconcile before build (docket-implement-next)

Re-read the change body + spec against current `origin/main` (tip `106b792`), the linked spec,
related #18/#21, recently-archived #19, and the live `internal/mcp` code. Findings:

- **Dependency #19 is `done`/archived** (`2026-08-07-0019-mcp-capability-negotiation.md`); all of
  its prerequisite seams the spec builds on are present on `origin/main`: `ServerCapabilities.Supports(key)`
  (`internal/mcp/capabilities.go:29`), the id-less `mcpConn.notify` send path (`internal/mcp/conn.go:14`),
  the `jsonrpcNotification { Method; Params }` frame (`internal/mcp/client.go:41`), and per-server
  `caps` tracking on the manager (`internal/mcp/manager.go:56`). Design is **not** invalidated —
  build-ready as specified.
- **Line-number drift only** in the spec's cited seams (the pump moved to `readPump` at
  `client.go:160` and the SSE id-less drop to `http_client.go:273-274`); the *structure* the spec
  describes is intact. No spec edit needed — the seams are named, not just line-pinned.
- **Scope nuance to fold into the plan (not a design change):** `tools.Tool.Execute(ctx, args) Result`
  (`internal/tools/registry.go`) carries **no `AgentNode` reference and no progress channel**, and
  `MCPTool` holds only an `mcpConn` (`internal/mcp/tool.go:20`) — so the spec's "emit a `ProgressEvent`
  on the `AgentNode`" and "manager records `token → (AgentNode, ring buffer)`" cannot be wired
  through the tool's return value. The progress→node association must be **manager-mediated**: the
  manager owns the `token → call-tracking` map and exposes a subscribe/event seam the TUI/agent-tree
  observes (mirroring how `internal/tui/mcp_provider.go` already reaches the manager), rather than
  threading a node handle through `Execute`. The plan will make the pump-route + notification-router
  + server-emit the load-bearing tasks and treat the node-binding as a manager event surface.
- `AUTO_CAPTURE_ENABLED=false` this repo — no adjacent-work stubs minted; no follow-ups surfaced
  beyond #0021's already-tracked router reuse.
