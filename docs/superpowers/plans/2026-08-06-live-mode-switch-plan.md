<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0035 — Mode switch must bite mid-turn — gates read the SessionMode holder live, not a construction snapshot](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0035-live-mode-switch.md)**
<!-- docket:backlink:end -->

# Plan — Mode switch must bite mid-turn (change 0035)

> Plan authored inline by `docket-implement-next` (degradation: the resolved
> plan skill `superpowers:writing-plans` was not invocable at runtime; per the
> Skill layer missing-skill rule the implementer authored the plan itself).

## Goal

Make a mid-turn permission-mode switch take effect on gates that are already
built — including running children — by giving `PermissionGate` a live view of
the session's `SessionMode` holder instead of a construction-time snapshot.
Holderless gates (one-shot, mcp-server, tests) keep the snapshot fallback and
behave exactly as today.

## Background (verified against `main`, 2026-08-06)

- `PermissionGate.currentMode()` returns `g.mode` under `modeMu`
  (`internal/permissions/gate.go`). The snapshot field is the only mode source.
- `buildGate` seeds each gate with `WithMode(sessionGateMode(cfg, sm))`
  (`cmd/fuse/run.go`), a construction-time snapshot of `sm.Get()`.
- `CloneForChild` snapshots `g.currentMode()` into the child's own `mode` field.
- `SetMode` performs the escalation-valve reset on the auto→non-auto transition;
  it is the only mode write path today.
- `SessionMode` (`internal/permissions/sessionmode.go`) is a mutex-guarded holder
  with `Get`/`Set`, already written by the TUI's Shift+Tab / `/mode`.

## Design

Add `WithSessionMode(*SessionMode)` option. When the holder is set:

- `currentMode()` returns `holder.Get()` live; the snapshot field is the fallback
  when the holder is nil.
- `CloneForChild` propagates the holder (`sessionMode` field copied by reference),
  so running children read the session mode live too.
- The valve reset must still fire on an observed auto→non-auto transition. Since
  the mode can now change without `SetMode` being called (the holder is mutated
  by the TUI directly), `currentMode()` becomes the observation point: it reads
  the effective mode, compares it to a `lastObservedMode` field tracked under
  `modeMu`, and on an auto→non-auto transition calls `valve.reset()`. `SetMode`'s
  existing reset stays for holderless gates.

Valve-reset care: `currentMode()` is called on the hot resolve path and must stay
cheap and race-free. The observation/compare/reset is done under `modeMu` (already
held for the mode read). `valve.reset()` takes its own `valve.mu` — no lock-order
inversion since `modeMu` and `valve.mu` are never acquired in the opposite order
anywhere. Seed `lastObservedMode` to the gate's initial effective mode at
construction so the first read is not spuriously seen as a transition.

Wiring: `buildGate` appends `WithSessionMode(sm)` when `sm != nil`; nil paths
unchanged (one-shot / mcp-server continue to pass no holder and keep `WithMode`).

## Tasks

### Task 1 — `WithSessionMode` + live `currentMode()` read, with valve-reset-on-observed-transition (TDD)

Failing tests first, in `internal/permissions/sessionmode_test.go` (or a new
`sessionmode_live_test.go`):

1. **Live read**: build a gate with `WithSessionMode(holder)` seeded smart; flip
   `holder.Set(ModeAuto)`; assert the SAME gate's `Mode()` now returns `ModeAuto`
   (no rebuild). A holderless gate's `Mode()` still returns its snapshot field.
2. **Mid-turn flip auto-approves**: gate wired with a classifier, holder seeded
   `ModeSmart`; a gray-area/prompt call would prompt; flip holder to `ModeAuto`;
   the same gate now auto-approves a read-only bash (`git status`) with the
   approval func never consulted — no rebuild.
3. **Valve reset on observed transition**: gate in auto with the holder, absorb ≥1
   classifier block so `valve.counts()` is non-zero; flip the holder to a non-auto
   mode; the next `Execute`/`currentMode()` observes the auto→non-auto transition
   and zeroes the valve (assert `valve.counts() == 0,0`). Entering/staying auto
   leaves counters untouched.
4. **Holderless unchanged**: a gate built without the holder still resolves off
   its snapshot field and `SetMode` still drives the valve reset (existing tests
   in `sessionmode_test.go` must stay green).
5. **Concurrency**: a `-race` goroutine pair flipping `holder.Set()` while the gate
   resolves through `currentMode()` — proves the live read path is race-free.

Implementation in `internal/permissions/gate.go`:

- Add `sessionMode *SessionMode` and `lastObservedMode PermissionMode` fields.
- `WithSessionMode(sm *SessionMode) Option`.
- Seed `lastObservedMode` at construction (after options apply) to the effective
  initial mode.
- Rewrite `currentMode()` to read `holder.Get()` when `g.sessionMode != nil`, else
  `g.mode`, all under `modeMu`; then compare to `lastObservedMode`, update it, and
  on auto→non-auto call `g.valve.reset()`.

### Task 2 — `CloneForChild` propagates the holder + child follows the session live (TDD)

Failing tests first:

1. A child cloned from a holder-backed parent, then the holder flipped to auto,
   observes auto live via the child's own `Mode()` (supersedes D10's
   "children keep their spawn mode" — now children with a holder follow it).
2. A holderless parent's child still snapshots the parent's mode (existing
   `TestGate_CloneForChild_SnapshotsModeBeforeSetMode` stays green).

Implementation: copy `sessionMode` by reference into the cloned gate; seed the
child's `lastObservedMode` to its effective initial mode.

### Task 3 — Wire `WithSessionMode` into `buildGate` (TDD)

Failing test first in `cmd/fuse/run_session_mode_test.go`:

- Build a gate via `buildGate(..., sm)` with `sm` seeded smart; flip `sm.Set(auto)`
  WITHOUT rebuilding; assert the SAME gate now auto-approves read-only bash and
  reports `Mode() == ModeAuto` — the mid-turn regression this change fixes at the
  wiring seam. A `buildGate(..., nil)` gate keeps the cfg-derived snapshot posture.

Implementation in `cmd/fuse/run.go`: in `buildGate`, append
`permissions.WithSessionMode(sm)` when `sm != nil`.

## Verification

- `go test ./internal/permissions/... ./cmd/fuse/... -race`
- `go build ./...`
- Full suite `go test ./... -race` at the end (single gate).

## Out of scope

- One-shot / mcp-server switching surfaces (no switcher there).
- Any TUI change — Shift+Tab and `/mode` already write the holder.
