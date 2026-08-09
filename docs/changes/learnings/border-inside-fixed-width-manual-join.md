---
name: border-inside-fixed-width-manual-join
slug: border-inside-fixed-width-manual-join
title: Fold a panel border into the existing fixed-width budget, don't wrap the join
hook: "Adding a per-panel border to a hand-composited fixed-width split must consume cells from the SAME fitLine budget (border glyph replaces a content column), never add width around a lipgloss.JoinHorizontal — assert the total-width invariant across widths × focus states"
promotion_state: candidate
changes: [41]
created: 2026-08-09
updated: 2026-08-09
topics: [tui, rendering, layout, bubbletea, lipgloss, width-invariant]
---

A fixed-width TUI that hand-joins its panes — `fitLine(left, treeW) + divChar + fitLine(right, detailW)` — owns an exact-total-width invariant: every rendered row is exactly the terminal width, so bubbletea's row diffing stays in sync. `lipgloss.JoinHorizontal` is deliberately *not* used here precisely because it can't constrain per-line widths; reaching for it (or for `lipgloss.Border`, which adds glyphs *around* content) to draw a focus border re-inflates the width and shears the layout.

The rule: a border glyph must be drawn **inside** the same width budget — it replaces a content column, it does not add one. The existing `divChar` seam between the two panes can double as one panel's border edge; a focus title likewise renders through `fitLine(title, w)` so it can never overflow. Pane widths (`treeW`/`detailW`) are computed once against the terminal width and every render site clamps to them.

The guard is not "does it look right" — it is a **width-invariant test that asserts each row equals the total width across a matrix of widths (80/100/120) AND every focus state**, because the border moves with focus and a regression only shows at one width or one focus combination.

## War story

(#41, PR #44) — Fuse agents split-panel overlay gained an accent focus border + title on the focused pane, muted on the unfocused. The width-accounting risk (flagged in the spec up front) was that a naive `lipgloss` border would blow the exact-width join. Resolution: manual border glyphs folded into the existing `fitLine(...)+divChar+fitLine(...)` join in `internal/tui/agents_model.go` (comment at the join records *why* `JoinHorizontal` is avoided); the focused pane borrows only its right border and reuses the existing 1-col `divChar` as the seam. `TestFocusWidthInvariant` (`internal/tui/agents_focus_test.go`) asserts the invariant across widths 80/100/120 in all focus states — the whole-branch review returned no blocking findings on the layout as a result.
