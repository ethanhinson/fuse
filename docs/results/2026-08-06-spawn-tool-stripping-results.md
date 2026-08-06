<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0033 — Strip spawn_agent from tool schemas at the concurrency cap and on budget exhaustion](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0033-spawn-tool-stripping.md)**
<!-- docket:backlink:end -->

# Strip spawn_agent from tool schemas — results
Change: #33 · Branch: feat/spawn-tool-stripping · PR: (opened at close-out) · Plan: docs/superpowers/plans/0033-spawn-tool-stripping-plan.md · ADRs: none

## Verify (human)

No interactive/manual checks required beyond CI — the behavior is fully covered by
automated tests (per-turn strip on budget exhaustion and at the active-child cap, cap
reversibility, budget permanence, backstops firing while stripped, and the new
negative-clamp validation). This section is informational only.

## Findings

Whole-branch review (before PR) found the implementation well-structured and correct —
race safety, off-by-one boundaries, the MaxDepth Unregister choice, all build paths,
registry non-mutation, and backstop preservation all explicitly verified. Three findings
were addressed in a follow-up commit:

- **Negative cap could silently disable a brake (minor).** A negative `agents.max_concurrent`
  (or `max_spawns`) passed the loader's `!= 0` merge gate and propagated raw. The tree
  clamps its own copy (`NewAgentTreeWithConcurrency` maps `<= 0` to the default), but
  `NewStripSpawnPredicate` received the raw negative and its `maxConcurrent > 0` /
  `max > 0` guards silently switched off the corresponding strip term. Fixed by clamping
  both caps to their defaults **once at load time** (`internal/config/loader.go`), keeping
  the tree's semaphore and the predicate's caps consistent. Regression tests added for
  both fields.
- **Stale README (minor).** The `agents` block documented `max_spawns: 16` (now 64) with an
  inline `7/16 used` example reflecting the old ceiling, and did not document the new
  `max_concurrent` key. Refreshed, including notes on the permanent budget-strip and
  reversible cap-strip.
- **Doc nit.** Added a note in `strip.go` on the deliberate asymmetry: the strip cap counts
  running + pending, while the runtime semaphore bounds running only (covered by
  `TestStripPredicatePendingCountsTowardCap`).

No decision rose to an ADR — the design was settled during brainstorm/build and the review
fixes are defensive validation, not architecture.

## Follow-ups

None. Auto-capture is disabled for this repo; no distinct follow-up work surfaced during
build or review.
