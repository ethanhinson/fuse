---
name: race-invisible-to-race-detector-without-concurrent-test
slug: race-invisible-to-race-detector-without-concurrent-test
title: A shared type read on a request path needs its own lock when a live-reload mutates it — and -race won't catch it unless a test drives request + reload concurrently
hook: "Adding a live-reload that mutates a shared type read on a request path? The data race is invisible to -race until a test drives a request concurrently with the reload."
promotion_state: candidate
changes: [21]
created: 2026-08-08
updated: 2026-08-08
topics: [concurrency, race, mcp, live-reload, testing]
---

When you add a config-watch / live-reload that **mutates a shared value in place** (a registry, cache, or catalog) while a request path **reads** that same value, the concurrent access is a data race — even if every prior test passed under `go test -race`. The detector only fires on memory it actually observes being touched from two goroutines *during a run*; if no test drives a request **concurrently with** a reload, the race is real but green.

**Why it's easy to miss:** each side looks innocent alone. The reader (`tools/list`, `tools/call`, `resources/read` dispatch) was correct before the reload existed; the writer (fsnotify → rebuild registry) is new and "only runs on config change." Nothing in the diff reads as concurrent, and the existing `-race` suite stays green because it exercises the two paths in separate tests, never overlapping. The gap is a *missing test*, not a visibly-wrong line.

**Rule:** When a live-reload starts mutating a type that a request/dispatch path reads:
- Put the lock **on the shared type itself**, not on each caller — the invariant belongs to the type (read lock on every getter/lookup, released *before* the value's body runs so a slow call doesn't block reloads; write lock on register/unregister). Prefer in-place reconcile under the lock over a pointer-swap when callers hold no reference across the call.
- Add a `-race` regression test that **drives a request in one goroutine and a reload in another**, in a tight loop. This is the only test shape that turns the invisible race red.

**How to apply:** Any time a diff introduces "rebuild the registry / refresh the cache on config change," trace who *reads* that structure on a hot path. If a request path does, the shared type owns the synchronization, and the proof is a concurrent request-vs-reload test — not the pre-existing suite staying green. Related: [[mutex-test-double-concurrent-provider]] (same detector blind spot, but on a test double rather than the production type).

## War story

### 2026-08-08 — the config-watch race that -race called clean (#21, PR #31)

Change 0021 (MCP resource subscriptions) dogfooded fuse's own MCP server: a new config watch rebuilt the `*tools.Registry` in place on live-reload and pushed `notifications/resources/updated` for `fuse://tools`, while the server dispatched `tools/list`/`tools/call`/`resources/read` against that same registry. A data race on an unsynchronized map/slice that could crash `fuse mcp-server` — but the `-race` suite was **green**, because no test drove a tool request concurrently with a reload. Caught in review, not by CI. Fixed by giving `tools.Registry` its own `sync.RWMutex` (read lock in `Has`/`Schemas`/`Tools`/`Subset`/`Clone` and the `Execute` lookup, released before the tool body runs; write lock in `Register`/`Unregister`) plus a `-race` regression test `TestRegistryConcurrentReadWrite`. Decision recorded as ADR-0013 (`docs/adrs/0013-tools-registry-owns-concurrency-safety.md`): the invariant lives in the shared type, and in-place reconcile was kept over a pointer-swap.
