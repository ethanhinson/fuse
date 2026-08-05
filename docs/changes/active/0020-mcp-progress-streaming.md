---
id: 20
slug: mcp-progress-streaming
title: MCP `$/progress` notifications and streaming tool results
status: proposed
priority: medium
type: feat
created: 2026-08-06
updated: 2026-08-06
depends_on: [19]
related: [18, 21]
discovered_from: [19]
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

Long-running MCP tool calls (code execution, multi-step retrievals, build commands) are currently opaque — the caller blocks until the tool completes, with no intermediate progress. The MCP 2025-03-26 spec formalizes the `$/progress` notification contract (was previously a convention) and introduces `$/stream` for continuous streaming of partial results. Implementing these would let fuse show real-time progress for long-running MCP tools in the TUI (progress bars in the agent tree, streaming partial results into the transcript), and would enable the MCP server side to stream incremental results back to clients like Claude Code and Cursor.

## What changes

- **`$/progress` notification sender** in the MCP client: tools can emit `{"jsonrpc":"2.0","method":"$/progress","params":{"progressToken":"...","progress":0.5,"total":1.0}}` during execution. The client-side notification handler routes these to the agent tree as progress events (new `ProgressEvent` type on `AgentNode`).
- **`$/progress` notification receiver** in the MCP server (`mcp_server.go`): when fuse's MCP server receives a `$/progress` notification from the client, it forwards it to the tool's renderer (TUI progress bar or plain-text percentage).
- **`$/stream` notification support**: tools that support streaming (e.g. a search that returns results incrementally) emit `$/stream` notifications with partial content chunks. The fuse MCP client assembles chunks in order and presents the full stream as the tool result, while optionally updating inline in the TUI.
- **Progress token tracking**: tool calls that include a `progressToken` in their MCP arguments are tracked by the manager; progress notifications are matched by token and forwarded to the agent tree.
- **TUI integration**: the agent tree node for a running tool shows a progress bar (e.g. `[████░░░░] 50%`); streaming tools show a rolling partial content line that resolves to the final result on completion.

## Out of scope

- `$/stream` on the client side for fuse's own tool results (web_search, bash) — only MCP-tool results stream.
- Streaming partial child agent prose into the parent (agent-runtime concern, not MCP).

## Research notes (input for the brainstorm)

The `$/progress` notification carries `progressToken` (string or integer, matching the one in the original tool call), `progress` (number, 0.0–1.0 or count), and optional `total` (for determinate progress). The `$/stream` notification has the same shape with a `delta` field containing the incremental content. The key design tension is whether the MCP client buffering model should be "hold until complete" (current behavior) or "stream progressively" (new). The safest path is a hybrid: stream into a ring buffer for the TUI display, but deliver the concatenated result as the tool result value — so the agent loop sees the same complete result it does today, but the human sees progress. This matches how Claude Code handles it.
