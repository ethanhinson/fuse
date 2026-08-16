---
name: fitline-width-invariant-hides-truncated-suffix
slug: fitline-width-invariant-hides-truncated-suffix
title: A fitLine width-invariant test is blind to content fitLine silently truncated
hook: "When a row is composed as prefix+content+suffix and then passed through fitLine(row, w), a test that only asserts the row is exactly w cells CANNOT see an over-wide segment eating the suffix — fitLine guarantees the very width the test checks. Assert the SUFFIX survives verbatim, and budget truncation in display cells (reserving the ellipsis), not bytes."
promotion_state: candidate
changes: [66]
created: 2026-08-16
updated: 2026-08-16
topics: [tui, rendering, width-invariant, testing, truncation, lipgloss, blind-spot]
---

A fixed-width TUI row built as `head + preview + tail` and then fitted with `fitLine(row, w)`
has a self-cancelling defect class: if `preview` overflows its computed budget, the composed
row becomes `w+1` cells, and `fitLine`'s right-truncation quietly clips the **tail** — the
duration / event-count suffix — to bring the row back to `w`. A width-invariant test that
asserts `lipgloss.Width(row) == w` across a matrix of widths passes anyway, because `fitLine`
*produces* exactly the width the test asserts. The invariant is real but it is the wrong
witness: it can only catch width drift the composition failed to fit, never content the fit
silently ate.

Two coupled root causes seen together (change 0066, turn-header prompt preview):
1. **Off-by-the-ellipsis budget.** `truncate(s, n)` returned `s[:n] + "…"`, so reserving cells
   for the two surrounding quotes but not the ellipsis made the preview `budget+1` cells.
2. **Byte budget for a cell-denominated width.** A `lipgloss.Width` (display-cell) budget was
   handed to a byte-counting truncator, so CJK/emoji previews under-filled by ~2/3 while ASCII
   overflowed — and the byte cut is exactly what accidentally *hid* the overflow from a hostile-
   bytes test whose fixture led with multibyte sanitized characters.

The rule: to guard a truncated segment inside a fitted row, assert on the **surviving suffix**
(`strings.Contains(header, "· running")` / `"· N events"`), not on the total width — and
truncate in **display cells** reserving one cell for the ellipsis (`truncateCells`), never in
bytes. Add a pure-ASCII long-prompt case; a multibyte fixture can pass for the wrong reason.

## War story

**Change 0066 (PR #63), 2026-08-16 — collapsible per-turn groups in the agents detail pane.**
The turn header rendered `turn N · "<prompt preview>" · <duration>`. `turnPromptPreview`
reserved cells for the wrapping quotes but not the ellipsis, so an overflowing ASCII prompt
made the header one cell too wide; `renderTurnHeader`'s `fitLine(plain, w)` then truncated from
the right, clipping the tail so `… · running` rendered as `… · runnin` and `… · 24 events` as
`… · 24 event`. `TestDetailRowsWidthInvariant` never saw it — `fitLine` guarantees the width it
asserts. The deep review caught it (finding #1, `important`), and the first test written to
reproduce it *also* initially passed for the wrong reason: its `hostile` fixture led with
`\x1b` bytes that `sanitizeDisplay` turned into a multibyte `·`, so the byte-budget cut landed
short of the cell budget and the preview came in under width at every tested size. Fix: a
`truncateCells` helper that budgets in cells and reserves the ellipsis, plus a suffix-survival
assertion and an explicit pure-ASCII (and CJK under-fill) case. See also
[[border-inside-fixed-width-manual-join]] — same file, same `fitLine` invariant, different
lesson (that one is *where* the border width goes; this one is *what the width test can't see*).
