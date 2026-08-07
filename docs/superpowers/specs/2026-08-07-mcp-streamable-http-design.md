<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0018 — Streamable HTTP transport for MCP (v2025-03-26)](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0018-mcp-streamable-http.md)**
<!-- docket:backlink:end -->

# Streamable HTTP transport for MCP (v2025-03-26)

**Change:** [#0018](../../changes/active/0018-mcp-streamable-http.md) · **Status:** design · **Date:** 2026-08-07

## Problem

fuse's MCP client speaks three things today: stdio, and the HTTP/SSE transport from
change 0007 (`internal/mcp/http_client.go` — a `GET /sse` endpoint-negotiation stream
plus a `POST` messages URL). The MCP spec **v2025-03-26** introduced **Streamable HTTP**
as the recommended HTTP transport, and newer servers (Cursor, Windsurf, hosted providers)
are standardizing on it. It differs from HTTP/SSE in three ways:

1. **Single endpoint.** No `GET /sse` + `POST /messages` split — the client POSTs
   JSON-RPC to one base URL.
2. **Dual response mode.** The server answers a POST *either* immediately with
   `Content-Type: application/json` (one synchronous JSON-RPC response) *or* with
   `Content-Type: text/event-stream` (a chunked SSE stream of JSON-RPC messages).
3. **Session management.** The server returns an `Mcp-Session-Id` header on the
   `initialize` response; the client echoes it on every subsequent request, and may
   `DELETE` it to end the session.

Without this transport fuse cannot reach a growing share of the MCP ecosystem. The OAuth2
layer (change 0007) ports over unchanged — Bearer token in `Authorization` — so the cost
is a new client, not new auth.

## Goals

- A `StreamableHTTPClient` in `internal/mcp/` satisfying the existing `mcpConn` interface
  (`call(ctx, method, params)` + `stop()`), selected by `transport: "streamable-http"`.
- Full Streamable HTTP **session lifecycle**: capture `Mcp-Session-Id` at `initialize`,
  echo it on every request, `DELETE` on `stop()`, re-initialize on a `404` (expired session).
- **Dual-mode response handling** per call: branch on the response `Content-Type`.
- **Resumability** of a response stream via `Last-Event-Id` when a stream disconnects
  mid-response.
- OAuth2 reuse (`GetAccessToken`) with a correct body-rewind on a `401`-triggered retry.
- Existing stdio and HTTP/SSE transports untouched; transport chosen by config, never
  auto-detected.

## Non-goals

- The standalone server-initiated `GET` SSE stream (server→client notifications with no
  prior request). That is the notification-routing seam owned by changes **0020**
  (`$/progress` / streaming tool results) and **0021** (resource subscriptions), which
  depend on capability negotiation (**0019**). This client leaves a clean seam for them
  (see *Notification seam*) but routes only per-call response frames.
- Deprecating or removing the HTTP/SSE transport — both remain supported.
- WebSocket (change 0022) and gRPC (not on the standards track).

## Design

### Config surface

`MCPServerConfig` (`internal/config/schema.go`) is selected by its `Transport` string; the
switch lives in `manager.go:startAndDiscover`. Add one transport value — **no new boolean
flag** (a `streamable: true` field was considered and rejected: it cuts against the
existing string-switch pattern and creates an ambiguous `http` + `streamable` product).

```yaml
mcp_servers:
  - name: hosted-example
    transport: streamable-http     # NEW value; stdio | http | sse | streamable-http
    url: https://mcp.example.com/  # base URL — POST target, no /sse suffix
    auth:                          # unchanged; oauth2 or bearer as today
      type: oauth2
      ...
```

`url` is the POST target verbatim (unlike HTTP/SSE, no `/sse` suffix is appended). `command`
stays stdio-only. No schema field is added; `Transport` already carries the selector.

### Client architecture — `StreamableHTTPClient`

New file `internal/mcp/streamable_http_client.go`. Holds:

```
name         string
baseURL      string
bearerToken  string           // from GetAccessToken at construction
http         *http.Client     // Timeout: 0 — per-call ctx governs deadlines
sessionID    string           // captured from initialize response; guarded by mu
protoVersion string           // negotiated protocol version, echoed as MCP-Protocol-Version
mu           sync.Mutex       // guards sessionID (re-init can rewrite it)
```

It satisfies `mcpConn`. Unlike `httpClient`, there is **no persistent background read
pump and no `pending` map** — Streamable HTTP is request-scoped: each `call()` owns its own
response (synchronous body or a short-lived SSE stream it drains to completion). This is
simpler and removes the id-matching pump entirely for the request/response path.

### Connect + initialize flow (`newStreamableHTTPClient`)

1. `GetAccessToken(name, url, auth)` — identical to the HTTP/SSE path in `startAndDiscover`.
2. POST an `initialize` JSON-RPC request (params: `protocolVersion: "2025-03-26"`,
   client capabilities, client info) to `url` with `Accept: application/json, text/event-stream`.
3. Read the **`Mcp-Session-Id` response header** and store it under `mu`. An absent header
   is legal (some servers are stateless) — treat empty session id as "omit the header on
   later requests," never an error.
4. Parse the `initialize` result (protocol version, server capabilities). Record
   `protoVersion` for the `MCP-Protocol-Version` request header the spec mandates on
   post-init requests.
5. POST the `initialized` notification (no id, no response expected).
6. Return the client; `startAndDiscover` then proceeds to `tools/list` as it does for every
   transport.

This client **owns its `initialize` exchange** because the session id is only available
from that response — it cannot rely on `startAndDiscover`'s current jump straight to
`tools/list`. Where this overlaps capability negotiation (**0019**), the reconcile pass at
build time re-checks whether a shared init helper already exists to call rather than
duplicating the handshake.

### Per-`call()` flow

1. Marshal the JSON-RPC request. Build the POST with `Accept: application/json,
   text/event-stream`, `Content-Type: application/json`, `Authorization: Bearer …`,
   `MCP-Protocol-Version: <protoVersion>`, and `Mcp-Session-Id: <sessionID>` when non-empty.
2. Set `req.GetBody` so the body can be replayed (see *OAuth retry + body rewind*).
3. `http.Client.Do` under the caller's `ctx`.
4. Branch on the **response**:
   - **`401`** → refresh the token once, rewind the body, retry (see below).
   - **`404`** with a session id set → the session expired. Re-run the initialize flow
     (new session id under `mu`), rewind the body, retry the original call **once**.
   - **`Content-Type: application/json`** → decode one `jsonrpcResponse`, return its
     result or map its error via the existing `errors.go` machinery.
   - **`Content-Type: text/event-stream`** → run the streaming pump (below) until the
     response matching this request's id arrives, then return.
   - other non-2xx → wrap as a transport error consistent with `http_client.go`.

### Streaming response pump (per call, not persistent)

Reuse the SSE frame parser shape from `readSSEPump`, but scoped to this one response body:

- Parse SSE events (`event:` / `data:` / `id:` lines) from the chunked body.
- For each `data:` frame, unmarshal a JSON-RPC message. When its `id` matches the request,
  capture the result and return (closing the body).
- **Track the last seen SSE `id:`** as `lastEventID` for resumability.
- **Id-less frames** (server notifications interleaved on the stream) are **not silently
  dropped into a void** — but per *Non-goals* they are also not routed anywhere yet. Send
  them to a single `handleServerFrame(raw)` seam that today logs-and-discards, so **0020/0021
  replace that one method** rather than reworking the pump. This is the explicit remedy for
  the `mcp-read-pumps-drop-inbound-notifications` learning: the drop is localized and named,
  not scattered.

### Resumability (`Last-Event-Id`)

If the response stream disconnects (EOF / read error) *before* the matching response
arrives and a `lastEventID` was seen, reconnect: issue a `GET` to `url` with
`Last-Event-Id: <lastEventID>`, the session header, and Bearer auth, and resume draining
from where the stream broke. Bound reconnects (small fixed cap, e.g. 2) so a
pathologically flapping stream fails the call instead of looping. A disconnect with no
`lastEventID` is an ordinary transport error (no resume possible).

### Session teardown (`stop()`)

If a session id is set, best-effort `DELETE` to `url` with the session header (short bounded
timeout; ignore the result — teardown must not block shutdown). Then release resources. A
stateless server (no session id) makes `stop()` a no-op beyond client cleanup.

### OAuth retry + body rewind

Streamable HTTP is a **POST with a body**, so this client is exactly the hazard the
`rewind-request-body-on-manual-retry` learning describes: `http.Client.Do` only auto-replays
the body via `GetBody` for its *own* redirect handling, not for our manual `401`/`404`
retries. Every retry path (token refresh, session re-init) **must reset `req.Body` from
`req.GetBody()` before re-issuing**, or attempt 2 sends an empty body. Set `GetBody` when
building the request and rewind on every manual retry.

### Manager wiring

In `startAndDiscover` (`internal/mcp/manager.go`), add a case alongside `"http"`/`"sse"`:

```go
case "streamable-http":
    if srv.URL == "" { return …, fmt.Errorf("… requires a url") }
    token, authErr := GetAccessToken(srv.Name, srv.URL, srv.Auth)
    …
    client, err = newStreamableHTTPClient(srv.Name, srv.URL, token)
```

`ServerStatus` reporting (the `StdioClient` type-assert block) needs no change — it already
degrades gracefully for non-stdio conns.

## Testing

Follow the existing `http_*_integration_test.go` pattern with `httptest.Server` doubles:

1. **Synchronous mode** — server replies `application/json`; assert `initialize` →
   `tools/list` → `tools/call` round-trips and results decode.
2. **Streaming mode** — server replies `text/event-stream` with chunked JSON-RPC frames;
   assert the pump matches by id and returns the right result.
3. **Session lifecycle** — server issues `Mcp-Session-Id` on init; assert every later
   request echoes it and `MCP-Protocol-Version`; assert `stop()` fires the `DELETE`.
4. **Session expiry** — server returns `404` once mid-session; assert one re-initialize +
   retry with the new id.
5. **401 refresh + body rewind** — server returns `401` once; assert the retry carries the
   refreshed token **and a non-empty body** (the regression guard for the learning).
6. **Resumability** — server closes the stream mid-response after emitting an `id:`; assert
   the client reconnects with `Last-Event-Id` and completes the call.
7. **Stateless server** — no `Mcp-Session-Id`; assert requests omit the header and `stop()`
   skips the `DELETE`.

Per `verify-tool-loop-at-gateway-seam`, the unit/integration doubles cover the transport;
no end-to-end binary verification is required for a transport-internal change.

## Learnings applied

- **`mcp-read-pumps-drop-inbound-notifications`** — the streaming pump routes id-less frames
  to a single named `handleServerFrame` seam (log-and-discard today) instead of dropping
  them inline, so 0020/0021 have one place to hook in.
- **`rewind-request-body-on-manual-retry`** — every manual retry (401 refresh, 404 re-init)
  rewinds `req.Body` from `req.GetBody()`; a dedicated test asserts a non-empty retried body.

## Open questions for build-time reconcile

- Whether change **0019** (capability negotiation, now done) landed a reusable client-side
  `initialize` helper this client should call rather than issuing its own handshake. If so,
  fold into it; if not, this client owns the exchange. The reconcile pass decides against
  the code as it stands at build time.
