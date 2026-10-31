<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0089 — Static-credential MCP servers authenticate, and every loop reports its MCP servers](../../changes/active/0089-static-credential-mcp-servers-authenticate-and-every-loop-re.md)**
<!-- docket:backlink:end -->

# Static-credential MCP auth and MCP server status events

## Problem

- `internal/mcp/oauth.go` `GetAccessToken` (around 308-328) has no `static` case and returns `unknown auth type "static"`. `manager.go` (around 152-156) logs and skips the server. Everywhere else (`harnessconfig.go` around 68, `overlay.go` around 127, `mcp_target.go` around 17, `tool_identity.go` around 311-316), static is treated as a bearer-equivalent credential, and GetCapabilities advertises it.
- `Manager.Status()` (around 264-290) already knows `Connected`, `Error` and `Tools` for each server, but none of it reaches the event stream; `internal/event` has no MCP kind.

## Design

- `GetAccessToken`: `case "bearer", "static": return cfg.ClientSecret`.
- Golden test: for every entry of `SpecMCPAuthKinds()`, a spec MCP server using that kind against a test MCP server connects and lists tools (ADR-0062: what is advertised must be backed by enforcement).
- New durable event kind `mcp.server`, emitted once per configured MCP server (operator and spec) in the hosted BuildAgent path (`loop_server.go` around 329, which holds the store and the loop's manager), right after `loop.harness`. Payload: `{name, transport, auth_type, connected, error, tools[]}`. Tool names are the names exposed to the model. Never include secrets.
- Keep skip-and-continue: an unreachable server does not fail the loop.
- Add the kind to the event catalog and the public SDK event types, and regenerate the stubs if the proto enumerates kinds.

## Testing

- Hosted loop with a static-auth spec MCP server: the test server receives `Authorization: Bearer <resolved secret>`; the stream has `mcp.server{connected: true, tools: [...]}`; the model's tool list includes those tools.
- Unreachable URL: `mcp.server{connected: false, error: <non-empty>}`, and the loop still completes a turn.
- The golden test over every advertised auth kind.
