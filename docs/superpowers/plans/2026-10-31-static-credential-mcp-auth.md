<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0089 — Static-credential MCP servers authenticate, and every loop reports its MCP servers](../../changes/archive/2026-10-31-0089-static-credential-mcp-servers-authenticate-and-every-loop-re.md)**
<!-- docket:backlink:end -->

# Static-credential MCP auth and mcp.server events Implementation Plan

**Goal:** static MCP auth connects; every loop records one mcp.server event per MCP server.

- [x] Task 1: GetAccessToken treats "static" like "bearer" (internal/mcp/oauth.go).
- [x] Task 2: Add event.KindMCPServer and MCPServerPayload (internal/event/event.go) with catalog test.
- [x] Task 3: Emit mcp.server per server from BuildAgent in cmd/fuse/loop_server.go, after loop.harness.
- [x] Task 4: Golden test over SpecMCPAuthKinds against a fake streamable MCP server, plus unreachable-server test (cmd/fuse/loop_harness_test.go).
- [x] Task 5: Document the event in sdk/README.md.
