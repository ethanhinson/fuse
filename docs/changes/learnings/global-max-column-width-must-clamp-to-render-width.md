---
name: global-max-column-width-must-clamp-to-render-width
slug: global-max-column-width-must-clamp-to-render-width
title: A column padded to a global-max width must be clamped to the render width, or one wide entry wraps every row
hook: "When you pad a column to the widest value across the WHOLE data set (not just the visible page) so gutters stay stable while scrolling, clamp that pad target against the actual render width. An off-screen over-wide entry (e.g. an MCP /mcp:<server>/<tool> name) sets a pad run that overflows the line; composited through wordwrap it breaks at a space INSIDE the padding and pushes later columns onto a second line on EVERY row — even when no wide entry is visible. Assert the rendered row fits the width it was given, not just that columns align."
promotion_state: candidate
changes: [78]
created: 2026-08-21
updated: 2026-08-21
topics: [tui, rendering, width-invariant, alignment, padding, wordwrap, lipgloss, scrolling, testing, blind-spot]
---

Padding a column to a **global** maximum — the widest value across the entire data set rather
than the currently-visible page — is the right way to keep gutters from shifting as a list
scrolls. The trap: that global max is unbounded by the viewport. A single wide entry that is
**not even on screen** sets the pad target, and if that target plus the remaining fixed columns
exceeds the render width, the composed row is over-wide. When the overlay is composited through a
wrapping primitive (`wordwrap` / lipgloss word-wrap), the line breaks at a space **inside the new
padding run**, pushing the kind-tag and description columns onto a second line — on **every** row,
because they all share the same pad target. An 8-row overlay becomes 16+ lines at an ordinary 80
columns, and the alignment the padding exists to create is destroyed.

The rule: **clamp the global-max pad target against the render `width`** before padding, and
truncate the padded portion to the clamp — `capCmd = width - (fixed column runs + a min desc
budget)`; pad to `min(globalMax, capCmd)`. Then guard it with a test that asserts the **rendered
row fits `width`** (`lipgloss.Width(row) <= width`) across a matrix that includes an entry wider
than the budget *while it is scrolled off the visible page* — an alignment-only assertion cannot
see this, exactly as a width-invariant assertion cannot see suffix-eating truncation
([[fitline-width-invariant-hides-truncated-suffix]] — sibling lesson: same package, same "the
obvious assertion is the wrong witness" shape, different root cause). Note the styling cost of the
clamp: a truncated command portion must be cut as one composed **plain** string (ANSI escapes are
not display cells), so a truncated row loses per-segment styling — an accepted trade for a row
that fits.

## War story

**Change 0078 (PR #82), 2026-08-21 — `/models` command + slash-menu column alignment.** The menu
was changed to pad the command column to `SlashRegistry.MaxCommandWidth()` — the widest command
across **all** entries, so the kind-tag/description gutters stay fixed while scrolling. That max
spans `All()`, which includes MCP entries shaped `/mcp:<server>/<tool>`, routinely 40+ cells. The
first implementation never clamped the pad run against `width`: composited through `wordwrap`, one
long MCP tool name broke every overlay row at a space inside the padding and turned the 8-row
overlay into 16+ lines at plain 80 columns — even with no MCP entry in the visible window. The
whole-branch review caught it as the run's one `important` finding; the accompanying test had
passed while blind, because it asserted only that columns *aligned*, never that the row *fit* the
width it passed. Fix: clamp to `width - (2 + 2 + kindTagWidth + 2 + completerMinDescCells)`,
truncate the command portion to the clamp, and assert `row fits width` with an off-screen
over-wide entry in the fixture.
