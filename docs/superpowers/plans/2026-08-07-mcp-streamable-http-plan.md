<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0018 — Streamable HTTP transport for MCP (v2025-03-26)](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0018-mcp-streamable-http.md)**
<!-- docket:backlink:end -->

# Implementation plan — Streamable HTTP transport for MCP (change 0018)

Spec: `docs/superpowers/specs/2026-08-07-mcp-streamable-http-design.md` (reconciled against change 0019).
Method: TDD, task-by-task. Each task = focused failing test → implement → `go test ./internal/mcp/...` green → commit.

## Context (post-0019 reality)

- `mcpConn` = `call(ctx, method, params)` + `notify(ctx, method, params)` + `stop()` (`internal/mcp/conn.go`).
- Transport switch is in `dial(srv)` (`internal/mcp/manager.go`); `handshakeAndDiscover` drives `initialize` → `notifications/initialized` → `tools/list` through the interface.
- Reuse: `jsonrpcRequest`, `jsonrpcNotification`, `jsonrpcResponse`, `jsonrpcError`, `RPCError{Code,Message}` (`client.go`/`errors.go`); `clientProtocolVersion = "2025-03-26"` (`capabilities.go`).
- New file: `internal/mcp/streamable_http_client.go`; tests in `internal/mcp/streamable_http_client_test.go`.

## Task 1 — Scaffolding + `dial()` routing

- Add `StreamableHTTPClient` struct (name, baseURL, bearerToken, http, counter, mu, sessionID, closed) and `newStreamableHTTPClient(name, url, token) (*StreamableHTTPClient, error)` — pure construction, no I/O.
- Method stubs so it satisfies `mcpConn`.
- Add `case "streamable-http":` to `dial()` — require `url`, call `GetAccessToken`, return the client. Mirror the `http`/`sse` branch.
- **Test:** `dial()` with `transport: "streamable-http"` + a url returns a `*StreamableHTTPClient`; empty url errors with "requires a url".

## Task 2 — Synchronous `call()` (`application/json`) + session capture

- `call()`: `id := counter.Add(1)`; marshal `jsonrpcRequest`; POST to `baseURL` with headers `Accept: application/json, text/event-stream`, `Content-Type: application/json`, `MCP-Protocol-Version: clientProtocolVersion`, `Authorization` (when token), `Mcp-Session-Id` (when set). Set `req.GetBody`.
- After `Do`: capture `Mcp-Session-Id` response header under `mu`. Reject non-200/202. Branch: `application/json` → decode one `jsonrpcResponse`; map `resp.Error` → `&RPCError{...}`, else return `resp.Result`.
- **Test:** httptest double replies `application/json`. Drive the **real** `handshakeAndDiscover` (initialize → initialized → tools/list) end-to-end; assert tools discovered, and that requests after init carry `Mcp-Session-Id` + `MCP-Protocol-Version`.

## Task 3 — `notify()`

- Marshal `jsonrpcNotification` (no id); POST with session + protocol + auth headers; accept 200/202; drain+close body; capture session header.
- **Test:** double records the `notifications/initialized` POST; assert no id, session header echoed once init set it.

## Task 4 — Streaming `call()` (`text/event-stream`) + notification seam

- When response `Content-Type` is `text/event-stream`: run a per-call SSE parser over the body (`event:`/`data:`/`id:`), tracking `lastEventID`. Match `resp.ID == id` → return. Route id-less **and** foreign-id frames to `handleServerFrame(raw)` (log-and-discard) — the `mcp-read-pumps-drop-inbound-notifications` seam.
- **Test:** double streams a chunked SSE response (optionally an interleaved id-less notification first); assert the matching result returns and the id-less frame does not corrupt it.

## Task 5 — Session lifecycle: `stop()` `DELETE` + full echo

- `stop()`: under `mu`, if not closed and session set, best-effort `DELETE baseURL` with session + auth headers (short bounded ctx); mark closed.
- **Test:** full lifecycle — server issues `Mcp-Session-Id` at init; assert every later request echoes it; assert `stop()` fires a `DELETE` carrying it. Separate: stateless server (no session id) → requests omit the header, `stop()` fires no `DELETE`.

## Task 6 — 401 refresh + body rewind (regression-guarded)

- On `401`: refresh token via `GetAccessToken`, **rewind `req.Body` from `req.GetBody()`**, retry once. (One refresh per call.)
- **Test (the `rewind-request-body-on-manual-retry` guard):** double returns 401 on attempt 1, 200 on attempt 2, **recording every request body**; assert attempt 2 carries the refreshed token AND a non-empty, complete body.

## Task 7 — 404 session expiry → inline re-initialize + retry

- On `404` with a session set: clear session, POST an inline `initialize` (reuse `initializeParams()`) to get a fresh `Mcp-Session-Id`, rewind body, retry the original call once.
- **Test:** double 404s once mid-session then succeeds with a new session id; assert one re-init and the retry uses the new id.

## Task 8 — Resumability (`Last-Event-Id`)

- If a streaming response body disconnects before the matching frame and a `lastEventID` was seen: reconnect via `GET baseURL` with `Last-Event-Id` + session + protocol + auth; resume draining. Bound reconnects (cap 2).
- **Test:** double closes the stream after emitting an `id:` frame, then serves the remainder on the `Last-Event-Id` GET; assert the call completes.

## Task 9 — Full-suite gate + live verification

- `go build ./... && go test ./...` green.
- **Live (merge-gate, manual):** configure a public Streamable HTTP MCP server (DeepWiki `https://mcp.deepwiki.com/mcp`, else Context7 `https://mcp.context7.com/mcp`) under `mcp_servers` with `transport: streamable-http`; run the real binary; confirm connect + tool discovery + a tool-call round-trip via the TUI / `fuse mcps list --live`. Record in the results file.

## Out of scope (per spec)

Standalone server-initiated GET notification stream (0020/0021), WebSocket (0022), transport auto-reconnect beyond the three request-scoped recoveries.
