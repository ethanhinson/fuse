<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0038 — Retire the interactive turn cap — unlimited shell turns, headless backstop, doom-loop detection](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0038-turn-budget-retirement.md)**
<!-- docket:backlink:end -->

# Plan — Retire the interactive turn cap (change 0038)

> Authored by `docket-implement-next` (plan role degraded to `auto` — `superpowers:writing-plans` is not installed on this machine; missing-skill rule).
> Rides `feat/live-mode-switch` / PR #16 at the human's direction — one merge completes the auto-mode long-run experience.

## Goal

Retire the blunt `max_turns` cap that killed the first real long unattended run. After this change:

- **Interactive shell** (`fuse shell`): `max_turns` unset ⇒ **unlimited** turns.
- **Headless** (one-shot `fuse "<task>"`, non-TTY, `mcp-server`, `research-probe`): `max_turns` unset ⇒ **100-turn backstop**.
- **Explicit `max_turns: N` (N>0)**: caps **every** context.
- **Explicit `max_turns: 0`**: explicitly **unlimited everywhere** (the visible, deliberate scripted-use footgun, like `--approve-all`).
- **Doom-loop detection** (already present, refined): 3 consecutive byte-identical tool-call sets ⇒ interactive **force-through approval** with a "possible loop" preview regardless of mode; non-interactive **structured abort** naming the repeated call. Counter resets on any differing set.

## Ground truth (verified on the feature branch, reconcile 2026-08-06)

- `internal/agent/agent.go:49-50` — `if maxTurns <= 0 { maxTurns = 25 }` (retire).
- `internal/agent/loop.go:133` — `for turn := 0; turn < a.maxTurns; turn++` (needs an unlimited branch); `:200` `return ErrMaxTurns`; `:15` `ErrMaxTurns`.
- `internal/agent/loop.go:24` `loopLimit = 3`; `:119` `newLoopDetector`; `:188-195` fingerprint + `detector.seen` → currently **always** `ErrLoopDetected` (refine to mode-aware).
- `internal/agent/fingerprint.go` — `fingerprint`, `loopDetector.seen` (order-independent set match).
- `internal/config/schema.go:97` (`Config.MaxTurns int`), `:119` (`rawConfig.MaxTurns int`), `:173` (`MaxTurns: 25` default).
- `*int` presence precedent: `rawPermissionsConfig.SessionAllow *bool` (`schema.go:144`, resolved `:180`).
- `cmd/fuse/run.go` — FOUR `agent.New(... cfg.MaxTurns ...)` sites (:228, :239, :329, :343), all uniform.
- `cmd/fuse/approval.go:19` `stdinIsTerminal()`; one-shot at `main.go:108`; interactive shell path `shell.go`; `mcp_server.go`, `research_probe.go` headless.
- `internal/permissions/gate.go` — `ApprovalFunc`, `WithInteractive`, `interactive` field (approval channel + interactivity signal already modeled here).

## Design decisions

1. **Presence detection at the config layer.** `rawConfig.MaxTurns` → `*int`. `Config.MaxTurns` stays `int` but gains a companion `MaxTurnsSet bool` (mirrors the resolved-`SessionAllow` shape), OR `Config.MaxTurns *int`. Chosen: **`Config.MaxTurns *int`** — nil = unset, non-nil = explicit (including explicit `0`). This preserves the three-way distinction (unset / explicit-0 / explicit-N) all the way to the call site, and the `25` hardcoded default at `schema.go:173` is deleted.

2. **The backstop decision is context-aware and lives in `run.go`**, not `schema.go` (which is context-free) and not `agent.New` (which is uniform across all four sites). `run.go` already knows interactive-vs-headless. A small resolver — `resolveMaxTurns(cfgMaxTurns *int, interactive bool) int` — maps:
   - `cfgMaxTurns == nil` (unset) + interactive ⇒ `0` (unlimited sentinel).
   - `cfgMaxTurns == nil` (unset) + headless ⇒ `100`.
   - `cfgMaxTurns != nil` ⇒ `*cfgMaxTurns` verbatim (`0` = unlimited, `N>0` = cap).

3. **`0` is the unlimited sentinel in the loop.** `agent.New` no longer coerces `<=0 ⇒ 25`; it stores `maxTurns` as given. `loop.go:133` becomes: unlimited when `a.maxTurns <= 0`, else bounded by `a.maxTurns`. `ErrMaxTurns` is only ever returned on a positive cap.

4. **Doom-loop force-through (refinement of existing detection).** The agent gains an interactivity signal + an approval callback so the loop, on a tripped detector, can force the repeated call through approval (interactive) instead of unconditionally aborting. Non-interactive keeps the structured `ErrLoopDetected` abort, enriched to name the repeated call in its preview. Wiring: extend `agent.New` (or an `Agent` option) with the interactive flag + an approval hook sourced from the same `ApprovalFunc`/`interactive` the gate already carries, so `run.go` passes them at the four sites. **This is the ADR-worthy decision** (loop gains an approval channel it previously lacked) — flag for `docket-adr` at review.

## Tasks (TDD — test first, then implement, per task)

### Task 1 — `Config.MaxTurns` presence detection (`internal/config/schema.go`)
- **Test:** `max_turns` omitted ⇒ resolved `MaxTurns == nil`; `max_turns: 0` ⇒ non-nil `*0`; `max_turns: 7` ⇒ non-nil `*7`. Cover the raw→resolved path (mirror the existing `SessionAllow` tests).
- **Impl:** `rawConfig.MaxTurns *int`; `Config.MaxTurns *int`; delete the `MaxTurns: 25` default at `:173`; carry presence through resolution.
- **Fallout:** every reader of `cfg.MaxTurns` now sees `*int` — fixed in Task 3.

### Task 2 — Unlimited-turn loop + retired coercion (`internal/agent/agent.go`, `internal/agent/loop.go`)
- **Test:** `Agent` with `maxTurns == 0` runs past 25 turns (drive a stub `Completer` that always returns a tool call for, say, 30 turns, and assert no `ErrMaxTurns`); with `maxTurns == 3` returns `ErrMaxTurns` after exactly 3; with `maxTurns == 5` a natural stop (no tool calls) returns nil before 5.
- **Impl:** delete `if maxTurns <= 0 { maxTurns = 25 }` in `agent.New`; change `loop.go:133` to treat `maxTurns <= 0` as unbounded (`for turn := 0; a.maxTurns <= 0 || turn < a.maxTurns; turn++`), leaving `ErrMaxTurns` reachable only under a positive cap.

### Task 3 — Context-aware backstop resolver + four call sites (`cmd/fuse/run.go`, entry points)
- **Test:** `resolveMaxTurns(nil, true) == 0`; `resolveMaxTurns(nil, false) == 100`; `resolveMaxTurns(ptr(0), *) == 0`; `resolveMaxTurns(ptr(9), *) == 9`. Then an entry-level test: interactive shell path resolves unlimited; one-shot path resolves 100 (extend `run_session_mode_test.go` / a sibling table test).
- **Impl:** add `resolveMaxTurns`; thread interactive-vs-headless (already known via `stdinIsTerminal()` / shell vs one-shot / mcp-server / research-probe) into each of the four `agent.New` sites so each passes the resolved `int`, not `cfg.MaxTurns` directly.

### Task 4 — Mode-aware doom-loop (`internal/agent/loop.go`, `agent.go`, `cmd/fuse/run.go`)
- **Test:** with the interactive approval hook set, a tripped detector calls the hook with a "possible loop" preview and, on approval, continues (counter behavior on approval defined in impl); with no hook / non-interactive, a tripped detector returns `ErrLoopDetected` whose message names the repeated call. Table both.
- **Impl:** give `Agent` an interactivity flag + approval hook; at `loop.go:192-195`, branch: interactive ⇒ force the repeated call through approval with a "possible loop" preview and continue when approved (reset or advance the counter deliberately so a genuinely-stuck run still terminates); non-interactive ⇒ `ErrLoopDetected` enriched with the repeated call name. Wire the hook from `run.go`'s existing `ApprovalFunc` + `interactive`.

### Task 5 — Full suite + gofmt/vet
- `go test ./...`, `gofmt -l`, `go vet ./...` green across the branch (0035 + 0038). No new lint.

## Out of scope
- Token/spend budgeting; context-window compaction (already handled separately in `loop.go`).
- Per-project `max_turns` overrides (`projects:` stays permissions-only).

## Verification note for the merge gate
Behavioral change — the human should verify from the **feature-worktree binary**, not the `main` checkout (learnings: `verify-from-feature-worktree-binary`): `cd .worktrees/live-mode-switch && go build -o /tmp/fuse-0038 ./cmd/fuse`, then confirm `fuse shell` runs past 25 turns and a piped/non-TTY one-shot still backstops at 100.
