---
id: 19
slug: mcp-capability-negotiation
title: MCP capability negotiation — structured init handshake
status: proposed
priority: high
type: feat
created: 2026-08-06
updated: 2026-08-06
depends_on: [3, 7]
related: [18, 20]
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

The MCP specification v2025-03-26 mandates **capability negotiation** during the initialization handshake. Clients and servers advertise fine-grained capability flags (`streaming`, `annotations`, `logging`, `batch`, `authz`, `resources`, `prompts`) in the `initialize` request/response. The current fuse MCP implementation does not advertise or inspect capabilities — the init exchange is minimal (protocol version only). This means fuse cannot (a) discover whether a server supports streaming before attempting a streaming call, (b) negotiate optional features like batch requests or resource subscriptions, or (c) advertise its own capabilities so the server can tailor its behavior. Capability negotiation is also a prerequisite for several follow-on features (Streamable HTTP, resource subscriptions, `$/progress` notifications).

## What changes

- **Structured `ClientCapabilities` and `ServerCapabilities` types** in `internal/mcp/` matching the 2025-03-26 spec shape — maps of capability name to optional detail objects (e.g. `{"streaming": {}, "batch": {"maxSize": 10}}`).
- **Init handshake upgrade**: the `initialize` request sends fuse's capability set; the `initialize` response is parsed for the server's capabilities, stored on the `managedServer` struct.
- **Capability-gated dispatch**: each feature (streaming, batch, subscribe) checks the negotiated capability before use, returning a clear error (e.g. "server X does not advertise 'streaming' capability") rather than attempting the call and failing opaquely.
- **Capability surface in `fuse mcps list --live`**: show per-server negotiated capabilities in the status output.
- **MCP server side** (`mcp_server.go`): the fuse MCP server advertises its own capabilities in its init response (tools only, same as today — but explicitly declared rather than absent).

## Out of scope

- Implementing the features the capabilities gate (streaming, batch, subscriptions) — those are separate changes that depend on this one as a prerequisite.
- Persisting capabilities across sessions — re-negotiated on every reconnect.

## Research notes (input for the brainstorm)

The capability map is a `ServerCapabilities` struct with optional fields: `streaming`, `batch` (with optional `maxSize`), `annotations`, `logging`, `authz` (with supported schemes), `resources` (with optional `subscribe` and `listChanged` booleans), `prompts` (with optional `listChanged`). Each is a JSON object or `true`. The client sends `ClientCapabilities` mirroring the same shape for what it supports. Neither side must reject a connection over unsupported capabilities — they simply don't use features the other side doesn't advertise. This is a "fails open" design: missing capability keys are treated as "not supported." The challenge is backward compatibility with pre-2025-03-26 servers, which return a minimal init response without capability fields — fuse must tolerate that and assume no optional capabilities.
