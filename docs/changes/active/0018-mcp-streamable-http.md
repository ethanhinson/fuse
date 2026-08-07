---
id: 18
slug: mcp-streamable-http
title: Streamable HTTP transport for MCP (v2025-03-26)
status: in-progress
priority: high
type: feat
created: 2026-08-06
updated: 2026-08-07
depends_on: [7]
related: [7, 19, 20, 21]
discovered_from: []
adrs: []
spec: docs/superpowers/specs/2026-08-07-mcp-streamable-http-design.md
plan: docs/superpowers/plans/2026-08-07-mcp-streamable-http-plan.md
results:
trivial: false
auto_groomable:
branch: feat/mcp-streamable-http
pr:
blocked_by:
reconciled: true
claimed_at: 2026-08-07T05:42:00Z
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-07-mcp-streamable-http-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-07-mcp-streamable-http-design.md) |
| Plan | [2026-08-07-mcp-streamable-http-plan.md](https://github.com/ethanhinson/fuse/blob/feat/mcp-streamable-http/docs/superpowers/plans/2026-08-07-mcp-streamable-http-plan.md) |
<!-- docket:artifacts:end -->

## Why

The existing MCP client supports stdio and HTTP/SSE transports (change 0007). The MCP spec v2025-03-26 introduced **Streamable HTTP** as the recommended HTTP transport, replacing bare SSE. In Streamable HTTP, a single HTTP connection handles both request and response via chunked transfer-encoding — no separate SSE endpoint needed. This simplifies proxy/firewall traversal, works naturally with serverless / edge environments (Cloudflare Workers, Vercel Edge Functions), and drops smoothly into the existing OAuth2 layer (Bearer token in `Authorization` header). Many newer MCP servers (Cursor, Windsurf, hosted providers) are adopting Streamable HTTP as their primary transport. Without it, fuse cannot reach a growing share of the MCP ecosystem.

## What changes

- A new `StreamableHTTPClient` (`internal/mcp/streamable_http_client.go`) satisfying the three-method `mcpConn` interface (`call` + `notify` + `stop`), selected by a new **`transport: "streamable-http"`** config value — no new boolean flag; the existing `url` + `auth` fields carry over unchanged.
- **Session lifecycle**: the client captures the `Mcp-Session-Id` response header (first seen on the manager-driven `initialize`), echoes it (and `MCP-Protocol-Version: 2025-03-26`) on every subsequent request, `DELETE`s the session on `stop()`, and refreshes it on a `404` (expired session) via an inline re-`initialize`. A stateless server (no session id) degrades cleanly.
- **Dual-mode responses**, request-scoped (no persistent background pump): each `call()` branches on the response `Content-Type` — `application/json` is one synchronous response; `text/event-stream` runs a short-lived SSE pump that resolves the matching id.
- **Resumability**: a response stream that disconnects mid-response reconnects via `Last-Event-Id` (bounded retries).
- **OAuth2 reuse**: `GetAccessToken` unchanged; manual `401`-refresh and `404`-reinit retries rewind the request body from `GetBody()` (regression-guarded).
- **Manager wiring**: a `"streamable-http"` case in **`dial()`** (`internal/mcp/manager.go`) — the transport-agnostic `handshakeAndDiscover` (from change 0019) drives `initialize`/`initialized`/`tools/list` through the interface unchanged.
- **Backward compatibility**: stdio and HTTP/SSE transports untouched; transport is config-selected, never auto-detected.

Full design — client struct, per-call flow, the notification seam, and test matrix — in the linked spec (reconciled against change 0019).

## Out of scope

- The standalone server-initiated `GET` SSE stream (server→client notifications). That is the notification-routing seam owned by changes 0020/0021 (dependent on 0019); this client leaves a named `handleServerFrame` seam but routes only per-call response frames.
- Deprecation or removal of the HTTP/SSE transport — both remain supported.
- WebSocket transport (change 0022); gRPC (not on the MCP standards track).

## Reconcile log

### 2026-08-07 — reconciled against origin/main (change 0019 merged)

Change 0019 (MCP capability negotiation, now done) landed the client-side init handshake and refactored the transport layer. The spec was materially revised:

- **`mcpConn` grew a third method, `notify`** — the new client implements `call` + `notify` + `stop`.
- **The transport switch moved from `startAndDiscover` to a new `dial()`**; a new `handshakeAndDiscover` drives `initialize` → `notifications/initialized` → `tools/list` transport-agnostically. The streamable client therefore **no longer owns an `initialize` exchange** — it captures `Mcp-Session-Id` from response headers inside `call()`/`notify()` while the manager drives the handshake.
- **`clientProtocolVersion = "2025-03-26"`** already exists (`capabilities.go`) — reused as the `MCP-Protocol-Version` header; no new constant.
- Reusable JSON-RPC types (`jsonrpcRequest`, `jsonrpcNotification`, `jsonrpcResponse`, `RPCError`) are adopted rather than reinvented.
- Local working tree was stale (behind `origin/main`); the feature branch cuts from `origin/main`, which carries 0019's code.

Net effect: simpler client (no owned handshake, no persistent pump), one `dial()` case, session capture folded into the per-request path.
