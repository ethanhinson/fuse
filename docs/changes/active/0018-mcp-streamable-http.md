---
id: 18
slug: mcp-streamable-http
title: Streamable HTTP transport for MCP (v2025-03-26)
status: proposed
priority: high
type: feat
created: 2026-08-06
updated: 2026-08-06
depends_on: [7]
related: [7]
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

The existing MCP client supports stdio and HTTP/SSE transports (change 0007). The MCP spec v2025-03-26 introduced **Streamable HTTP** as the recommended HTTP transport, replacing bare SSE. In Streamable HTTP, a single HTTP connection handles both request and response via chunked transfer-encoding — no separate SSE endpoint needed. This simplifies proxy/firewall traversal, works naturally with serverless / edge environments (Cloudflare Workers, Vercel Edge Functions), and drops smoothly into the existing OAuth2 layer (Bearer token in `Authorization` header). Many newer MCP servers (Cursor, Windsurf, hosted providers) are adopting Streamable HTTP as their primary transport. Without it, fuse cannot reach a growing share of the MCP ecosystem.

## What changes

- A new `StreamableHTTPClient` in `internal/mcp/` implementing the `mcpConn` interface.
- **Connection flow**: single POST to a configured endpoint URL with `Content-Type: application/json` and `Accept: text/event-stream` (for streaming responses) or `Accept: application/json` (for immediate responses). The server responds either immediately (standard JSON-RPC response) or streams back newline-delimited JSON-RPC messages using `Transfer-Encoding: chunked`.
- **Streaming response pump**: if the server responds with `Content-Type: text/event-stream`, a read pump (similar to the existing `readSSEPump`) parses SSE events and fans them to pending callers. If the response is immediate `application/json`, it's treated as a single synchronous response.
- **Integration into `internal/mcp/manager.go`**: route `transport: "streamable-http"` or `transport: "http"` with `streamable: true` to `newStreamableHTTPClient`. Reuse the existing `GetAccessToken` OAuth2 flow unchanged.
- **Config surface**: `MCPServerConfig` gains an optional `streamable: true` flag (default `false` for backward compat) alongside existing `url` and `auth` fields.
- **Backward compatibility**: existing HTTP/SSE servers and stdio servers continue to work unchanged. The transport is selected by the config, not auto-detected.

## Out of scope

- Deprecation or removal of the HTTP/SSE transport — both remain supported.
- WebSocket transport (separate change).
- gRPC transport (not on the MCP standards track).

## Research notes (input for the brainstorm)

Streamable HTTP differs from HTTP/SSE in three key ways: (1) single endpoint instead of GET/sse + POST/messages split; (2) the server may respond immediately (synchronous) or via chunked-encoding SSE (asynchronous); (3) no SSE `endpoint` event negotiation — the URL is just the server's base URL. The OAuth2 flow is identical (Bearer token), so the existing `GetAccessToken`, PKCE, token refresh, and credential caching all port directly. The Go stdlib `net/http` handles chunked transfer-encoding transparently on the response body read side; the challenge is detecting which response mode the server chose and routing accordingly. Some servers may advertise capability in the init response — see capability negotiation (change 0019).
