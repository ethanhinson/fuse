---
id: 7
slug: mcp-http-oauth
title: Remote MCP Servers via HTTP/SSE Transport + OAuth 2.0
status: in-progress
priority: high
type: feat
created: 2026-08-04
updated: 2026-08-04
depends_on: [3]
related: [3]
discovered_from: []
adrs: []
spec:
trivial: false
auto_groomable: false
branch: feat/mcp-http-oauth
claimed_at: 2026-08-04T00:00:00Z
pr: 5
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
<!-- docket:artifacts:end -->

## Why

The existing MCP client only supports stdio (child-process) transport. Remote MCP servers expose HTTP/SSE endpoints — a GET for the event stream, a POST for each request. Many hosted MCP services require OAuth 2.0 to authenticate. Without this, fuse can only reach locally-spawned servers.

## What changes

### Config (`internal/config/schema.go`)
- Add `URL string` and `Auth MCPAuthConfig` fields to `MCPServerConfig`.
- New `MCPAuthConfig` struct: `type` (none | bearer | oauth2), `client_id`, `client_secret`, `scopes`, `token_file`.

### `internal/mcp/conn.go` (new)
- Unexported `mcpConn` interface (`call`, `stop`) unifying stdio and HTTP transports within the package.

### `internal/mcp/tool.go`
- Change `client *StdioClient` field to `client mcpConn`.

### `internal/mcp/oauth.go` (new)
- `discoverAuthMeta` — GET `{url}/.well-known/oauth-authorization-server` (RFC 8414).
- `generatePKCE` — 32-byte random verifier, SHA-256 S256 challenge.
- `dynamicRegister` — POST to `registration_endpoint` when `client_id` is absent.
- `runBrowserFlow` — local callback server on random port, browser open, code exchange.
- `refreshTokens` — silent refresh via `refresh_token`.
- Token persistence at `~/.fuse/mcp-tokens/<name>.json`.
- `GetAccessToken(serverName, serverURL string, cfg MCPAuthConfig) (string, error)` — main entry point routing all three auth types.

### `internal/mcp/http_client.go` (new)
- `httpClient` — GET `/sse` opens SSE stream; `endpoint` event sets the messages URL; POST JSON-RPC to messages URL; SSE read pump fans responses to pending callers.
- Satisfies `mcpConn`.

### `internal/mcp/manager.go`
- Change `clients []mcpConn`.
- Route `transport: "http"` or `"sse"` to `newHTTPClient` after calling `GetAccessToken`.

## Out of scope

- Token revocation.
- mTLS / client certificate auth.
- Streaming (SSE chunked) tool results.
