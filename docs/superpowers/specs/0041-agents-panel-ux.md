<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0041 — Agents split-panel UX — focus indicator, reliable scrolling, blackboard readability](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/archive/2026-08-09-0041-agents-panel-ux.md)**
<!-- docket:backlink:end -->

# Spec 0041 — Agents split-panel UX: focus indicator, focused-panel scrolling, blackboard readability

## Problem

The agents overlay (`internal/tui/agents_model.go`) renders a two-panel split — a
subagent tree/output list on the left (40% width) and a detail/blackboard pane on
the right (60% width), separated by a `│` divider (`agents_model.go:165-220`). Three
concrete usability defects, each traced to code:

1. **No focused-panel indicator.** There is no visual signal of which panel is
   active. Focus lives entirely in state flags (`inDetail`, `inBlackboard`,
   `inEventView`, `inSegmentView` — `agents_model.go:39-51`) that are never rendered.
   The divider is always the same muted color (`agents_model.go:205`); the only
   reverse-video highlight is on the *selected row*, not the *active panel*
   (`agents_model.go:504-507`, `916-919`). The user cannot tell left from right.

2. **Mouse wheel is unreliable.** `handleMouse` (`agents_model.go:358-394`) ignores
   `msg.X`/`msg.Y` entirely and routes the wheel by internal state, and for the
   list panes it moves the **selection** (`m.selected`, `m.eventSel`) rather than a
   **scroll offset**. Wheeling therefore drags the cursor around instead of
   scrolling the view — the "unreliable" feel.

3. **Blackboard breaks scrolling AND is hard to read.**
   - **Scroll bug:** `handleMouse` has **no `inBlackboard` case**, so a wheel event
     over the blackboard falls into `default: // tree pane` and moves the *tree*
     selection instead of scrolling `m.bbScroll` (`agents_model.go:358-394`,
     blackboard offset at `agents_model.go:50-51,722-745`).
   - **Readability:** values render in muted `#5c6370` (low contrast); entries are
     sorted alphabetically by key with no writer grouping and no separators; JSON
     values print single-line with no structure (`agents_model.go:664-746`,
     `687-710`, colors in `theme.go:6-12`).

## Ground truth: what the blackboard actually is

Per `internal/agent/blackboard.go`, the blackboard is a **session-scoped, in-memory,
thread-safe key→value store** shared by every agent in an `AgentTree` (change 0023),
enabling ensemble / debate / producer-consumer coordination (`blackboard.go:24-28`).
It is **not** a chronological transcript by nature — it is keyed by name and displayed
sorted by key.

Crucially, every entry already carries provenance, stamped by the store (never by the
model): `WriterID`, `WriterLabel` (which agent wrote it), and `WrittenAt` (wall-clock)
— `blackboard.go:17-22`. The TUI reads a race-free `Snapshot()`
(`blackboard.go:117-125`). Therefore a "who said what" presentation is achievable
**purely in the TUI render path — no data-model change**. This spec changes only how
the existing snapshot is read and rendered, and how input is routed. `blackboard.go`
is not touched.

## Goals

- Make the active panel obvious at a glance.
- Make mouse-wheel scrolling behave predictably in every pane, including the
  blackboard.
- Make the blackboard read as an attributed, grouped, legible view of who-wrote-what.

## Non-goals

- No change to the blackboard data model, store, or the `Blackboard`/`BlackboardStore`
  API (`internal/agent/blackboard.go`, `internal/tools/blackboard.go`).
- No cursor-position hit-testing for the mouse (we route the wheel to the *focused*
  panel, not the panel physically under the pointer — see Decision 2).
- No full markdown renderer for values (values pretty-print as JSON — see Decision 5).
- No writer-based access control, timestamps-as-timeline reordering, or "toggle view"
  mode (a time-ordered lens is noted as a possible future, not built here).
- No change to the tree/event-list *keyboard* semantics beyond what's needed to make
  focus and scroll coherent.

## Design

Two phases, one change/PR. Phase 1 is the small interaction fix; Phase 2 is the
blackboard presentation redesign. They share the focus model, so Phase 1 lands the
focus concept and Phase 2 builds on it.

### Decision 1 — Colored-border focus indicator (Phase 1)

Wrap each panel in a box border (lipgloss) and color it by focus:

- **Focused panel:** accent-colored border (reuse an existing accent from
  `theme.go` — e.g. `colCyan`), and render its title/header in the same accent.
- **Unfocused panel:** muted border (`colMuted`).

A single "which side has focus" notion drives the border. Today focus is spread
across `inDetail`/`inBlackboard`/`inEventView`/`inSegmentView`. Introduce a derived
`focusedPane` concept (left = tree, right = detail/blackboard/event/segment — the
right pane is whatever the right column is currently showing). The existing flags
still decide *what the right column renders*; the new concept decides *which border
is hot*. Do not add a redundant source of truth — derive `focusedPane` from the
existing flags (right pane is focused whenever `inDetail || inBlackboard ||
inEventView || inSegmentView`, else left).

**Width accounting is the risk.** The view builds lines by hand and joins them with
`fitLine(...)+divChar+fitLine(...)` to keep exact column widths
(`agents_model.go:168-209`); it deliberately avoids `lipgloss.JoinHorizontal`. A box
border consumes 2 interior columns per panel (left+right border) and 2 rows
(top+bottom). The implementer must reduce the content width/height budget passed to
`buildTreeLines`/`buildDetailLines`/`buildBlackboardLines` accordingly and render the
border either by (a) switching to `lipgloss.Border` per panel and re-joining, or
(b) drawing border glyphs manually within the existing fit-line scheme. Option (b)
preserves the current exact-width invariant with least churn and is recommended;
whichever is chosen, the existing "no line exceeds pane width" guarantee must hold.
The center `│` divider can be subsumed by the two adjacent borders or kept — the
implementer picks whichever renders cleanest at the 40/60 split.

### Decision 2 — Wheel scrolls the focused panel's offset (Phase 1)

Rework `handleMouse` (`agents_model.go:358-394`) so wheel-up/down adjusts the
**scroll offset of the focused pane**, not a selection and not the pane under the
pointer:

- Left focused → adjust `m.treeScroll`.
- Right focused, detail/event list → adjust `m.detailScroll`.
- Right focused, expanded event → adjust `m.eventScroll` (already does this).
- Right focused, **blackboard → adjust `m.bbScroll`** (the missing case; this is the
  "blackboard breaks scrolling" fix).
- Right focused, segment view → adjust its offset analogously.

Scroll step stays 3 lines (current). Each offset must be clamped to
`[0, max(0, len(body)-visibleRows)]` on every wheel event so the view can't scroll
past content in either direction — the tree/detail panes currently clamp implicitly
via selection-follow, which disappears once the wheel stops moving the selection, so
explicit clamping is required. Note the behavioral change in the PR: the wheel no
longer moves the selection cursor; `j`/`k` (and arrows) remain the selection keys.

Cursor-position routing (`msg.X` hit-testing) is explicitly **out of scope** — the
user chose "scroll focused panel only." Keyboard focus (Tab / existing pane-switch
keys) selects which panel the wheel drives. If no explicit pane-switch key exists
today, add a `tab` binding to toggle left/right focus and document it in the overlay
help/footer.

### Decision 3 — Blackboard grouped by writer with sticky headers (Phase 2)

Rebuild `buildBlackboardLines` (`agents_model.go:664-746`) to present the snapshot
grouped by writer:

- Read `Snapshot()` and bucket entries by `WriterLabel` (fall back to `WriterID`,
  then `"(unknown)"` when both are empty).
- Order groups by the writer's earliest/most-recent `WrittenAt` (recommend: most
  recent group first, so fresh coordination is at the top) — deterministic and
  stable; document the chosen order.
- Within a group, list keys sorted (keep alphabetical within a writer for
  stability).
- Render a **sticky per-writer header** — the header line for the group the top of
  the viewport currently sits in stays pinned at the top of the pane as the body
  scrolls (classic sticky-section-header behavior). Implement by tracking which
  group the first visible body line belongs to and prepending that group's header
  when it would otherwise have scrolled off. Keep it simple: one pinned header line;
  no nested pinning.

### Decision 4 — Contrast + separators (Phase 2)

- **Values:** render in the normal foreground (`colNormal` `#abb2bf`), not muted, so
  they're readable. Keep keys accented (`colCyan` bold) and the "wrote by"/meta
  labels muted (`colMuted`).
- **Separators:** insert a faint rule (muted `─`) or a blank line between entries
  within a group, and a stronger visual break between writer groups (the sticky
  header itself serves as the group break).

### Decision 5 — Pretty-printed JSON values (Phase 2)

Replace the single-line `encodeBlackboardValue` output with **indented (pretty)
JSON** (`json.MarshalIndent`, 2-space) for object/array values; scalars (string,
number, bool) print inline next to the key. No markdown rendering — values are shown
as uniform, indented JSON (the user chose "Pretty JSON only"). Pretty-printed lines
still pass through the existing `sanitizeDisplay` + `wrapToWidth`
(`agents_model.go:708-709`, `761-767`) so no line exceeds pane width; indentation is
preserved by wrapping the already-indented lines individually rather than
re-flowing them.

### Decision 6 — Next/prev writer-group navigation (Phase 2)

Add keybindings (recommend `n`/`p`, or `]`/`[`) that, when the blackboard is
focused, jump `m.bbScroll` to the first line of the next / previous **writer group**
(not next entry). This gives the "scroll to next/prev responses" behavior over the
grouped view. Document the keys in the overlay footer/help.

## Affected code (all in `internal/tui/`)

- `agents_model.go`
  - `View()` (`165-220`) — border rendering + width/height budget adjustment.
  - `handleMouse()` (`358-394`) — offset-based, focused-pane routing incl. the new
    blackboard + segment cases and explicit clamping.
  - `buildTreeLines`/`buildDetailLines`/`buildBlackboardLines` — accept reduced
    content dimensions; blackboard rebuilt for grouping/sticky/contrast/pretty-JSON.
  - focus-derivation helper + `tab` (and `n`/`p`) key handling in `Update()`.
- `theme.go` (`6-12`) — reuse existing colors; add an accent-border style if needed.
- `shell_model.go` (`526-530`) — mouse forwarding unchanged (already forwards all
  `tea.MouseMsg` to the agents model); verify no regression.

## Testing

Existing TUI harness/golden-style tests already cover this area
(`blackboard_scroll_test.go`, `blackboard_tab_test.go`, `blackboard_live_test.go`,
`human_route_screenshot_test.go`, `harness_test.go`). New/updated tests:

- **Focus indicator:** golden/screenshot test asserting the focused panel's border
  uses the accent style and the unfocused one muted, for both left- and right-focus.
- **Wheel scroll:** feed `tea.MouseMsg{Button: WheelUp/WheelDown}` and assert the
  **focused pane's offset** changes (not selection); assert clamping at both ends;
  **assert wheeling over the blackboard moves `bbScroll` and NOT `m.selected`** (the
  explicit regression test for the reported bug), updating/extending
  `blackboard_scroll_test.go`.
- **Blackboard render:** table/golden tests for grouping-by-writer, sticky header
  pinning as the body scrolls, normal-contrast values, entry separators, and
  pretty-printed JSON for a nested value; scalar-inline rendering.
- **Next/prev nav:** assert `n`/`p` move `bbScroll` to adjacent writer-group starts
  and clamp at the ends.

## Open questions (resolve at build time)

- Exact border-render technique (manual glyphs vs. `lipgloss.Border` re-join) —
  Decision 1 recommends manual glyphs to preserve the exact-width invariant, but the
  implementer may switch to `lipgloss.Border` if the width math is cleanly
  refactored.
- Group ordering (most-recent-first vs. earliest-first) — recommend most-recent-first;
  confirm against how debate/ensemble output reads in practice.
- Whether `tab` already has a binding in the overlay that should be reused vs.
  introduced.
