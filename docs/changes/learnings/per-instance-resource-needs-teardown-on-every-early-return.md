---
slug: per-instance-resource-needs-teardown-on-every-early-return
hook: "When a per-instance/per-loop factory attaches a resource that owns goroutines (an mcp.Manager with a read-pump, a client, a store), EVERY early-return path in the setup — not just the happy-path completion — must release it, symmetric with the store's Close. A `StartLoop` that fails AFTER attaching the manager but before the completion goroutine that would close it leaks the goroutines and its tracking-map entry. Add a teardown hook the runtime invokes on the early-return path, and regression-test the failure path explicitly."
topics: [go, concurrency, resource-leak, lifecycle, multi-tenancy, mcp]
changes: [59, 63]
created: 2026-08-11
updated: 2026-08-21
promotion_state: candidate
promoted_to:
---

## Apply

Deglobalizing to per-instance state ([[deglobalize-holder-also-per-instance-the-shared-graph]]) buys
isolation, but each per-instance resource that owns background goroutines now needs a **teardown on
every failure path**, not only on normal completion. The trap: the happy path closes the resource in
a completion goroutine, so setup that fails *after* attaching the resource but *before* that
goroutine is scheduled leaks it — the goroutines (read-pump, notify loop) run forever and the
tracking `sync.Map` entry is never cleared.

The fix pattern: give the runtime a **teardown seam** it invokes symmetrically with the resource it
already releases (here, the event store's `Close`), and route every early-return through it. For a
one-shot resource closed by a completion goroutine, return an **idempotent** (`sync.Once`) close func
and call it on the error return too. Then **regression-test the failure path directly** — inject the
failure (e.g. `BuildAgent` errors after `NewToolRegistry` attached the manager) and assert the
resource was released; a leak is invisible to a happy-path test and to `-race`.

## War story
- 2026-08-11 (#59, PR #56) — two review SHOULD-FIX leaks. (S1) The per-loop `mcp.Manager` attached in
  the loop-server's `NewToolRegistry` closure leaked when `StartLoop` failed at `BuildAgent`
  afterward — manager goroutines and the tracking `sync.Map` entry orphaned. Fixed by having the
  runtime invoke `Deps.LoopTeardown(toolReg)` on that early-return, symmetric with the store `Close`;
  test `TestStartLoop_TeardownOnBuildAgentError`. (S2) The one-shot manager was closed only by the
  completion goroutine, so a `StartLoop` error return leaked it — fixed by returning an idempotent
  (`sync.Once`) close func that `main.go` also calls on the error return. Both were found in
  whole-branch review, not by the suite, precisely because only the failure paths leak.
- 2026-08-21 (#63, PR #79) — the next turn of the same screw: **the teardown seam existed and was
  simply never reached on one binding.** The bash sandbox's warm pool releases through the
  `Deps.LoopTeardown(toolReg)` hook this finding's #59 story added — correct for the loop-server and
  one-shot paths. But the **shell/TUI binding leaked its sandbox pool for the entire session**,
  because there the TUI drives turns itself and the `runtime.New(...)` result is *discarded*, so the
  hook was dead code on that path: containers stayed alive for as long as the shell ran. Generalizes
  the rule — auditing that a teardown seam *exists* is not the audit. Enumerate every **construction
  site** of the runtime/registry and confirm each one actually reaches the seam; a binding that
  discards the runtime handle has silently opted out of every lifecycle guarantee attached to it.
  Same review-not-suite detection as #59, for the same reason: a leak that never fails a turn.
