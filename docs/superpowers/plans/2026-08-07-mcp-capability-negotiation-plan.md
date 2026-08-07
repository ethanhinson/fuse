<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0019 — MCP capability negotiation — structured init handshake](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0019-mcp-capability-negotiation.md)**
<!-- docket:backlink:end -->

# Plan — MCP capability negotiation (structured init handshake)

**Change:** #0019 · **Spec:** `docs/superpowers/specs/2026-08-06-mcp-capability-negotiation-design.md` (on `docket`)
**Branch:** `feat/mcp-capability-negotiation` · **Date:** 2026-08-07

> Plan authored by the docket-implement-next **auto-fallback** — `superpowers:writing-plans`
> was not available in this session. Method is the agent's; the artifact (this plan + a
> TDD build) is unchanged.

Each task is test-first (TDD): write the failing test, implement, green, refactor. Tasks are
ordered so each builds on the last with the suite green at every boundary. All code lands in
`internal/mcp/` and `cmd/fuse/` on this feature branch — no docket metadata is touched here.

## Task 1 — `notify` transport path (id-less JSON-RPC notifications)

**Why first:** the handshake's `initialized` message needs it; both transports must satisfy it.

- **Test** (`client_error_test.go` / a new `notify_test.go`):
  - `StdioClient.notify(ctx, "notifications/initialized", nil)` writes a frame to the server's
    stdin whose JSON has **no `id` key** and `method: "notifications/initialized"`, and returns
    without blocking for a response. Assert by capturing the encoder's output (pipe the stdio
    server side and decode the raw frame → assert `_, ok := m["id"]; !ok`).
  - `httpClient.notify` POSTs an id-less frame (drive against an `httptest.Server` recording the
    body; assert no `id` key) and returns nil on 200/202.
- **Implement:**
  - Add to `mcpConn` (`conn.go`): `notify(ctx context.Context, method string, params any) error`.
  - New frame type in `client.go`: `jsonrpcNotification{JSONRPC, Method, Params omitempty}` — **no
    `ID` field** (the existing `jsonrpcRequest.ID` is not `omitempty`, so it can't express a
    notification).
  - `StdioClient.notify`: under `c.mu`, guard on `c.done`, `c.enc.Encode(jsonrpcNotification{...})`;
    no pending channel, no wait.
  - `httpClient.notify`: marshal the notification, POST to `messagesURL` with the Bearer header
    (mirroring `call`), drain+close the response body, return an error only on transport failure /
    non-2xx. No pending registration.
- **Green:** `go test ./internal/mcp/ -run Notify`.

## Task 2 — `ServerCapabilities` value + `Supports` + `parseInitializeResult`

Pure, table-tested logic — the contract #21 will depend on. No I/O.

- **Test** (`capabilities_test.go`):
  - `Supports` table: bare key present (`logging` → true), bare key absent (false), bare key with
    JSON `null` value (false), dotted `resources.subscribe` with nested `true` (true), nested
    `false` (false), nested key missing (false), `resources` absent entirely (false), malformed
    nested object (false — fails open).
  - `parseInitializeResult`: (a) full 2025-03-26 result → correct `Supports` answers + echoed
    `protocolVersion`; (b) minimal result with **no** `capabilities` field → empty set (all
    `Supports` false) + version echoed; (c) garbage `capabilities` (a JSON string, not object) →
    empty set, **no error**.
- **Implement** (`capabilities.go`):
  - `type ServerCapabilities struct { raw map[string]json.RawMessage }`.
  - `func (c ServerCapabilities) Supports(key string) bool` — split on the first `.`; bare = top
    key present and not JSON `null`; dotted = unmarshal the top object to
    `map[string]json.RawMessage`, nested value must unmarshal to bool `true`. Any unmarshal error →
    `false`.
  - `func (c ServerCapabilities) Keys() []string` — sorted display list: top-level keys present,
    with `resources` expanded to `resources.subscribe` / `resources.listChanged` when those nested
    flags are true. Used by the `--live` surface.
  - `func parseInitializeResult(raw json.RawMessage) (ServerCapabilities, string)` — unmarshal
    `{protocolVersion, capabilities}`; a missing/malformed `capabilities` yields an empty `raw`
    map; never returns an error.
- **Green:** `go test ./internal/mcp/ -run Capab`.

## Task 3 — Client handshake in `startAndDiscover` + `managedServer` storage

Wire the handshake into connect; this is the behavioral core.

- **Test** (`integration_test.go` / `manager_test.go`, using the existing mock-stdio harness):
  - **Ordering:** a mock server records received methods; after `Add`, assert the sequence begins
    `initialize`, then the id-less `notifications/initialized`, then `tools/list`.
  - **Init hard-fail:** a mock server that returns a JSON-RPC error to `initialize` → `Add` returns
    an error, `Status()` shows the server `Connected:false` with `connErr` set, and **no tools**
    registered (assert the registry has none from this server).
  - **Capabilities stored:** a mock server advertising `{"resources":{"subscribe":true}}` →
    `Supports("resources.subscribe")` true on the stored `managedServer.caps`.
- **Implement** (`manager.go`):
  - `initializeParams()` → `{protocolVersion:"2025-03-26", capabilities:{}, clientInfo:{name:"fuse",
    version:"1.0.0"}}`.
  - In `startAndDiscover`, before `tools/list`: `call(ctx,"initialize",initializeParams())`; on error
    `client.stop(); return … fmt.Errorf("initialize: %w", err)`. Then
    `caps, protoVer := parseInitializeResult(init)`, `client.notify(ctx,"notifications/initialized",nil)`
    (log-and-ignore its error).
  - Widen `startAndDiscover` return to `(mcpConn, []*MCPTool, ServerCapabilities, string, error)`;
    update its **sole caller** `Manager.Add` to store `caps`/`protoVer` on the `managedServer`.
  - `managedServer` gains `caps ServerCapabilities` and `protoVer string`.
- **Green:** `go test ./internal/mcp/`.

## Task 4 — `--live` status surface

Expose the negotiated values through `Status()` and render them.

- **Test:**
  - `manager_test.go`: `Status()` populates `ProtocolVersion` and `Capabilities` from a connected
    mock server.
  - `cmd/fuse/main_test.go` (or a focused mcps test): `fuse mcps ... --live` output includes a
    protocol-version + capabilities surface; a server advertising none shows `none`, not blank.
- **Implement:**
  - `ServerStatus` (`manager.go`) gains `ProtocolVersion string` and `Capabilities []string`;
    `Status()` fills them from `ms.protoVer` / `ms.caps.Keys()`.
  - `cmd/fuse/mcps.go` `--live` table: add a `PROTO` column and a `CAPS` column (comma-joined
    `Capabilities`, `none` when empty). Keep the existing columns and alignment.
- **Green:** `go test ./internal/mcp/ ./cmd/fuse/`.

## Task 5 — fuse server side: version bump + echo

- **Test** (`server_test.go`):
  - `initialize` with params `{"protocolVersion":"2025-03-26"}` → response echoes `2025-03-26`,
    `capabilities == {"tools":{}}`.
  - `initialize` with `{"protocolVersion":"2024-11-05"}` (a recognized older version) → response
    echoes `2024-11-05` (negotiation, server side).
  - `initialize` with an unrecognized/absent version → response advertises the default
    `2025-03-26`.
- **Implement** (`server.go` `dispatch`): parse `req.Params` for `protocolVersion`; echo it when it
  is in the recognized set `{"2025-03-26","2024-11-05"}`, else `"2025-03-26"`. Keep
  `capabilities:{"tools":{}}` and `serverInfo` unchanged.
- **Green:** `go test ./internal/mcp/`.

## Task 6 — Full suite + real-seam verification

- `go build ./...`, `go vet ./...`, `go test ./...` all green.
- Per the `verify-tool-loop-at-gateway-seam` learning, add/confirm at least one test that exercises
  the handshake through the real `cmd/fuse` MCP wiring (the `mcps --live` path drives a real
  `mcp.NewManager`, satisfying this) rather than only the in-package harness.

## Notes / non-goals (from spec)

- No gated feature (streaming/batch/subscriptions/`$/progress`) is implemented — only the
  handshake, storage, `Supports` accessor, and surfaces. #20/#21 call `Supports(...)` later.
- Client advertises empty capabilities `{}`; fails-open throughout; no cross-session persistence;
  no rejection on version/capability mismatch.
