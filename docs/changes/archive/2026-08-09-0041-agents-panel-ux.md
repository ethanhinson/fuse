---
id: 41
slug: agents-panel-ux
title: Agents split-panel UX — focus indicator, reliable scrolling, blackboard readability
status: done
priority: medium
type: feat
created: 2026-08-09
updated: 2026-08-09
depends_on: []
related: []
discovered_from: []
adrs: []
spec: docs/superpowers/specs/0041-agents-panel-ux.md
plan: docs/superpowers/plans/2026-08-09-agents-panel-ux.md
results: docs/results/2026-08-09-agents-panel-ux-results.md
trivial: false
auto_groomable:
branch: feat/agents-panel-ux
claimed_at: 
pr: https://github.com/ethanhinson/fuse/pull/44
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [0041-agents-panel-ux.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/0041-agents-panel-ux.md) |
| Plan | [2026-08-09-agents-panel-ux.md](https://github.com/ethanhinson/fuse/blob/feat/agents-panel-ux/docs/superpowers/plans/2026-08-09-agents-panel-ux.md) |
| Results | [2026-08-09-agents-panel-ux-results.md](https://github.com/ethanhinson/fuse/blob/feat/agents-panel-ux/docs/results/2026-08-09-agents-panel-ux-results.md) |
| PR | [#44](https://github.com/ethanhinson/fuse/pull/44) |
<!-- docket:artifacts:end -->

## Why

The agents overlay renders a two-panel split — a subagent tree/output list on the
left and a detail/blackboard pane on the right (`internal/tui/agents_model.go`). It
has three concrete usability defects, each confirmed in code:

1. **You can't tell which panel is selected.** Panel focus lives only in state flags
   and is never rendered — no border, no title styling. The only highlight is on the
   selected *row*, not the active *panel*.
2. **Mouse-wheel scrolling is unreliable.** The wheel handler ignores cursor
   position and, for the list panes, moves the *selection cursor* instead of a scroll
   offset — so wheeling drags the cursor around rather than scrolling the view.
3. **The blackboard breaks scrolling and is hard to read.** The wheel handler has no
   blackboard case at all, so wheeling over the blackboard scrolls the *tree* instead.
   Its values render in a low-contrast muted color, entries are ungrouped and
   unseparated, and JSON values dump on a single line.

The blackboard is the shared coordination surface agents use for ensemble/debate/
grading (change 0023). Each entry already records its writer and timestamp, so a
clear "who said what" view is achievable purely in the TUI render path — no data-model
change. Making this pane legible directly improves the operator's ability to follow
multi-agent coordination.

## What changes

A TUI-only change to `internal/tui/`, in two phases within one PR:

- **Phase 1 — interaction fixes.** A colored-border focus indicator (accent border +
  title on the focused panel, muted on the unfocused). Rework the mouse-wheel handler
  so the wheel scrolls the *focused panel's content offset* — including a proper
  blackboard case — instead of moving the selection, with explicit clamping. Add/confirm
  a keyboard focus-switch (`tab`).
- **Phase 2 — blackboard readability.** Group blackboard entries by writer with a
  sticky per-writer header; raise value contrast to the normal foreground; add
  separators between entries; pretty-print JSON values; and add next/prev navigation
  that jumps between writer groups.

Design detail, exact code locations, and the width-accounting risk are in the linked
spec.

## Out of scope

- Any change to the blackboard data model or store API (`internal/agent/blackboard.go`,
  `internal/tools/blackboard.go`) — presentation and input routing only.
- Cursor-position (`msg.X`) hit-testing for the wheel — the wheel drives the *focused*
  panel, not the panel physically under the pointer.
- A full markdown renderer for blackboard values — values pretty-print as JSON.
- A time-ordered / toggle-able transcript lens (noted as a possible future).
- Writer-based access control on blackboard keys.

## Open questions

- Border render technique: manual glyphs (preserves the exact-width invariant) vs.
  `lipgloss.Border` with refactored width math — recommended manual, decided at build.
- Writer-group ordering: most-recent-first (recommended) vs. earliest-first.
- Whether a `tab` focus-switch binding already exists in the overlay to reuse.

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->

### 2026-08-09

Reconciled against current `origin/main`. All three defects and every code
citation in the spec verified against live code — no drift:

- `internal/tui/agents_model.go`: `View()` (168-220) still hand-joins with
  `fitLine(...)+divChar+fitLine(...)` (the exact-width invariant the spec's
  width-accounting risk turns on); `divChar` muted at 205. `handleMouse()`
  (358-394) confirmed to move **selection** (`m.selected`/`m.eventSel`) for the
  list panes and to have **no `inBlackboard` case** (falls to `default:` tree),
  exactly the reported bugs. `buildBlackboardLines()` (664-746) still sorts by
  key with no writer grouping, renders values via `wroteStyle` (muted
  `colMuted`) and single-line `encodeBlackboardValue` (`json.Marshal`).
- State flags `inDetail`/`inBlackboard`/`inEventView`/`inSegmentView` and offsets
  `treeScroll`/`detailScroll`/`eventScroll`/`bbScroll` all present as cited.
- `internal/agent/blackboard.go`: `BlackboardEntry` still carries `WriterID`,
  `WriterLabel`, `WrittenAt`; `Snapshot()` returns `map[string]BlackboardEntry`
  (117-125). No data-model change needed, as the spec assumes.
- `internal/tui/theme.go`: palette matches — `colCyan #56b6c2` (accent),
  `colNormal #abb2bf` (target for readable values), `colMuted #5c6370`.
- `internal/tui/shell_model.go` (526-530) still forwards all `tea.MouseMsg` to
  the agents model — no forwarding change required.

**Open-question resolution.** Spec Decision 2 / open question "does a `tab`
focus-switch already exist" — **yes**: `tab` already toggles left↔right
(tree→detail at 241; detail/blackboard/event/segment→tree at 270/312/325/348).
The build reuses the existing `tab` for focus rather than introducing a new
binding; the wheel-routing rework keys off the same derived left/right focus.

No scope change, no obsolescence — design fully valid. Proceeding to plan/build.
