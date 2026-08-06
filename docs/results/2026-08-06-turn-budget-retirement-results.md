<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0038 — Retire the interactive turn cap — unlimited shell turns, headless backstop, doom-loop detection](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0038-turn-budget-retirement.md)**
<!-- docket:backlink:end -->

# Retire the interactive turn cap — results
Change: #38 · Branch: feat/live-mode-switch · PR: https://github.com/ethanhinson/fuse/pull/16 · Plan: docs/superpowers/plans/2026-08-06-turn-budget-retirement-plan.md · ADRs: none

Rides PR #16 alongside change 0035 (live-mode-switch) at the human's direction —
one merge completes the auto-mode long-run experience.

## Verify (human)

Automated tests cover the turn-cap resolution, doom-loop detection/reset, and
the `--approve-all` budget posture hermetically. Optional real-shell smoke at the
merge gate:

- [ ] `fuse shell` with `max_turns` unset: run a long agent task past 25 turns;
      confirm it no longer dies at "agent: max turns reached" (unlimited).
- [ ] Provoke a doom loop interactively (a task that repeats one identical tool
      call); confirm the popup reads **"⚠ Possible loop"** with a `Loop:` field
      (not an empty `Tool:`), offers only `[y] continue once` / `[n] abort`, and
      that `s` does nothing.
- [ ] `fuse "task" --approve-all` on a TTY provoking a doom loop: confirm it
      **aborts** with the structured `tool-call loop detected` error rather than
      auto-continuing forever, and that an unset `max_turns` backstops at 100.
- [ ] `fuse run` / non-TTY / mcp-server / research-probe with unset `max_turns`:
      confirm the 100-turn headless backstop still applies.

## Findings

- **Whole-branch review — correctness CLEAN.** The loop bound, detector reset on
  a differing call, context-retry, absence of stale readers, all four
  `agent.New` wiring sites, and the tests (verified non-tautological) all passed
  review.

- **Review CONCERN (fixed): `--approve-all` on a TTY auto-approved a doom loop
  forever.** A single `stdinIsTerminal()` boolean drove three things at once:
  the approval channel, the turn-cap posture (TTY ⇒ unlimited), and the loop
  force-through hook (TTY ⇒ wired to the approval func). Under `--approve-all`
  that approval func is `autoApprove`, so a doom loop tripped every 3 turns and
  was auto-continued indefinitely with no backstop. Fix: split the turn/loop
  **budget** posture from the **approval-channel** posture — `--approve-all` is a
  scripted "don't ask me" posture, so the budget resolves headless even on a TTY
  (100 backstop for unset `max_turns`; a nil loop hook so a trip aborts with the
  structured `ErrLoopDetected`). Explicit `max_turns` config still wins.
  Regression: `TestOneShotBudgetPostureUnderApproveAll`. No ADR — a bug fix
  inside the settled design, not a new architectural choice.

- **Review NIT (fixed): the loop force-through popup was mislabeled.** It showed
  an empty `Tool:` field and offered "allow for session" whose bool is discarded.
  Fix: the loop `ApprovalRequest` now carries a sentinel `ToolName`
  (`permissions.LoopApprovalToolName`); the TUI renders it as a "Possible loop"
  check, drops the session option, and makes the `[s]` key inert there. Tests:
  `TestLoopApprovalPopupWording`, `TestLoopApprovalKeySInert`.

## Follow-ups

None. Auto-capture is disabled for this repo; no adjacent work was surfaced that
would be its own change.

## Plan deviations (skill-layer degradations)

The three superpowers workflow skills resolved for this repo were not invocable
at runtime in this session, so per the docket Skill-layer missing-skill rule each
degraded to `auto` with the implementer performing the step directly:

- `superpowers:writing-plans` (plan) → plan authored inline.
- `superpowers:subagent-driven-development` (build) → plan executed inline with
  TDD (RED → GREEN per task) on the feature branch.
- `superpowers:requesting-code-review` (review) → whole-branch review performed
  before the PR; the CONCERN + NIT above are its findings, both fixed with TDD
  regressions.

These are per-machine skill-availability degradations, not repo state.
