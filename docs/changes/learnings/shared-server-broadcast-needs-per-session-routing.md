---
name: shared-server-broadcast-needs-per-session-routing
slug: shared-server-broadcast-needs-per-session-routing
title: A response stream that broadcasts to every connection is latent while single-client but becomes cross-principal leakage the moment a shared server gets a second principal — route each response to its own session
hook: "A server that broadcasts every response to ALL connected streams (and never prunes them) is harmless while it is single-client/test-only, but the moment it becomes a long-lived shared server with per-principal connections it leaks one principal's response onto another's stream — and if request ids are numbered per-client they collide, so a call can resolve with a STRANGER'S data. Give each connection a session id, advertise it on the endpoint, route each response to only that session, and prune on disconnect. Sequential-switch tests never catch it (no pending id to corrupt); reading does."
promotion_state: candidate
changes: [60]
created: 2026-08-16
updated: 2026-08-16
topics: [go, sse, mcp, multi-tenant, isolation, security, routing, testing]
---

## Apply

When a server holds a set of live response streams and answers requests over them, the lazy shape is
to **broadcast** every response frame to every registered connection:

```go
// LEAKY: fan every response out to ALL connected streams
conns := append([]chan string(nil), s.sseConns...)   // and sseConns is never pruned on disconnect
for _, ch := range conns { ch <- frame }
```

This is *invisible* while the server is single-client or test-only: one `httptest` server, one client,
one stream — the broadcast has exactly one recipient, so it reads as point-to-point. It stays green
through every unit test and even through a sequential acceptance lane.

It becomes a **cross-principal isolation breach** the instant the server turns into a long-lived,
shared process that different principals connect to (change 0060: `cmd/rentals-mcp` went from a
test-only `rentals.Server` to a standalone server that `cmd/fuse` opens a *per-loop* connection to).
Now each principal has its own stream, the broadcast copies principal B's response onto principal A's
stream too — and if the client library numbers request ids **per-client** (`id := counter.Add(1)`),
the two id spaces collide by construction, so A's client can resolve its pending call with **B's
response body**. For a favorites/rentals MCP server that is the exact inverse of the per-principal
isolation the feature exists to demonstrate.

**Rule:** on any server that multiplexes responses over long-lived connections shared across
principals, responses are **addressed, not broadcast**:

- Give each connection a **session id** at accept time.
- Advertise it on the endpoint the client posts back to (`/messages?sessionId=…`), so a request
  carries the session that owns it.
- Route each response to **only** that session's channel; an absent/unknown session delivers
  **nowhere** (fail-closed), never to everyone.
- **Prune** the connection from the registry in a `defer` when the stream handler returns — an
  unpruned set grows without bound and keeps a departed principal's channel live.

## Why the tests missed it

The browser lane (`TestWanderBrowserUserSwitchIsolatesFavorites`) switches users **sequentially**:
the previous principal's loop has no in-flight request when the next one starts, so the stray
broadcast frame arrives with no pending id to corrupt and is silently dropped. A green suite proved
the *sequential* isolation and said nothing about *concurrent* streams. The defect was found by
**reading the fan-out**, not by a test — the general caution in [[smoke-over-fake-backend-proves-wire-not-system]]:
a passing lane bounds what it exercised, not what the code does. A regression test for this must open
**two** concurrent principal streams and assert a frame addressed to one never lands on the other.

## Related

- [[fanout-send-snapshot-identity-not-pointer]] — a *different* fan-out trap (send-on-closed-channel
  race from carrying live pointers across the lock). That one is a concurrency panic; this one is a
  routing/addressing leak. A shared response server can have both.
- [[cache-over-tenant-scoped-source-reassert-key-on-hit]] — the reciprocal on the *write* side:
  key every mutation on the token-derived principal, never on a client-supplied argument.
- [[race-invisible-to-race-detector-without-concurrent-test]] — same shape of blind spot: a hazard
  that only a genuinely concurrent test can surface.
