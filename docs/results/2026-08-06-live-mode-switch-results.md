<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0035 — Mode switch must bite mid-turn — gates read the SessionMode holder live, not a construction snapshot](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0035-live-mode-switch.md)**
<!-- docket:backlink:end -->

# Mode switch must bite mid-turn — results
Change: #35 · Branch: feat/live-mode-switch · PR: <opened at close-out> · Plan: docs/superpowers/plans/2026-08-06-live-mode-switch-plan.md · ADRs: none

## Verify (human)

Automated tests cover the mid-turn behavior hermetically (no network / no live
classifier), so no manual check is strictly required. Optional real-shell
smoke, if desired at the merge gate:

- [ ] Start `fuse` interactively in `smart` mode; kick off a long agent run that
      prompts repeatedly; press Shift+Tab (or `/mode auto`) MID-RUN; confirm the
      running turn (and any spawned children) stop asking and auto-approve
      read-only calls without waiting for the next turn.

## Findings

- **Whole-branch review caught a latent valve double-reset (blocker, fixed).**
  The first implementation advanced `g.mode` in `SetMode` but not the new
  `lastObservedMode` transition tracker, so a `currentMode()` call immediately
  after a leaving-auto `SetMode` re-detected the same auto→non-auto edge and
  reset the escalation valve a second time. Idempotent today (0,0 → 0,0) but it
  violated "reset exactly once per leaving-auto edge" and was inconsistent for
  holder-backed gates. Fix: `SetMode` now computes the transition against
  `lastObservedMode` and advances it, so `SetMode` and `currentMode()` share one
  observation ledger. Added `TestGate_SetMode_ThenResolve_NoDoubleReset`.
  No ADR — the decision is a bug fix within the settled design, not a new
  architectural choice.

- **Design decisions were all dictated by the settled change body** (holder as
  live source in `currentMode()`, holder propagation into children superseding
  D10's spawn-mode snapshot, valve-reset-on-observed-transition). No ADR
  produced; ADRs 0005/0006 remain the cited context.

## Follow-ups

None. Auto-capture is disabled for this repo; no adjacent work was surfaced that
would be its own change.

## Plan deviations (skill-layer degradations)

The three superpowers workflow skills resolved for this repo were not invocable
at runtime in this session, so per the docket Skill-layer missing-skill rule
each degraded to `auto` with the implementer performing the step directly:

- `superpowers:writing-plans` (plan) → plan authored inline.
- `superpowers:subagent-driven-development` (build) → plan executed inline with
  TDD (RED → GREEN per task) on the feature branch.
- `superpowers:requesting-code-review` (review) → whole-branch adversarial
  review performed via a dispatched review subagent before the PR.

These are per-machine skill-availability degradations, not repo state.
