---
slug: websocket-read-errors-are-not-closeerror
hook: "A WebSocket transport must treat EVERY post-handshake read error as a clean shutdown — abnormal peer close (TCP RST / no close frame) surfaces as raw io.ErrUnexpectedEOF or *net.OpError, and a self-Close race as net.ErrClosed, NEVER as a websocket.CloseError; matching only CloseError makes a routine client drop look like a server error, and a WS conn cannot resume after a read error anyway so mapping all of them to io.EOF is correct."
topics: [websocket, transport, error-handling, networking, go]
changes: [48]
created: 2026-08-11
updated: 2026-08-11
promotion_state: candidate
promoted_to:
---

## Apply

When you read requests off a `coder/websocket` (or any WS) connection in a server read
pump, do not assume an orderly client disconnect arrives as a `websocket.CloseError` you
can pattern-match. The abnormal cases — a TCP reset, a peer that vanishes without sending a
close frame — surface as a **raw `io.ErrUnexpectedEOF` or `*net.OpError`**, and a
self-initiated `Close`/`CloseNow` racing an in-flight read surfaces as **`net.ErrClosed`**.
None of these is a `CloseError`. If the read loop only classifies `CloseError` as "client
went away," every routine drop is reported up as a server-side error, and callers that log
or alert on server errors get noise on the normal path.

The invariant that makes the fix simple: **a WS connection cannot resume after any read
error** — there is no half-open recovery. So the correct handling is not to enumerate error
classes and branch, but to map **every** post-handshake read error to a clean `io.EOF` and
tear the connection down once. Keep the teardown idempotent and leak-free: the observe/read
pump's `defer cancel()` must fire on any return path so no goroutine or subscription is
stranded whether the close was abnormal, mid-session cancel, or orderly.

**How to test:** drive the abnormal path explicitly — call `conn.CloseNow()` while a read is
pending (asserts the self-close race maps clean) and cancel a live session mid-stream
(asserts mid-session cancel maps clean). A test that only exercises an orderly close frame
will pass while the abnormal-close bug ships. Related: [[replay-live-handoff-dedup-at-watermark]]
(the same loopserver observe seam), [[mcp-read-pumps-drop-inbound-notifications]] (read-pump
error classification).

## Provenance

2026-08-11 — the WS transport (`internal/loopserver`, `net.go`) in fuse's binding #3
(#48, PR #51). The whole-branch review flagged as a Major: `readRequest` under-matched the
error classes `coder/websocket` returns on abnormal peer close, so a routine client drop
surfaced as a server error. Fixed by mapping every post-handshake read error to `io.EOF`,
covered by `TestWSAbnormalCloseIsCleanShutdown` (drives `conn.CloseNow()`) and
`TestWSMidSessionCancelIsCleanShutdown`. No goroutine/subscription leak either way — the
observe pump's `defer cancel()` fires on any return.
