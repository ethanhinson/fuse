---
slug: httptest-defer-close-before-tcleanup-deadlock
hook: "defer runs before t.Cleanup (LIFO across two mechanisms): defer srv.Close() on an httptest server with a live goroutine (SSE pump) deadlocks against a t.Cleanup(client.stop) that would end it — register BOTH teardowns with t.Cleanup so the client stops first"
topics: [testing, concurrency, httptest, mcp]
changes: [52, 59]
created: 2026-08-11
updated: 2026-08-11
promotion_state: candidate
promoted_to:
---

## Apply

When a test double is an `httptest.Server` that spawns a long-lived server-side goroutine (an SSE
pump, a streaming handler, a websocket loop), do **not** mix `defer srv.Close()` with a
`t.Cleanup(...)` that stops the client feeding it. Go runs **all `defer`s before any
`t.Cleanup`**, and each pool is LIFO within itself — so `defer srv.Close()` fires first and blocks
forever waiting on the still-live goroutine, while the `t.Cleanup(client.stop)` that would have
released it never gets to run. The symptom is a whole-package test timeout (here: 600s), not an
obvious hang in one test.

Fix: register **both** shutdowns with `t.Cleanup`, ordered so the client stops before the server
closes (rely on `t.Cleanup`'s LIFO — register `srv.Close` first, then `client.stop`, so the client
stops first). Keeping teardown in a single mechanism restores predictable ordering.

## War story
- 2026-08-11 (#52, PR #55) — MCP egress identity-propagation tests used a capturing SSE double
  registering `t.Cleanup(client.stop)` while the tests used `defer srv.Close()`. `defer` runs
  before `t.Cleanup`, so `httptest.Server.Close()` blocked on the live SSE goroutine and the
  packages timed out at 600s. Moving server shutdown into `t.Cleanup` (LIFO → client stops first)
  cleared the deadlock. Surfaced during the change's whole-branch review + suite run, not by any
  single failing assertion.
- 2026-08-11 (#59, PR #56) — the loop-server MCP acceptance lane hit the identical deadlock: the
  stub/rentals SSE servers loop on `r.Context().Done()`, so `httptest.Server.Close()` blocked until
  fuse's SSE read-pump disconnected. Fixed by teardown ordering so the MCP manager/client closes
  before the server (`LoopTeardown` before the server's cleanup, mirroring `egress_test.go`'s
  `capturingSSEServer`). Same 600s `cmd/fuse` package timeout as the #52 hit — the finding fired
  exactly as written, a second time on a different binding.
