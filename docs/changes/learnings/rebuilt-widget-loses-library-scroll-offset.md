---
name: rebuilt-widget-loses-library-scroll-offset
slug: rebuilt-widget-loses-library-scroll-offset
title: A stateful list widget rebuilt every render loses the library's scroll bookkeeping — window the rows yourself
hook: "If you construct a bubbles/table (or any widget holding its own viewport offset) fresh on each View() and restore selection with SetCursor, the cursor moves but the viewport YOffset does not — only the incremental MoveUp/MoveDown mutators advance it. The selected row scrolls out of sight while the widget reports the correct cursor index, so state assertions pass and only a human sees it. Either keep ONE long-lived widget instance and drive it with the library's own key handling, or slice the visible window of rows yourself and stop relying on the library's offset."
promotion_state: candidate
changes: [80]
created: 2026-09-02
updated: 2026-09-02
topics: [tui, bubbletea, bubbles, table, viewport, scrolling, selection, stateful-widget, immediate-mode, blind-spot]
---

Bubble Tea's render loop invites an immediate-mode habit: build the view's widgets from the model
inside `View()` and let them be garbage on the next frame. That is safe for stateless renderers and
unsafe for any library widget that keeps **derived state the constructor does not accept**. A
`bubbles/table` keeps two independent cursors: the logical selected index, and the viewport's
`YOffset`. `SetCursor` writes the first and re-renders the content; it does **not** write the
second. Only the incremental mutators (`MoveUp`, `MoveDown`) advance `YOffset`, because they are
the only path the library treats as "the user scrolled". So a table rebuilt per frame and restored
with `SetCursor(n)` renders with a correct highlight at a viewport that never moved — the selection
walks off the bottom edge and stays there.

The failure is worse than a cosmetic one because it is **invisible to the natural test**. Model
state is right: the cursor index is `n`, the key handler ran, the selection changed. Only the
composited frame is wrong, and only past the point where the list is longer than the pane — so a
fixture with a handful of rows never reproduces it. Pair the lesson with the fixture requirement:
any scroll/selection test needs **more rows than fit**, exactly as a width test needs an entry
wider than the budget.

The rule, in preference order:

1. **Hold one instance.** Store the widget in the model, mutate it in `Update`, and let the
   library own both cursors. This is what the library is designed for.
2. **If the widget must be rebuilt** — because the row set is derived from data that changes shape,
   which is the usual reason — **stop using the library's offset at all.** Compute the visible
   window explicitly (`start = clamp(cursor - height + 1, 0, len(rows) - height)`) and hand the
   widget only that slice with a locally-adjusted cursor. Half-measures — rebuilding *and* calling
   `SetCursor` and hoping the offset follows — are the bug.

Generalize past `bubbles/table`: the smell is **a constructor that cannot express a piece of state
the widget mutates**. Any such widget is not safe to rebuild per frame.

## War story

**Change 0080 (PR #85), 2026-09-02 — shared TUI table primitive + tabbed `/config` screen.** The
`/config` Models tab built its `bubbles/table` from the current model list on every render and
restored the highlight with `SetCursor`. Arrowing down past the bottom of the pane moved the
highlight out of the visible window permanently: `SetCursor` had updated `m.start` and called
`viewport.SetContent`, but `YOffset` stayed at 0 because neither `MoveUp` nor `MoveDown` was ever
called. The whole-branch deep review filed it as the run's **only blocker** (finding #1), and the
severity came from the neighbourhood rather than the rendering — the Models tab's delete action has
no confirmation dialog, so an invisible selection meant deleting a row the human could not see. The
fix abandoned the library's offset bookkeeping and windowed the rows explicitly before handing them
to the table.
