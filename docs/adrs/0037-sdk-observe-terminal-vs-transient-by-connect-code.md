---
id: 37
slug: sdk-observe-terminal-vs-transient-by-connect-code
title: Client SDK observe classifies terminal vs transient stream failures by Connect code
status: Accepted
date: 2026-08-12
supersedes: []
reverses: []
relates_to: [33, 34, 35]
change: 56
---

## Context

The `@fuse/sdk` (and Go `sdk/fuse`) `observe(loopId, fromSeq)` reconnect loop transparently
re-opens `Observe(fromSeq=last)` on any stream end/error — the browser analogue of the
learning websocket-read-errors-are-not-closeerror: "a post-handshake read/close is a clean
resume signal, not a fault." But change 0050 shipped this with a bare `catch {}` that
swallowed EVERY error. A TERMINAL failure the server can never recover — auth rejected, a
spoofed / cross-owner tenant, an unknown loop, or a finished loop — therefore caused the
client to re-observe FOREVER: an invisible hot-loop hammering the server, with no
app-visible signal. Building Wander (change 0056) surfaced this against a misconfigured
token. The reconnect discipline must distinguish "resume" from "stop" without inferring it
from stream shape.

## Decision

The SDK classifies a stream failure by its **Connect error code**. A fixed TERMINAL set —
`connect.CodeUnauthenticated`, `connect.CodePermissionDenied`, `connect.CodeNotFound`,
`connect.CodeFailedPrecondition` — STOPS the reconnect loop and surfaces a typed terminal
error (TS: an exported `FuseTerminalError` thrown out of the `observe` async-iterator,
carrying the code, plus a `closed` lifecycle transition).

These four codes are exactly what the loop handler
(`internal/loopconnect/handler.go` + `auth.go` + `observe.go`) emits for
auth-reject / tenant-spoof / cross-owner-authz / unknown-loop / finished-loop respectively;
the server maps every OTHER failure (including a mid-stream runtime `Observe` error) to
`connect.CodeInternal`. Everything NOT in the terminal set — a network drop, a clean stream
end, `CodeInternal` / `Unavailable` / `Unknown` / `Canceled` — stays TRANSIENT and
reconnects from the watermark (the 0050 behavior, preserved).

The terminal set is defined by what the SERVER emits as unrecoverable, so it is a **shared
contract every language SDK** (Go, TS, and future Python / mobile) must honor identically;
adding a new terminal server code is a coordinated wire + SDK change.

A non-`ConnectError` throw (a raw network error, or a programming bug) is intentionally
treated as TRANSIENT (retried), since a genuine transport drop is not always a
`ConnectError`.

## Consequences

- **Enables:** an app can render the right affordance (session closed → refresh; auth
  failed) instead of hot-looping or seeing a raw Connect error, and the invisible
  server-hammering hot-loop on a terminal condition stops.
- **Costs / gives up:** the terminal set is now a cross-language coupling point between the
  Connect handler's emitted codes and every SDK's classification — they must not drift. A
  server that starts emitting a currently-transient code for a genuinely-terminal condition
  would (correctly) need both a handler change and an SDK terminal-set update.
- **Trade-off:** treating a non-`ConnectError` throw as transient means a real generator
  defect could hide as an infinite capped-backoff reconnect rather than surfacing.
