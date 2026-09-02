---
name: primitive-adoption-must-thread-the-width
slug: primitive-adoption-must-thread-the-width
title: Consolidating a width clamp behind a primitive does not deliver it — every adoption site must thread the real width through
hook: "When you extract a shared render primitive whose whole point is a width guarantee, audit every call site for the unbounded sentinel (`width: 0`, no width, `math.MaxInt`). A primitive with an opt-out defaults callers OUT of the protection the extraction exists to provide, and the migration diff looks complete because the call compiles and renders. Either drop the sentinel so width is a required argument, or give every adopted surface its own `rendered row fits width` test — the shared primitive's own passing test is not a witness for its callers."
promotion_state: candidate
changes: [80]
created: 2026-09-02
updated: 2026-09-02
topics: [tui, rendering, width-invariant, refactoring, primitive-extraction, adoption, testing, blind-spot]
---

Extracting a shared component to fix a class of bug creates a false sense of closure: the bug is
now *fixable* in one place, but it is only *fixed* at the sites that actually pass the component
the information it needs. When the primitive accepts an unbounded sentinel — `width: 0` meaning
"lay out however wide you like", the natural signature for a caller that genuinely has no width
budget — then every migrated call site that has not yet plumbed a width down to it silently opts
out. The consolidation diff reads as complete: the old hand-rolled padding is gone, the shared
primitive is called, the code compiles, and the surface still renders. What it renders is the
pre-consolidation behavior.

Two failure shapes follow from the sentinel, and they look different at the terminal:

1. **Unbounded layout, then wrapped downstream.** The primitive pads columns to their natural
   maximum, the composed row exceeds the pane, and whatever composites it (`wordwrap`, lipgloss)
   breaks the row onto a second line — the change-0078 failure mode
   ([[global-max-column-width-must-clamp-to-render-width]]) reappearing through a component that
   was written to prevent it.
2. **Unbounded layout, then clipped downstream.** The over-wide row reaches a `fitLine`-style
   fitter, which right-truncates and eats the trailing columns — invisible to any width-invariant
   assertion, because the fitter produces exactly the width the test checks
   ([[fitline-width-invariant-hides-truncated-suffix]]).

The rule: **treat the sentinel as the defect, not the default.** Prefer a signature where width is
required and a caller that truly has no bound says so explicitly (a distinct `Unbounded` value that
greps as a decision, not a zero that greps as "not filled in yet"). Where the sentinel must stay,
the migration is not done when the call sites compile — it is done when each adopted surface has
its own test asserting the rendered row fits the width that surface was given. A shared primitive's
own green width test proves the primitive; it proves nothing about the fifteen callers.

## War story

**Change 0080 (PR #85), 2026-09-02 — shared TUI table primitive + tabbed `/config` screen.** The
change extracted a table component specifically to consolidate column-width handling after changes
0078/0079 had chased the same alignment bugs through hand-rolled padding at 33 render sites. Of the
three surfaces adopted in the change, **two passed `width: 0`**: the `/models` listing was laid out
unbounded and then wordwrapped, and the model-editor overlay was laid out unbounded and then
clipped from the right by `fitLine`. Both were caught by the whole-branch deep review as separate
findings (#2 and #3, `important`) — not by the suite, which was green, because the primitive's own
width tests passed and neither adopted surface had one. Both were fixed by threading the real pane
width down to the call and adding per-surface width tests. The change had 18 of 33 sites migrated;
the lesson is that "migrated" is a claim about the call, and the guarantee is a claim about the
argument.
