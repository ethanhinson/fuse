---
id: 22
slug: mcp-websocket-transport
title: WebSocket transport for MCP
status: killed
priority: low
type: feat
created: 2026-08-06
updated: 2026-08-08
depends_on: [7]
related: [18]
discovered_from: []
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

MCP is converging on three transport families: stdio (local child processes), Streamable HTTP (remote, serverless-friendly), and WebSocket (persistent full-duplex connections). WebSocket offers true bidirectional communication without the asymmetry of SSE (client→server via POST, server→client via SSE stream). Several MCP clients (Cursor, Windsurf) and servers (real-time data feeds, long-running computations) prefer WebSocket for its lower per-message overhead and native push model. Adding WebSocket transport ensures fuse can connect to the full range of MCP endpoints.

## What changes

- **New `wsClient` in `internal/mcp/`** implementing `mcpConn` via `gorilla/websocket` (the de facto Go WebSocket library, widely used and well-maintained).
- **Connection flow**: dial `ws://` or `wss://` URL with optional `Authorization: Bearer` header (reusing existing `GetAccessToken`). Upgrade to WebSocket, then JSON-RPC messages flow bidirectionally over the WebSocket connection.
- **Read pump**: goroutine reading JSON-RPC responses from the WebSocket, fanning to pending channels by ID (same pattern as stdio and HTTP/SSE).
- **Write**: serialize JSON-RPC request as text frame and write to the WebSocket.
- **Ping/pong keepalive**: use WebSocket's built-in ping/pong (the server-side ping interval is configurable; the client responds automatically via `SetPongHandler`).
- **Transport selection in `manager.go`**: route `transport: "ws"` or `"wss"` to `newWSClient`. Error if the URL scheme doesn't match.
- **Server side** (`mcp_server.go`): optionally listen on a WebSocket endpoint alongside stdio. Reuse the same JSON-RPC dispatcher.

## Out of scope

- WebRTC DataChannel transport — experimental, not yet on the MCP standards track.
- gRPC transport — not on the MCP standards track.
- Removing stdio or HTTP transports — all three remain supported.

## Research notes (input for the brainstorm)

WebSocket is a natural fit for MCP because (a) every JSON-RPC message maps to a single text frame, (b) the connection model eliminates the SSE asymmetry, (c) the stdlib `net/http` can upgrade to WebSocket trivially with gorilla/websocket, and (d) the existing pending-channel response fan-out model maps directly (read pump goroutine reads frames, fans to pending by ID). The main challenge is reconnection semantics: WebSocket connections drop, and the MCP server may have state that needs re-negotiating. The safest approach is to treat a WebSocket drop as equivalent to a server restart — re-initialize and re-discover tools. The gorilla/websocket library has a `CloseNormalClosure` code and supports `SetCloseHandler` for clean shutdown.

## Why killed

Killed at grooming (2026-08-08) after research showed WebSocket MCP is a phantom transport with essentially nothing real to connect to:

- **Not in the MCP spec.** The current spec (`2025-06-18`) defines exactly two transports — stdio and Streamable HTTP. WebSocket appears only under the generic "custom transport" escape hatch.
- **The standards-track attempt was closed unmerged.** SEP-1288 / PR #1287 ("WebSocket transport for MCP") was closed on 2026-12-03 [sic — Dec 3, 2025] without merging; an earlier proposal (issue #493) was also closed. Maintainers deliberately chose Streamable HTTP: WebSocket adds overhead for RPC-style calls, browsers cannot set `Authorization` on the WS handshake, only GET upgrades, and it is harder to load-balance / proxy / run serverless.
- **No mainstream client uses it.** The stub's premise ("Cursor, Windsurf prefer WebSocket") is factually wrong — Cline, Cursor, and Windsurf all use stdio + Streamable HTTP (with legacy SSE fallback). No well-known server exposes `wss://`; only hobbyist/demo repos.
- **Already covered.** Streamable HTTP (#0018, done) + legacy HTTP/SSE (#0007, done) reach the entire live MCP ecosystem today.

Watch item, not a reason to keep this open: the MCP transport working group has repeatedly entertained bidirectional transports (SEP-1288, gRPC #1352, postMessage #1005), and MCP governance moved under the Linux Foundation in Dec 2025. If WebSocket is ever adopted into the spec with real servers to reach, file a fresh change then — there is nothing merged to build against now.

## Why killed

Phantom MCP transport: not in spec (SEP-1288 closed unmerged), no real servers/clients, superseded by Streamable HTTP (#0018) + HTTP/SSE (#0007)
