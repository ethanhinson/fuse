<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0019 — MCP capability negotiation — structured init handshake](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0019-mcp-capability-negotiation.md)**
<!-- docket:backlink:end -->

# MCP capability negotiation — structured init handshake

**Change:** [#0019](../../changes/active/0019-mcp-capability-negotiation.md) · **Status:** design · **Date:** 2026-08-06

## Problem

MCP requires an `initialize` request/response as the mandatory first exchange of every
connection, followed by an `initialized` notification from the client. As of the
v2025-03-26 spec, that handshake also carries **capability negotiation**: each side
advertises fine-grained capability flags (`resources.subscribe`, `streaming`, `logging`,
`prompts`, …) so neither attempts a feature the other has not declared.

fuse's MCP client does **neither** today. `startAndDiscover` (`internal/mcp/manager.go`)
opens the transport and jumps straight to `tools/list` — it never sends `initialize` and
never sends `initialized`. This is a latent spec-compliance gap (strict servers may reject
`tools/list` before a handshake), and it leaves fuse with no idea what optional features a
server supports. Capability negotiation is a hard prerequisite for changes #20 (progress
streaming) and #21 (resource subscriptions), both of which must gate on a negotiated
capability before use.

This change introduces the full client-side handshake and the capability storage +
accessor those follow-on changes consume. It deliberately does **not** implement any of
the gated features themselves (streaming, batch, subscriptions) — those are #18/#20/#21.

## Design decisions (settled in brainstorm)

1. **Lean footprint — handshake + storage + one accessor.** Store the server's advertised
   capabilities as a permissive raw map, exposed through a single `Supports(key)` method.
   No typed per-feature fields and no feature-gating call sites are added now, because none
   of the consuming features exist yet — adding them would be dead code. #20/#21 add their
   own `Supports("…")` checks when they land.
2. **Protocol version: advertise 2025-03-26, tolerate any echo.** The client sends
   `protocolVersion: "2025-03-26"`; it stores whatever version the server echoes and never
   rejects on mismatch (MCP's fails-open norm). fuse's own server is bumped from the
   hardcoded `2024-11-05` to `2025-03-26`, echoing the client's requested version when it
   recognizes it.
3. **Init failure hard-fails the connection.** If `initialize` errors or times out, the
   connect fails and the server is skipped with `connErr` set — exactly as `tools/list`
   behaves today. A server that cannot complete the mandatory handshake is broken.
4. **Capabilities fail open.** A missing capability key means "not supported." Pre-2025-03-26
   servers that return a minimal init response (no `capabilities`, or a sparse one) are
   fully tolerated — they simply support no optional capabilities.

## What changes

### 1. Client handshake in `startAndDiscover` (`internal/mcp/manager.go`)

Insert, before the existing `tools/list` call and inside the same timeout budget:

```
init, err := client.call(ctx, "initialize", initializeParams())
if err != nil {
    client.stop()
    return nil, nil, fmt.Errorf("initialize: %w", err)   // hard-fail, connErr set by Add()
}
caps, protoVer := parseInitializeResult(init)
client.notify(ctx, "notifications/initialized", nil)      // fire-and-forget, no id
// ... existing tools/list, now guaranteed post-handshake
```

`initializeParams()` returns fuse's `initialize` params:

```json
{
  "protocolVersion": "2025-03-26",
  "capabilities": {},
  "clientInfo": {"name": "fuse", "version": "1.0.0"}
}
```

fuse's client advertises an **empty** capability object for now — it consumes server
capabilities but exposes none of the optional client-side capabilities (`roots`,
`sampling`, …) yet. Follow-on changes populate this as they add client features.

### 2. `notify` on the transport interface (`internal/mcp/conn.go`)

The `initialized` message is a JSON-RPC **notification** — no `id`, no response. The
current `call()` always allocates an id and blocks on a reply, so a notification path is
needed:

```go
type mcpConn interface {
    call(ctx context.Context, method string, params any) (json.RawMessage, error)
    notify(ctx context.Context, method string, params any) error   // NEW: no id, no wait
    stop()
}
```

- `StdioClient.notify` encodes a frame with no `id` field and does not register a pending
  channel or wait.
- `httpClient.notify` POSTs the notification frame; a notification expects no response body.

A notification send failure is logged but non-fatal — `initialized` is advisory and the
connection is already established.

### 3. `ServerCapabilities` value + `Supports` accessor (new, `internal/mcp/`)

```go
// ServerCapabilities is the server's advertised capability set, stored verbatim.
type ServerCapabilities struct {
    raw map[string]json.RawMessage // top-level capability objects, e.g. "resources", "logging"
}

// Supports reports whether the server advertises a capability.
//   Supports("logging")            -> top-level key present
//   Supports("resources.subscribe")-> "resources" present AND its "subscribe" field == true
// Unknown or missing keys return false (fails open).
func (c ServerCapabilities) Supports(key string) bool
```

- A bare key (`"logging"`, `"streaming"`) is supported when the top-level key is present
  with any non-null value.
- A dotted key (`"resources.subscribe"`, `"resources.listChanged"`) is supported when the
  top-level object is present **and** the named nested field is the boolean `true`.
- `parseInitializeResult(raw)` unmarshals `result.capabilities` into `raw` and returns
  `result.protocolVersion`. A malformed or absent `capabilities` yields an empty (supports
  nothing) set, never an error — fails open.

### 4. Storage on `managedServer` (`internal/mcp/manager.go`)

```go
type managedServer struct {
    cfg      config.MCPServerConfig
    conn     mcpConn
    tools    []*MCPTool
    caps     ServerCapabilities // NEW: negotiated server capabilities
    protoVer string             // NEW: protocol version the server echoed
    connErr  string
}
```

`startAndDiscover` returns `caps`/`protoVer` alongside the conn and tools; `Add` stores them.

### 5. `--live` status surface (`internal/mcp/manager.go` + `fuse mcps list`)

- `ServerStatus` gains `ProtocolVersion string` and `Capabilities []string` (the sorted set
  of supported capability keys, e.g. `["logging", "resources.subscribe"]`).
- `Manager.Status()` populates them from `ms.protoVer` / `ms.caps`.
- `fuse mcps list --live` renders a per-server line for the negotiated protocol version and
  capabilities. Servers advertising none show an explicit "none" rather than a blank.

### 6. fuse's server side (`internal/mcp/server.go`)

- Bump the advertised `protocolVersion` from `"2024-11-05"` to `"2025-03-26"`; when the
  incoming `initialize` request carries a recognized `protocolVersion`, echo that value
  back instead (version negotiation, server side).
- Keep the advertised capabilities as `{"tools": {}}` — fuse's server still exposes only
  tools; the change makes the declaration explicit and version-correct, nothing more.

## Out of scope

- Implementing any gated feature (streaming, batch, resource subscriptions, `$/progress`) —
  those are #18, #20, #21 and call `Supports(...)` themselves.
- Advertising optional **client** capabilities (`roots`, `sampling`) — the client sends
  `{}` until a feature needs one.
- Persisting capabilities across sessions — they are re-negotiated on every reconnect.
- Rejecting connections over version or capability mismatch — fails-open throughout.

## Testing strategy

- **Unit — `Supports` semantics:** table test over bare keys, dotted keys, present-but-false
  nested fields, missing top-level keys, and a malformed/empty capability map (all fail
  open to `false`).
- **Unit — `parseInitializeResult`:** full 2025-03-26 init result, a minimal
  (pre-2025-03-26) result with no `capabilities`, and a garbage `capabilities` value →
  empty set + echoed protocol version, no error.
- **Integration — handshake ordering:** a mock stdio server that records the method
  sequence asserts `initialize` precedes `tools/list` and that `notifications/initialized`
  (id-less) is sent between them.
- **Integration — init hard-fail:** a mock server that returns a JSON-RPC error to
  `initialize` → `Add` records `connErr`, registers no tools, server reported disconnected.
- **Integration — fuse server negotiation:** drive fuse's own server (`server.go`) with an
  `initialize` carrying `2025-03-26` and assert the echoed version + `{"tools":{}}`
  capabilities; drive it with `2024-11-05` and assert it echoes that older version back.
- **Verify at the real seam:** per the `verify-tool-loop-at-gateway-seam` learning, exercise
  the handshake through the real `cmd/fuse` MCP wiring against a scripted mock server, not
  only the in-package harness.

## Risks & mitigations

- **Quirky servers that answered `tools/list` without a handshake now hard-fail.** This is
  the intended compliance tightening; the clear `initialize:` error message names the
  server so a misbehaving one is diagnosable. If a real-world server proves to need the old
  lenient path, that is a follow-up config knob, not a reason to weaken the default.
- **`notify` sending an empty body / wrong frame.** Covered by the id-less-frame assertion
  in the ordering integration test; the http path reuses the existing request plumbing.
- **`Supports` dotted-key semantics drifting from what #21 expects.** The accessor contract
  is pinned by unit tests here so #21 can rely on `Supports("resources.subscribe")` exactly.
