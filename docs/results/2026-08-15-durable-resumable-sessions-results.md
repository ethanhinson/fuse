# Durable, resumable sessions — results

Change [#0054](../changes/active/0054-durable-resumable-sessions.md) ·
Branch `feat/durable-resumable-sessions` · Base `origin/main` @ `e6e637f`

## Outcome

All six plan tasks landed, TDD, one commit each. A refresh/reconnect now restores a
persistent conversation: an interactive loop survives client disconnect, its transcript is
rebuilt from the durable event stream, and a `Send` transparently resumes a reaped/evicted
session instead of returning `ErrLoopFinished`.

## What shipped (by decision)

- **D1/D5 — rebuild from events, no second source of truth.** `reconstructMessages`
  folds the durable stream into the model-facing transcript. Delivered as the materialized
  final state (not step-by-step catch-up). Byte-equal to the live transcript (round-trip
  test).
- **New `KindUserInput` event.** Build-time reconcile found user input was absent from the
  stream; added it so the fold is lossless. `Agent.SetSeeded` suppresses re-emit on resume.
- **D2 — session ≠ connection.** Interactive loops run under a `WithoutCancel`-derived
  session ctx; a 30m-default idle-TTL reaper bounds a disconnected session. Verified:
  disconnect-survives, idle-reap without leak.
- **D4 — `Resume` seam.** Tenant-scoped rehydrate + re-park under the original loop_id.
  Verified cold cross-instance (runtime B revives a loop runtime A finished).
- **D3 — resume on `Send`, reuse Connect stream.** No new `fuse.loop.v1` RPC. Real-server
  cold-resume acceptance passes end to end.

## Reconciliation

The stub was written against the pre-#55 WS+REST wire; ADR-0033 replaced it with Connect
`fuse.loop.v1`. The design decisions survived the remap; the spec and this build target
today's reality. All 8 spec code-level assumptions were verified against `origin/main`
before building. One design gap the spec flagged as open (user-input events) was resolved
by adding `KindUserInput`.

## Test results

- Changed packages green under `-race`: `internal/runtime`, `internal/agent`,
  `internal/event`, `internal/loopconnect`.
- `internal/...`, `sdk/fuse/...`, and `cmd/fuse` (with the flaky test below skipped) all
  pass.
- **Pre-existing flake — NOT from this change.** `cmd/fuse` `TestObservabilityAcceptanceHermetic`
  fails intermittently (~12–35%) only under full-package load. Reproduced on **clean
  `origin/main`** (1/8 full-package runs failed) with change 0054 absent. It is a timing
  race inside that test (1ms span batch timeout / 2s start deadline under load), unrelated
  to session resume — a non-interactive loop's span/metric structure is unchanged by this
  change (the only delta is an untracked `KindUserInput` event, which creates no span).
  Passes reliably in isolation. Filing/fixing it is out of scope for 0054.

## Base-branch note

The feature branch is based on `e6e637f`; `origin/main` has since advanced (observability
0051/0061). Finalize's rebase-onto-base gate reconciles at merge time.
