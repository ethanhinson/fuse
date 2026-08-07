<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0018 — Streamable HTTP transport for MCP (v2025-03-26)](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0018-mcp-streamable-http.md)**
<!-- docket:backlink:end -->

# Streamable HTTP transport for MCP (v2025-03-26)

**Change:** [#0018](../../changes/active/0018-mcp-streamable-http.md) · **Status:** design (reconciled 2026-08-07) · **Date:** 2026-08-07

## Problem

fuse's MCP client speaks stdio and the HTTP/SSE transport from change 0007
(`internal/mcp/http_client.go` — a `GET /sse` endpoint-negotiation stream plus a `POST`
messages URL). The MCP spec **v2025-03-26** introduced **Streamable HTTP** as the
recommended HTTP transport, and newer servers (Cursor, Windsurf, hosted providers, DeepWiki,
Context7) are standardizing on it. It differs from HTTP/SSE in three ways:

1. **Single endpoint.** No `GET /sse` + `POST /messages` split — the client POSTs
   JSON-RPC to one base URL.
2. **Dual response mode.** The server answers a POST *either* immediately with
   `Content-Type: application/json` (one synchronous JSON-RPC response) *or* with
   `Content-Type: text/event-stream` (a chunked SSE stream of JSON-RPC messages).
3. **Session management.** The server returns an `Mcp-Session-Id` header on the
   `initialize` response; the client echoes it on every subsequent request, and may
   `DELETE` it to end the session.

Without this transport fuse cannot reach a growing share of the MCP ecosystem. OAuth2
(change 0007) ports over unchanged — Bearer token in `Authorization`.

## Reconciliation note (2026-08-07)

This spec was **reconciled against `origin/main` after change 0019 (capability negotiation,
now done) merged.** 0019 refactored the transport layer in ways that materially simplify
this change:

- **`mcpConn` now has three methods** — `call`, **`notify`**, `stop` (`internal/mcp/conn.go`).
  The new client implements all three.
- **The transport switch moved to `dial(srv)`**; a new `handshakeAndDiscover(ctx, client,
  name)` drives the mandatory handshake — `initialize` → `notifications/initialized` →
  `tools/list` — **transport-agnostically through the interface** (`manager.go`).
- Therefore **this client does NOT own an `initialize` exchange.** The manager sends
  `initialize` via `client.call(...)`; the client captures `Mcp-Session-Id` from *that
  response's headers* transparently inside `call()`.
- **`clientProtocolVersion = "2025-03-26"`** already exists (`capabilities.go`) — exactly
  this transport's version. The client sends it as the `MCP-Protocol-Version` header; no new
  constant.
- Reusable types exist: `jsonrpcRequest`, `jsonrpcNotification`, `jsonrpcResponse`,
  `jsonrpcError`, and `RPCError{Code, Message}` (error mapping) — the client reuses them, no
  new JSON-RPC plumbing.

The original design (client owns its own `initialize`/`initialized` handshake) is
**superseded** by the sections below.

## Goals

- A `StreamableHTTPClient` (`internal/mcp/streamable_http_client.go`) satisfying the
  three-method `mcpConn` interface, selected by `transport: "streamable-http"` in `dial()`.
- **Session lifecycle**: capture `Mcp-Session-Id` from response headers, echo it on every
  request, `DELETE` on `stop()`, and refresh it on a `404` (expired session) via an inline
  re-`initialize`.
- **Dual-mode responses** per call: branch on the response `Content-Type`.
- **Resumability** of a response stream via `Last-Event-Id` when a stream disconnects
  mid-response.
- OAuth2 reuse (`GetAccessToken`) with a correct body-rewind on a `401`/`404`-triggered retry.
- Existing stdio and HTTP/SSE transports untouched; transport chosen by config, never
  auto-detected.

## Non-goals

- The standalone server-initiated `GET` SSE stream (server→client notifications with no
  prior request). That is the notification-routing seam owned by changes **0020**
  (`$/progress` / streaming tool results) and **0021** (resource subscriptions). This client
  leaves a named `handleServerFrame` seam but routes only per-call response frames.
- Deprecating or removing the HTTP/SSE transport — both remain supported.
- WebSocket (change 0022) and gRPC (not on the standards track).
- General transport auto-reconnect (no fuse transport reconnects today) — only the two
  request-scoped recoveries below (401 refresh, 404 session refresh, Last-Event-Id resume).

## Design

### Config surface

`MCPServerConfig` (`internal/config/schema.go`) is selected by its `Transport` string; the
switch lives in `manager.go:dial`. Add one transport value — **no new boolean flag** (a
`streamable: true` field was considered and rejected: it cuts against the string-switch
pattern and creates an ambiguous `http` + `streamable` product).

```yaml
mcp_servers:
  - name: deepwiki
    transport: streamable-http     # NEW value; stdio | http | sse | streamable-http
    url: https://mcp.deepwiki.com/mcp   # base URL — POST target, no /sse suffix
    # auth: optional; oauth2 or bearer as today (DeepWiki/Context7 are no-auth)
```

`url` is the POST target verbatim (no `/sse` suffix appended). `command` stays stdio-only.
No schema field is added; `Transport` already carries the selector.

### Client architecture — `StreamableHTTPClient`

New file `internal/mcp/streamable_http_client.go`:

```
name         string
baseURL      string
bearerToken  string           // from GetAccessToken; mutable across a 401 refresh
http         *http.Client     // Timeout: 0 — per-call ctx governs deadlines
counter      atomic.Uint64    // request id source (mirrors httpClient)
mu           sync.Mutex       // guards sessionID + bearerToken + closed
sessionID    string           // captured from any response's Mcp-Session-Id header
closed       bool
```

Satisfies `mcpConn` (`call` + `notify` + `stop`). Unlike `httpClient` there is **no
persistent background read pump, no `pending` map, and no `done` channel** — Streamable HTTP
is request-scoped: each `call()` owns its own response (a synchronous body or a short-lived
SSE stream it drains to completion). No handshake here — the manager drives it.

### Construction (`newStreamableHTTPClient`)

`dial()` calls `GetAccessToken(name, url, auth)` (identical to the HTTP/SSE branch) and
`newStreamableHTTPClient(name, url, token)`. Construction just builds the struct and the
`*http.Client{Timeout: 0}` — **no network I/O**; the first request is the manager's
`initialize` call. `dial()` gains:

```go
case "streamable-http":
    if srv.URL == "" { return nil, fmt.Errorf("mcp server %q: transport %q requires a url", srv.Name, srv.Transport) }
    token, authErr := GetAccessToken(srv.Name, srv.URL, srv.Auth)
    if authErr != nil { return nil, fmt.Errorf("mcp server %q: auth: %w", srv.Name, authErr) }
    return newStreamableHTTPClient(srv.Name, srv.URL, token)
```

### Session capture (no owned handshake)

The manager's `handshakeAndDiscover` sends `initialize` through `client.call()`. Inside every
`call()`/`notify()`, **after receiving the HTTP response, read the `Mcp-Session-Id` response
header**; if non-empty, store it under `mu`. The `initialize` response is where it first
appears, but capturing on every response is harmless and covers servers that (re)issue it.
An absent header is legal (stateless server) — the client simply never sends the session
header. `notifications/initialized` flows through `notify()` and already carries the captured
session id.

### Per-`call()` flow

1. `id := counter.Add(1)`; marshal `jsonrpcRequest{JSONRPC:"2.0", ID:id, Method, Params}`.
2. Build POST to `baseURL` via `http.NewRequestWithContext(ctx, ...)`, set:
   `Accept: application/json, text/event-stream`, `Content-Type: application/json`,
   `MCP-Protocol-Version: 2025-03-26`, `Authorization: Bearer …` (when set), and
   `Mcp-Session-Id: …` (when set). Set `req.GetBody` (see *body rewind*).
3. `http.Client.Do` under the caller's `ctx`; capture the session header.
4. Branch on the response:
   - **`401`** → refresh the token once (`GetAccessToken`), rewind body, retry once.
   - **`404`** with a session set → session expired: clear it, run an inline
     `initialize` POST (reusing `initializeParams()`) to obtain a fresh `Mcp-Session-Id`,
     rewind the original body, retry once. (Bounded: one refresh per call.)
   - **`Content-Type: application/json`** → decode one `jsonrpcResponse`; on `resp.Error`
     return `&RPCError{Code, Message}` (matching `httpClient`), else `resp.Result`.
   - **`text/event-stream`** → run the per-call streaming pump (below) until the frame whose
     `id` matches arrives; return its result/error.
   - other non-2xx (and not 200/202) → wrapped transport error, mirroring `http_client.go`.

### Streaming response pump (per call, not persistent)

Reuse the SSE frame-parse shape from `readSSEPump`, scoped to this one response body:

- Parse SSE `event:` / `data:` / `id:` lines. Track the last `id:` seen as `lastEventID`.
- Each complete `data:` frame → unmarshal `jsonrpcResponse`. If `resp.ID == id` (this call),
  capture and return (close body).
- **Id-less frames** (interleaved server notifications) go to a single
  `handleServerFrame(raw)` seam that logs-and-discards today — the explicit remedy for the
  `mcp-read-pumps-drop-inbound-notifications` learning, so 0020/0021 replace that one method
  rather than reworking the pump. Frames with a *different* id are likewise handed to the
  seam (not this call's response).

### Resumability (`Last-Event-Id`)

If the stream disconnects (EOF/read error) before the matching response arrives and a
`lastEventID` was seen, reconnect: `GET baseURL` with `Last-Event-Id: <lastEventID>`, the
session header, `MCP-Protocol-Version`, and Bearer auth; resume draining. Bound reconnects
(fixed cap, e.g. 2). A disconnect with no `lastEventID` is an ordinary transport error.

### `notify()`

Marshal `jsonrpcNotification{JSONRPC:"2.0", Method, Params}` (no id), POST with the same
headers (session + protocol + auth), accept `200`/`202`, drain+close body, capture any
session header. Mirrors `httpClient.notify`.

### `stop()`

Under `mu`: if not already closed and a session id is set, best-effort `DELETE baseURL` with
the session header + auth (short bounded context; ignore result — teardown must not block
shutdown). Mark `closed = true`. A stateless server makes `stop()` a near no-op.

### OAuth retry + body rewind

Streamable HTTP is a **POST with a body**, exactly the `rewind-request-body-on-manual-retry`
hazard: `http.Client.Do` only auto-replays via `GetBody` for its *own* redirect handling,
not for our manual `401`/`404` retries. Every manual retry **resets `req.Body` from
`req.GetBody()` before re-issuing**, or attempt 2 sends an empty body. Set `GetBody` at build
time; rewind on every retry.

### Manager wiring

Only `dial()` changes (the `case "streamable-http"` above). `handshakeAndDiscover`,
`startAndDiscover`'s signature, and `ServerStatus` reporting are untouched — the new client
satisfies `mcpConn`, so the handshake, capability parse, and discovery all work unchanged.

## Testing

`httptest.Server` doubles, following `http_*_integration_test.go`:

1. **Synchronous mode** — server replies `application/json`; assert `initialize` (via the
   real `handshakeAndDiscover` path) → `tools/list` → `tools/call` round-trip and decode.
2. **Streaming mode** — server replies `text/event-stream` chunked frames; assert the pump
   matches by id and returns the right result.
3. **Session lifecycle** — server issues `Mcp-Session-Id` on the init response; assert every
   later request echoes it + `MCP-Protocol-Version`; assert `stop()` fires the `DELETE`.
4. **Session expiry** — server returns `404` once mid-session; assert one inline
   re-`initialize` + retry with the new id.
5. **401 refresh + body rewind** — server returns `401` once; assert the retry carries the
   refreshed token **and a non-empty body** (the regression guard for the learning).
6. **Resumability** — server closes the stream mid-response after an `id:`; assert reconnect
   with `Last-Event-Id` and completion.
7. **Stateless server** — no `Mcp-Session-Id`; assert requests omit the header and `stop()`
   skips the `DELETE`.

### Live verification (per the human's directive)

Beyond doubles, verify the **real fuse TUI against a reputable public Streamable HTTP MCP
server** — first reachable of: DeepWiki `https://mcp.deepwiki.com/mcp`, Context7
`https://mcp.context7.com/mcp` (both no-auth). Configure it under `mcp_servers` with
`transport: streamable-http`, launch the binary, and confirm the server connects, tools are
discovered (`/mcp` status), and a tool call round-trips. Recorded as a merge-gate step in the
results file (`verify-tool-loop-at-gateway-seam`: the transport is exercised end-to-end, not
just at the Completer seam).

## Learnings applied

- **`mcp-read-pumps-drop-inbound-notifications`** — id-less/foreign-id frames route to a
  single named `handleServerFrame` seam, not dropped inline; 0020/0021 hook there.
- **`rewind-request-body-on-manual-retry`** — every manual retry (401 refresh, 404 re-init)
  rewinds `req.Body` from `req.GetBody()`; a test asserts a non-empty retried body.
- **`bound-every-model-call`** (transport analogue) — per-call `ctx` deadlines govern; no
  unbounded `http.DefaultClient` use; reconnects are capped.
