---
name: mutex-test-double-concurrent-provider
slug: mutex-test-double-concurrent-provider
title: Mutex-protect BOTH getter and setter on test doubles shared with goroutines
hook: "Mutex-protect both getter and setter on test doubles shared with goroutines — a lock on one side only is still a race"
promotion_state: retained
changes: [10]
created: 2026-08-04
updated: 2026-08-04
topics: [testing, concurrency, race, goroutines]
---

When a test double (a `fakeProvider`, stub, etc.) is shared between production goroutines and test-control code, BOTH the read path (`Commands()`) and the write path (`push()`) must hold the **same** mutex. A mutex only on the production-called getter does nothing if the test setter mutates the slice without holding it.

**Why it's easy to miss:** The setter (`push()`) is test-only code — it feels like "just a helper." But if the production goroutine (e.g. the `SlashRegistry` fanout) calls `Commands()` concurrently with the test goroutine calling `push()`, a data race fires under `-race` even though the slice is never accessed without the getter's lock.

**Rule:** For any test double that a goroutine reads via a method:
- Getter (`Commands()`): `p.mu.RLock()` / `p.mu.RUnlock()`; copy the slice before returning
- Setter (`push()`): `p.mu.Lock()` / `p.mu.Unlock()` around the assignment
- Return a copy from the getter — the slice header is safe, but callers who hold no lock will observe a slice whose backing array is being replaced

**How to apply:** Any time you write a fake for a `CommandProvider`, `source`, or other "emit a list on demand" interface that a background goroutine polls, add `sync.RWMutex` to the struct and protect both sides immediately — not as an afterthought when `-race` fails in CI.

## War story

(#10, PR #9) — Change 0010 (shell slash-command autocomplete). `SlashRegistry.fanout()` calls `Commands()` on each provider in a goroutine; the test's `fakeProvider.push()` had no lock. `go test -race` caught the race. Fix: added `sync.RWMutex` to `fakeProvider` with `RLock` in `Commands()` and `Lock` in `push()`; `Commands()` now returns a defensive copy.
