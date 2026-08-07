---
id: 10
slug: mcp-client-requires-init-handshake-fails-open-capabilities
title: MCP client hard-fails on the initialize handshake but fails open on capability content
status: Accepted
date: 2026-08-07
supersedes: []
reverses: []
relates_to: []
change: 19
---

## Context

Before change 0019, fuse's MCP client never performed the JSON-RPC `initialize`
handshake at all — `startAndDiscover` opened the transport and immediately called
`tools/list`. MCP requires `initialize` (and a following `initialized`
notification) as the mandatory first exchange of every connection, and the
v2025-03-26 spec layers **capability negotiation** onto that handshake. Adding the
handshake forces two independent failure-posture decisions that pull in opposite
directions:

1. **The handshake as a whole** — what happens when `initialize` errors or times
   out? Some servers today tolerate `tools/list` with no prior handshake, so
   fuse's non-compliant shortcut happened to work against them.
2. **The capability *content*** — pre-2025-03-26 servers return a minimal
   `initialize` result with no `capabilities` field (or a sparse one), and the
   MCP norm is that neither side rejects a connection over unsupported
   capabilities.

Treating these two with a single posture would be wrong in one direction or the
other: fail-hard everywhere drops every older server over a missing capability
key; fail-open everywhere masks a genuinely broken server that cannot complete
the mandatory handshake.

## Decision

Split the posture along the handshake/content seam:

- **The `initialize` call hard-fails the connection.** If it errors or times out,
  `startAndDiscover` stops the transport and returns an error; the server is
  skipped with `connErr` set and registers no tools — exactly as a failed
  `tools/list` already behaves. A server that cannot complete the mandatory
  handshake is treated as broken, not tolerated. This deliberately drops the
  quirky servers that previously worked only because fuse skipped the handshake.
- **Capability content fails open.** `parseInitializeResult` never returns an
  error: a missing, sparse, or malformed `capabilities` block yields an empty
  set. Capabilities are stored verbatim as a permissive raw map behind a single
  `Supports(key)` accessor (bare and dotted lookups), which returns `false` for
  anything absent or unparseable. No typed per-capability fields are introduced
  until a consuming feature needs one. The `initialized` notification is likewise
  advisory — a send failure is logged, never fatal.

fuse's client advertises `protocolVersion: 2025-03-26` and tolerates whatever
version the server echoes (no rejection on mismatch).

## Consequences

- **Enables** capability-gated features (#20 progress streaming, #21 resource
  subscriptions) to rely on a stable `Supports("resources.subscribe")`-style
  contract, and makes fuse spec-compliant for strict servers that reject
  pre-handshake `tools/list`.
- **Costs** backward compatibility with non-compliant servers that answered
  `tools/list` without an `initialize` handshake — they now fail to connect, with
  a clear `initialize: …` error naming the server. If a real-world server proves
  to need the old lenient path, that is a future opt-in config knob, not a reason
  to weaken the default.
- **Gives up** early type safety on capabilities: the permissive raw-map storage
  trades compile-time capability typing for a lean surface that consuming changes
  extend as needed. The `Supports` contract is pinned by unit tests so downstream
  changes can depend on it.
