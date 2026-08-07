---
id: 11
slug: streamable-http-mcp-transport-request-scoped
title: Streamable HTTP MCP transport is request-scoped with in-band session ownership
status: Accepted
date: 2026-08-07
supersedes: []
reverses: []
relates_to: [10]
change: 18
---

## Context

fuse's MCP client layer (post change 0019) exposes a transport-agnostic
`mcpConn` interface (`call` / `notify` / `stop`). The manager's
`handshakeAndDiscover` drives the mandatory MCP handshake (`initialize` →
`notifications/initialized` → `tools/list`) through that interface,
transport-agnostically — the handshake posture itself is fixed by ADR-0010.

The existing HTTP/SSE transport (`httpClient`, change 0007) is built around a
**persistent** background SSE read pump plus a `pending[id]` map: it opens a
long-lived `GET /sse` stream at connect, and every response arrives
asynchronously on that stream and is fanned to the waiting caller by id.

MCP v2025-03-26 Streamable HTTP is shaped differently: a single endpoint, each
JSON-RPC request is its own `POST`, and the server answers that `POST` *either*
synchronously (`application/json`) *or* with a short per-request
`text/event-stream`. Sessions are managed via an `Mcp-Session-Id` response
header the client echoes on later requests (and `DELETE`s to end). The
persistent-pump/pending-map shape is a poor fit for this protocol: there is no
long-lived stream to pump, and responses are already correlated to their
originating `POST`.

## Decision

The new `StreamableHTTPClient` is implemented **request-scoped**: no persistent
read pump and no pending-response map. Each `call()` owns its own response —
either a synchronous JSON body, or a short-lived per-call SSE pump it drains to
completion — governed by that call's own context deadline.

It **owns its session lifecycle in-band**:

- It captures `Mcp-Session-Id` from response headers during the
  manager-driven handshake; it does **not** own a separate `initialize`
  exchange (the manager drives the handshake per ADR-0010).
- It echoes the captured session id plus `MCP-Protocol-Version` on subsequent
  requests.
- It `DELETE`s the session on `stop()`.
- On a `404` (expired session) it performs a **client-driven inline
  re-initialize** before retrying the request.

Inbound id-less / foreign stream frames route to a single named
`handleServerFrame` seam (log-and-discard today) so changes 0020/0021 replace
that one method rather than reworking a pump.

## Consequences

- **Simpler** than the HTTP/SSE transport — no goroutine/pump lifecycle, no
  shared `pending` map — so fewer concurrency hazards; per-call context governs
  deadlines.
- **Two coexisting HTTP-family transports** in the same package now use
  structurally different models (persistent-pump vs request-scoped). A
  maintainer must not assume symmetry; the divergence is deliberate, driven by
  the protocol shape, not an oversight.
- **Session refresh (404 → re-initialize) lives in the client**, a small
  duplication of the `initialize` payload, because the manager's handshake is
  one-shot at startup and has no mid-session reconnect concept. This keeps full
  session lifecycle without adding manager-level reconnect orchestration (which
  no fuse transport has today).
- **Sets precedent**: the server-initiated notification stream (0020/0021) hooks
  the `handleServerFrame` seam rather than reworking a pump; a future WebSocket
  transport (0022) is free to choose its own model rather than inherit either
  existing one.
