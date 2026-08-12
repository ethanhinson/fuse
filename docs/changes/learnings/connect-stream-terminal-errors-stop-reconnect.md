---
slug: connect-stream-terminal-errors-stop-reconnect
hook: "A reconnecting Connect stream must classify terminal protocol codes before retrying — auth, permission, missing-loop, and finished-loop errors must close and surface a typed error; retrying them hot-loops forever, while other stream ends remain transient."
topics: [typescript, connect, streaming, error-handling, retries, sdk]
changes: [56]
created: 2026-08-12
updated: 2026-08-12
promotion_state: candidate
promoted_to:
---

## Apply

An SDK that reconnects a long-lived Connect stream must not treat every caught error as a
transient network drop. Classify the protocol code at the SDK boundary: authentication and
authorization failures, a missing loop, and a finished loop are terminal for that observation and
must stop retrying, transition the lifecycle to `closed`, and surface a typed error carrying the
code. Other stream failures remain transient and may reconnect from the sequence watermark.

The terminal set belongs at the client boundary, where the application can render a useful
closed-session state. A blanket `catch { reconnect() }` otherwise turns a bad token or finished
session into an invisible hot loop that hammers the server and gives the browser no recovery path.
Test every terminal code for one surfaced error and no reconnect cycle, plus a separate transient
drop that does reconnect.

## War story

- 2026-08-12 (#56, PR #57) — Wander's misconfigured-token and finished-loop paths exposed that the
  TS SDK swallowed every Connect error and reopened `Observe` forever. The fix classified
  `Unauthenticated`, `PermissionDenied`, `NotFound`, and `FailedPrecondition` as terminal,
  surfaced `FuseTerminalError`, fired `closed`, and retained reconnect behavior for transient
  stream ends. Per-code tests made the no-hot-loop invariant explicit.
