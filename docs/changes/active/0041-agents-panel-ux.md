---
id: 41
slug: agents-panel-ux
title: Agents split-panel UX — focus indicator, reliable scrolling, blackboard readability
status: in-progress
priority: medium
type: feat
created: 2026-08-09
updated: 2026-08-09
depends_on: []
related: []
discovered_from: []
adrs: []
spec: docs/superpowers/specs/0041-agents-panel-ux.md
plan:
results:
trivial: false
auto_groomable:
branch: feat/agents-panel-ux
claimed_at: 2026-08-09T20:09:59Z
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [0041-agents-panel-ux.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/0041-agents-panel-ux.md) |
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
