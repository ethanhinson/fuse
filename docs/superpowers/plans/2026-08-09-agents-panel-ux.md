<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0041 — Agents split-panel UX — focus indicator, reliable scrolling, blackboard readability](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0041-agents-panel-ux.md)**
<!-- docket:backlink:end -->

# Plan 0041 — Agents split-panel UX

> Change 0041 — agents-panel-ux. Spec: `docs/superpowers/specs/0041-agents-panel-ux.md`
> (on the `docket` metadata branch).

TUI-only change in `internal/tui/`. Two phases in one PR: Phase 1 = focus indicator
+ focused-pane wheel scrolling; Phase 2 = blackboard readability. Method:
subagent-driven-development / TDD — each task writes a failing test, implements the
minimum to pass, verifies, and self-reviews before the next task.

## Guardrails (load-bearing — apply to every task)

1. **Width accounting is THE risk.** `View()` (`agents_model.go:168-220`) builds each
   row by hand as `fitLine(treeLines[i], treeW) + divChar + fitLine(detailLines[i], detailW)`
   and deliberately avoids `lipgloss.JoinHorizontal`. Any border added by Phase 1
   MUST reduce the width/height budget passed to `buildTreeLines`/`buildDetailLines`/
   `buildBlackboardLines` by exactly the columns/rows the border consumes, so the
   existing invariant "no rendered line exceeds its pane width" still holds. Draw the
   border with manual glyphs inside the existing fit-line scheme (spec Decision 1
   option b) — do not switch to `lipgloss.JoinHorizontal`.
2. **Sanitize + wrap every model/tool-controlled string** (learning
   `sanitize-untrusted-bytes-fixed-width-tui`). Blackboard keys, writer labels, and
   values already flow through `sanitizeDisplay` + `wrapToWidth`; keep that.
   Pretty-printed JSON (Phase 2) must wrap **each already-indented line individually**
   (preserve indentation; never re-flow the whole blob), and no border row may exceed
   pane width.
3. **Screenshot/golden tests** assert against the final model's `View()` with
   `lipgloss.SetColorProfile(termenv.TrueColor)` forced (learning
   `teatest-final-frame-via-finalmodel-view`; the existing `blackboard_scroll_test.go`
   header already does this — copy that preamble). Assert on the presence of the
   accent color code (`colCyan` = `#56b6c2`) vs muted (`colMuted` = `#5c6370`) in the
   rendered border, not on pixels.
4. **`tab` already toggles left/right focus** (tree→detail at `agents_model.go:241`;
   detail/blackboard/event/segment→tree at 270/312/325/348). Reuse it. Derive a
   left/right `focusedPane` from the EXISTING flags — right is focused whenever
   `inDetail || inBlackboard || inEventView || inSegmentView`, else left. Do NOT add a
   redundant focus source of truth.
5. **Do NOT touch** `internal/agent/blackboard.go` or `internal/tools/blackboard.go`.
   Presentation + input routing only. `Snapshot()` returns
   `map[string]BlackboardEntry`; each entry carries `WriterID`, `WriterLabel`,
   `WrittenAt`.

## Phase 1 — interaction fixes

### Task 1 — Derived `focusedPane` helper

- **Test** (`internal/tui/agents_focus_test.go`, new): construct an `AgentsModel`;
  assert a helper `m.rightFocused()` (bool) returns `false` on the fresh tree view and
  `true` after entering detail (`tab`), blackboard (`b`), event view, and segment view;
  returns to `false` after `tab`/`esc` back to tree.
- **Implement**: add `func (m *AgentsModel) rightFocused() bool { return m.inDetail || m.inBlackboard || m.inEventView || m.inSegmentView }` to `agents_model.go`. No new state field.
- **Verify**: `go test ./internal/tui/ -run TestFocus`.

### Task 2 — Colored-border focus indicator with corrected width/height budget

- **Test** (`internal/tui/agents_focus_test.go`): render `View()` at a fixed size
  with `termenv.TrueColor` forced. Assert (a) the focused pane's border carries the
  accent color `#56b6c2` and the unfocused pane's border carries `#5c6370`, for BOTH
  left-focus (tree) and right-focus (after `tab`); (b) **width invariant** — every
  visible line's display width (via `lipgloss.Width`) equals the full overlay width
  and no `buildTreeLines`/`buildDetailLines` content line exceeds its reduced pane
  width. Add a helper in the test that strips ANSI and checks `runewidth` per line.
- **Implement** in `agents_model.go` `View()`:
  - Compute `treeContentW = treeW - 2` and `detailContentW = detailW - 2` (left+right
    border glyph per pane) and a content height `h - 2` (top+bottom border rows);
    floor each at 1. Pass the reduced dims to `buildTreeLines`/`buildDetailLines`
    (they already take a width arg; thread the reduced height through `m.height`
    exactly as the existing scheduler-header shrink does).
  - Wrap each pane's content lines with manual border glyphs (`┌─┐ │ └─┘`) styled by
    `lipgloss.NewStyle().Foreground(accent|muted)`, where `accent = colCyan` for the
    focused pane (`m.rightFocused()` picks left vs right) and `muted = colMuted`
    otherwise. Render the pane title in the border top row in the same color.
  - Keep the join shape `fitLine(left,treeW)+divChar+fitLine(right,detailW)`; the
    border glyphs live inside each `fitLine` budget. The center `│` divider may be
    kept or subsumed by the two adjacent borders — pick whichever renders cleanest at
    40/60 and keep total width exact.
- **Verify**: `go test ./internal/tui/ -run TestFocus`; eyeball via the existing
  harness if convenient.

### Task 3 — Wheel scrolls the focused pane's OFFSET (not selection)

- **Test** (extend `internal/tui/blackboard_scroll_test.go` + new
  `internal/tui/wheel_scroll_test.go`):
  - Feed `tea.MouseMsg{Button: tea.MouseButtonWheelDown}` / `WheelUp` and assert the
    **focused pane's offset** moves: left-focus → `m.treeScroll`; right-focus detail
    list → `m.detailScroll`; expanded event → `m.eventScroll`; **blackboard →
    `m.bbScroll`**; segment view → its offset. Assert `m.selected`/`m.eventSel` do
    NOT change on a wheel event.
  - **Regression test (the reported bug):** with the blackboard focused, wheel down N
    times and assert `m.bbScroll` increased and `m.selected` is unchanged.
  - Assert clamping at both ends: wheel up at top keeps offset ≥ 0; wheel down past
    content clamps at `max(0, len(body)-visibleRows)`.
- **Implement** in `agents_model.go` `handleMouse()` (`358-394`): replace the
  selection-moving branches with offset adjustment keyed off focus/flags. Step stays
  3. Clamp each offset to `[0, max(0, len(body)-visibleRows)]` on every event
  (compute `len(body)` the same way the corresponding `build*Lines` does, or clamp at
  render as bb already does and additionally floor at 0 here). Add the missing
  `m.inBlackboard` case (→ `m.bbScroll`) and the `m.inSegmentView` case. Leave `j`/`k`
  / arrows as the selection keys in the key handlers.
- **Verify**: `go test ./internal/tui/ -run 'TestWheel|TestBlackboard'`.

## Phase 2 — blackboard readability

### Task 4 — Group blackboard by writer + sticky per-writer header

- **Test** (`internal/tui/blackboard_render_test.go`, new): build a blackboard with
  entries from two writers (`alice`, `bob`) written at distinct `WrittenAt` times via
  `bb.Put(key, val, id, label)`. Assert `buildBlackboardLines` output: (a) entries are
  grouped under a per-writer header showing the writer label; (b) group order is
  most-recent-writer-first; (c) keys sorted alphabetically within a group; (d) a
  writer with empty label/id renders under `(unknown)`. Sticky-header test: set a
  small height so the body scrolls, advance `bbScroll` past the first group's header,
  and assert the current group's header line is still pinned at the top of the window.
- **Implement**: rebuild `buildBlackboardLines` (`664-746`): after `Snapshot()`,
  bucket by `WriterLabel` (fallback `WriterID`, then `(unknown)`); order groups by the
  group's max `WrittenAt` descending; sort keys within a group. Emit a header line per
  group (styled), then the group's entries. Track which group the first visible body
  line belongs to and, when its header has scrolled above the window top, prepend that
  header as a single pinned line (one pinned header; no nested pinning). Keep the
  existing header/rule/help row accounting and `bbScroll` clamping.
- **Verify**: `go test ./internal/tui/ -run TestBlackboard`.

### Task 5 — Contrast + separators

- **Test** (extend `blackboard_render_test.go`): assert value lines carry `colNormal`
  (`#abb2bf`), key lines carry `colCyan` (`#56b6c2`) bold, and meta/"written by"
  labels carry `colMuted` (`#5c6370`); assert a separator (blank line or muted `─`)
  appears between entries within a group.
- **Implement**: in `buildBlackboardLines`, switch the value style from the muted
  `wroteStyle` to a `colNormal` style; keep the key `colCyan` bold; keep meta muted;
  insert the entry separator. Keep the group break carried by the sticky header.
- **Verify**: `go test ./internal/tui/ -run TestBlackboard`.

### Task 6 — Pretty-printed JSON values

- **Test** (extend `blackboard_render_test.go`): a nested object value (e.g.
  `map[string]any{"a": map[string]any{"b": 1}}`) renders as multi-line indented JSON
  (assert a line contains 2-space indentation and the nested key on its own line); a
  scalar value (string/number/bool) renders inline next to the key. Assert each
  produced line still fits pane width (pass a narrow width and check `lipgloss.Width`).
- **Implement**: replace `encodeBlackboardValue`'s single-line `json.Marshal` path for
  object/array values with `json.MarshalIndent(v, "", "  ")`; scalars stay inline.
  Split the pretty output on `\n` and pass **each line** through `sanitizeDisplay` +
  `wrapToWidth(line, w)` (preserve indentation; wrap each line individually rather than
  re-flowing the blob). Guardrail 2.
- **Verify**: `go test ./internal/tui/ -run TestBlackboard`.

### Task 7 — Next/prev writer-group navigation

- **Test** (extend `blackboard_render_test.go` or `blackboard_tab_test.go`): with the
  blackboard focused and ≥3 writer groups, `n` moves `bbScroll` to the next group's
  first line and `p` to the previous group's first line; both clamp at the ends
  (first group's `p` stays at first, last group's `n` stays at last).
- **Implement**: in `handleBlackboardKey`, add `n`/`p` cases that compute the body-line
  index of each writer-group's first line (reuse the Task 4 grouping) and set
  `m.bbScroll` to the adjacent group start, clamped. Document `n`/`p` in the blackboard
  help line (`buildBlackboardLines` `help`).
- **Verify**: `go test ./internal/tui/ -run TestBlackboard`.

## Final task — full-suite gate

- Run `go build ./...` then `go test ./...` (at minimum `go test ./internal/tui/...`).
  All existing tests (`blackboard_scroll_test.go`, `blackboard_tab_test.go`,
  `blackboard_live_test.go`, `blackboard_tui_e2e_test.go`, `human_route_screenshot_test.go`,
  `harness_test.go`) must stay green — the wheel behavior change (wheel no longer moves
  selection) may require updating any existing test that asserted the old behavior;
  update those tests to the new offset semantics and note the behavioral change in the
  PR body.
