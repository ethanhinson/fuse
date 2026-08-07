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
related: [18, 20, 21]
discovered_from: []
adrs: []
spec: docs/superpowers/specs/2026-08-06-mcp-capability-negotiation-design.md
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
| Spec | [2026-08-06-mcp-capability-negotiation-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-06-mcp-capability-negotiation-design.md) |
<!-- docket:artifacts:end -->

## Why

MCP requires an `initialize` request/response as the mandatory first exchange of every connection, followed by an `initialized` notification from the client; the v2025-03-26 spec adds **capability negotiation** to that handshake so each side advertises fine-grained flags (`resources.subscribe`, `streaming`, `logging`, `prompts`, …) and neither attempts a feature the other has not declared.

fuse's MCP client does **neither** today — `startAndDiscover` opens the transport and jumps straight to `tools/list`, never sending `initialize` or `initialized`. That is a latent compliance gap (strict servers may reject `tools/list` before a handshake) and leaves fuse blind to what a server supports. Capability negotiation is a hard prerequisite for #20 (progress streaming) and #21 (resource subscriptions), which must gate on a negotiated capability before use.

## What changes

- **Client handshake**: `startAndDiscover` sends `initialize` (advertising `protocolVersion: 2025-03-26`, empty client capabilities, fuse `clientInfo`) before `tools/list`, then fires an id-less `notifications/initialized`. A failed `initialize` **hard-fails** the connect (server skipped with `connErr`), matching `tools/list`.
- **`notify` on the `mcpConn` interface**: an id-less, no-wait send path for notifications, implemented by both the stdio and http transports.
- **`ServerCapabilities` + `Supports(key)` accessor**: the server's capabilities are stored verbatim as a permissive raw map; `Supports("logging")` (top-level key present) and `Supports("resources.subscribe")` (nested boolean `true`) fail open to `false` on anything missing or malformed. No typed per-feature fields and no gating call sites are added now — #20/#21 call `Supports(...)` when they land.
- **Storage on `managedServer`**: negotiated `caps` and echoed `protoVer`, surfaced through `ServerStatus` and shown per-server in `fuse mcps list --live`.
- **fuse server side** (`server.go`): bump the advertised `protocolVersion` `2024-11-05`→`2025-03-26`, echoing a recognized client-requested version; keep capabilities `{"tools": {}}`, now explicitly version-correct.

## Out of scope

- Implementing any gated feature (streaming, batch, resource subscriptions, `$/progress`) — those are #18/#20/#21 and call `Supports(...)` themselves.
- Advertising optional **client** capabilities (`roots`, `sampling`) — the client sends `{}` until a feature needs one.
- Persisting capabilities across sessions — re-negotiated on every reconnect.
- Rejecting connections over version or capability mismatch — fails-open throughout.

Full design, type sketch, and testing strategy in the linked spec.
