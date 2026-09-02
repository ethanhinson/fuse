---
id: 54
slug: charm-table-tabs-as-shared-tui-component-layer
title: Adopt charmbracelet table/tabs as the shared TUI component layer
status: Accepted
date: 2026-09-01
supersedes: []
reverses: []
relates_to: []
change: 80
---

## Context

`fuse shell` had four private, divergent column renderers, each re-implementing the same
`lipgloss.Width` + pad-cells algorithm: 33 non-test call sites across
`internal/tui/agents_model.go` (15), `models_command.go` (10), `slash_completer.go` (6),
and `models_editor.go` (2).

That duplication is not merely untidy — it made a real defect possible. Change 0078 shipped a bug
in the slash completer where a column padded to a registry-wide global max was not clamped against
the render width. One off-screen over-wide MCP entry (`/mcp:<server>/<tool>`) made every overlay row
over-wide, and because the shell composites that overlay through wordwrap, the wrap broke at a space
inside the padding and spilled the kind tag and description onto a second line for every row. Each
renderer had to learn that lesson separately.

There was also no structured surface for grouped settings: the models surface was split across
`/models` and a `/models edit` modal, with no home for permissions or MCP beyond scattered slash
commands.

## Decision

Adopt charmbracelet's table/tabs packages — already pinned
(`lipgloss v1.1.1-0.20250404203927-76690c660834`, `bubbles v1.0.0`; no dependency bump) — as the
shared TUI component layer, behind two fuse-owned primitives in `internal/tui`:

- **`table.go`** — a static table primitive (`RenderTable`/`Column`/`Row`) that owns display-cell
  measurement on unstyled text, global-max column widths **clamped to the render width**, truncation
  in display cells reserving the ellipsis, blank cells that hold their column, a trailing tag column,
  and a width-neutral active marker. `padCells`/`truncateCells` are consolidated here as the single
  implementation.
- **`tabs.go`** — a tabbed container (`Pane`/`Tabs`) whose separator glyphs are paid for out of the
  same width budget rather than added around it.

Backend split: `lipgloss/table` backs static listings; `bubbles/table` backs surfaces that want
selection and scroll (the `/config` Models tab). One fuse-facing API over both.

**The boundary:** new menus use the primitives. The named legacy surfaces —
`internal/tui/agents_model.go`'s tree, the `/queue` editor, and the `/approvals` list — keep their
current rendering and migrate in a follow-up sweep; they are deliberately not migrated under this
change.

The render-width clamp is designed **into** the primitive rather than left to each call site, so the
0078 failure mode cannot recur per-surface.

## Consequences

- One consistent look and one place to fix an alignment bug; change 0080 consolidated 18 of the 33
  sites.
- A deprecation path for the hand-rolled renderers, with the remaining 15 sites in `agents_model.go`
  named as follow-up work.
- A dependency-surface commitment to charm's table/tabs packages; their upgrade cadence now touches
  every menu rather than none.
- The primitive's width clamp trades styling for fit: a truncated cell must be cut as one composed
  plain string because ANSI escapes are not display cells, so a truncated row loses per-segment
  styling. That is an accepted trade for a row that fits.
- Two surfaces (the `/models` listing and the `/models edit` overlay) still pass width 0 into the
  primitive as of the initial change; threading their real widths through is tracked as review
  follow-up, not as a property of this decision.
