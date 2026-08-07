<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0018 — Streamable HTTP transport for MCP (v2025-03-26)](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0018-mcp-streamable-http.md)**
<!-- docket:backlink:end -->

# Results — Streamable HTTP transport for MCP (change 0018)

**Date:** 2026-08-07 · **Branch:** `feat/mcp-streamable-http`

## What shipped

A `StreamableHTTPClient` (`internal/mcp/streamable_http_client.go`) implementing the MCP
v2025-03-26 Streamable HTTP transport, selected by `transport: streamable-http` in `dial()`.
Request-scoped (no persistent read pump); branches on the response `Content-Type`
(`application/json` synchronous vs. a per-call `text/event-stream` pump); captures/echoes
`Mcp-Session-Id` + `MCP-Protocol-Version`; `DELETE`s the session on `stop()`; refreshes on
`401` (token) and `404` (inline re-initialize) with the request body rewound; routes
inbound id-less/foreign stream frames to a named `handleServerFrame` seam; resumes a broken
stream via `Last-Event-Id` (bounded). See the linked spec + ADR.

## Reconcile note

The design was reconciled against `origin/main` after change **0019** (capability
negotiation) merged: `mcpConn` grew a third method (`notify`), the transport switch moved to
`dial()`, and the handshake is now driven transport-agnostically by `handshakeAndDiscover`.
The client therefore does **not** own an `initialize` exchange — it captures the session id
in-band. Details in the change's `## Reconcile log`.

## Automated verification

- `go test ./...` — full hermetic suite green.
- `internal/mcp/streamable_http_client_test.go` — httptest doubles covering: `dial()`
  routing, synchronous handshake + session/proto echo, `notify` (no id), streaming response
  + notification seam, **512 KiB single-frame** (pump not truncated), session lifecycle +
  `DELETE`, stateless server (no session/no DELETE), **401 refresh + body rewind**
  (regression guard), **404 → inline re-initialize**, **Last-Event-Id resumability**, and
  `RPCError` mapping.

## Live verification (the "test the TUI with remote http servers" directive)

Verified end-to-end against the reputable public **DeepWiki** Streamable HTTP MCP server
(`https://mcp.deepwiki.com/mcp`, no auth, stateless):

1. `internal/mcp/streamable_http_integration_test.go` (`//go:build integration`,
   network-gated skip) — real handshake + discovery **through the SSE response pump** + a
   live `tools/call read_wiki_structure` round-trip. Result:
   `proto=2025-03-26, 3 tools, caps.tools=true`, tool call returns a content block.
2. Real binary via the actual manager wiring:
   ```
   $ fuse mcps list --live
   NAME       TRANSPORT        AUTH   STATUS   TOOLS   PROTO        CAPS
   deepwiki   streamable-http  none   ok       3       2025-03-26   experimental,prompts,resources,resources.listChanged,tools
   ```

## Merge-gate manual check (recommended)

For an interactive TUI confirmation before merge, add to `~/.fuse/config.yml`:

```yaml
mcp_servers:
  - name: deepwiki
    transport: streamable-http
    url: https://mcp.deepwiki.com/mcp
```

Launch `fuse`, confirm the `deepwiki` tools are available, and ask a question that exercises
`ask_question`/`read_wiki_structure` against a public repo. (Context7
`https://mcp.context7.com/mcp` is a second no-auth option.)

## Notable decisions / deviations

- Distinct `transport: "streamable-http"` value rather than a `streamable: true` boolean flag
  (matches the existing string switch; avoids an ambiguous `http`+`streamable` state).
- Request-scoped, in-band session ownership — recorded as an ADR (see the change's `adrs:`).
- SSE pump uses a `bufio.Reader` line loop (not `bufio.Scanner`) so a large single `data:`
  frame is not silently truncated — a hardening over the older HTTP/SSE pump.

## Follow-ups (not in scope here)

- Server-initiated `GET` notification stream → changes **0020** (`$/progress`) / **0021**
  (resource subscriptions), which replace the `handleServerFrame` seam.
- WebSocket transport → change **0022**.
