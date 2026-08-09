<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0041 — Agents split-panel UX — focus indicator, reliable scrolling, blackboard readability](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0041-agents-panel-ux.md)**
<!-- docket:backlink:end -->

# Agents split-panel UX — results

Change: #0041 · Branch: feat/agents-panel-ux · PR: <set on open> · Plan: docs/superpowers/plans/2026-08-09-agents-panel-ux.md · ADRs: none

## Verify (human)

Automated tests cover focus color, width invariant, wheel-scroll offsets, blackboard
grouping/sticky/contrast/pretty-JSON, and n/p nav. Optional visual confirmation at the
merge gate (not required — the golden/color tests assert the rendered ANSI):

- [ ] Open the agents overlay (`/agents` or Tab); confirm the focused panel shows an
      accent (cyan) border+title and the unfocused panel a muted border; `tab` flips it.
- [ ] Mouse-wheel over each pane (tree, detail, blackboard) and confirm the wheel scrolls
      the focused pane's content — including the blackboard — and does NOT move the
      selection cursor.
- [ ] Open the blackboard (`b`) with multi-writer entries; confirm entries are grouped
      under sticky per-writer headers, values are legible (normal contrast), JSON values
      pretty-print, and `n`/`p` jump between writer groups.

## Findings

- **Whole-branch review returned no blocking findings.** The central width/height
  accounting (manual border glyphs inside the existing `fitLine`+`divChar`+`fitLine`
  join, no `lipgloss.JoinHorizontal`) holds the exact-width invariant; enforced by
  `TestFocusWidthInvariant` across widths 80/100/120 in all focus states.
- **Three should-fix items were found and fixed in this change** (commit `e3c3613`):
  1. Blackboard bottom-scroll now clamps to `max(0, len(body)-visibleRows)` (was
     `len(body)-1`), matching spec Decision 2 and the detail pane — no more over-scroll
     into a near-empty pane. New test `TestBlackboardBottomClampToLastFullWindow`.
  2. `treeManual`/`detailManual` (wheel-suppresses-selection-follow) flags are now reset
     on pane enter/exit, so re-entering detail correctly follows the tail. New test
     `TestDetailReentryFollowsTailAfterWheel`.
  3. Corrected the now-accurate "sticky" comments on those flags.
  Plus a gofmt cleanup of the touched files.
- No non-obvious *implementation* decision arose beyond what the spec's Decisions 1–6
  already record, so **no ADR was minted**.

## Follow-ups

- **Deep-nested pretty-JSON in a very narrow pane** may lose leading indentation on
  wrapped continuation rows (`wordwrap.String` trims leading space); still width-safe,
  just a fidelity edge. Tests cover shallow nesting only. Minor; a candidate future
  polish, not filed as a change (auto-capture is disabled in this repo).
- A time-ordered / toggle-able blackboard transcript lens remains explicitly out of
  scope (noted in the spec as a possible future).
