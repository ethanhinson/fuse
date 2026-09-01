<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0080 — Shared TUI table component + tabbed /config UI — line up the menus like Claude](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0080-tui-table-tabbed-ui.md)**
<!-- docket:backlink:end -->

# Design: shared TUI table component + tabbed UI (change 0080)

## Problem

`fuse shell`'s menus render columns with per-view, hand-rolled alignment. The
`/models` listing (change 0078) and the models editor (change 0079) each measure
column widths and pad cells independently; the slash-command completer (0078's
alignment pass) does the same a third way. The result is inconsistent and
fragile: the `/models` listing goes ragged the moment a persona is blank (its
persona column is positional, so a missing value shifts nothing into the gap and
the eye reads misalignment), and every new menu re-implements the same
`lipgloss.Width` + `padCells` dance. There are ~34 ad-hoc column-render call
sites across the TUI.

There is also no structured way to present *grouped* settings. The models
surface is split across two entry points (`/models` list, `/models edit` modal),
and there is no home for other configurable surfaces (permissions mode, MCP
servers) beyond scattered slash commands.

We want fuse's menus to line up cleanly and read like Claude Code's — aligned
tables and a tabbed container for grouped views.

## Goals

1. A **shared table primitive** for the TUI, built on charmbracelet's
   `lipgloss`/`bubbles` `table` packages (already in `go.mod`), that owns column
   measurement, alignment, header styling, active-row marking, and truncation.
2. A **shared tabbed-container primitive** (tab bar + active-pane routing) for
   grouped views.
3. Adopt the table primitive in three surfaces: the `/models` listing, the
   slash-command completer menu, and the `/models edit` editor's list view.
4. A new **`/config` tabbed screen** (Models | Permissions | MCP) that hosts the
   models table as its first tab and demonstrates the tabbed primitive on a real
   surface.
5. An **ADR** recording the decision to adopt charm's table/tabs and the
   shared-component boundary that replaces the ad-hoc renderers.

## Non-goals

- Migrating every ad-hoc column renderer at once. The `/agents` tree, `/queue`
  editor, and `/approvals` list keep their current rendering; they are candidates
  for a follow-up sweep, not part of this change.
- A live provider-API model list, or any new model-resolution behavior — this is
  presentation only.
- Replacing the existing overlay/keyboard-routing model. The table and tabs are
  render+state primitives that plug into the current `ShellModel` overlay
  pattern (as `/queue` and the models editor already do).

## Approach

### Table primitive

A small `internal/tui` component wrapping `lipgloss/table` (for static, styled
listings) and, where interactive selection/scroll is wanted, `bubbles/table`.
The fuse-facing API takes typed rows + column definitions (header, alignment,
optional min/max width, optional style-per-cell) and renders display-cell-correct
aligned output — folding the `padCells`/`truncateCells`/`lipgloss.Width` logic
into one place. It must support:

- an active-row marker (the existing `▸ ` glyph) and per-row styling,
- a trailing tag column (e.g. `(default, active)`),
- width clamping to the viewport (the completer already does this; the primitive
  inherits it),
- blank cells that hold their column (fixing the ragged persona case).

### Tabbed primitive

A tab-bar renderer + active-index state that routes key input to the active
pane. Panes are ordinary render funcs, so a tab can host a table, a form, or any
existing overlay body. Tab switching is keyboard-driven (e.g. left/right or a
number key); the exact bindings are settled during build against the existing
key-routing precedence.

### `/config` screen

A new builtin `/config` command opening a tabbed overlay:

- **Models** — the models table (Available list) with the editor's add/edit/remove
  actions available in-tab (folding `/models edit`'s capability into the tab, while
  keeping `/models edit` working as a direct entry point).
- **Permissions** — the session permission mode (surfacing what `/mode` shows).
- **MCP** — the configured MCP servers (read view initially).

The models table and slash menu adopt the primitive regardless of `/config`, so
the alignment fix lands even for users who never open `/config`.

### ADR

Produced at build time: "Adopt charmbracelet table/tabs as the shared TUI
component layer." Context = the ad-hoc-renderer proliferation and raggedness;
Decision = the primitive + the boundary (new menus use it; the named follow-up
surfaces migrate later); Consequences = one dependency-surface adoption, a
consistent look, and a deprecation path for the hand-rolled renderers.

## Open questions

- Tab-switch key bindings vs. the existing completer/overlay key precedence —
  settle against `handleKey`'s guard order at build time.
- `lipgloss/table` (static) vs. `bubbles/table` (interactive) per surface: the
  listing and slash menu are likely static; the `/config` Models tab and editor
  want selection/scroll. The build may use both; the primitive should present one
  fuse-facing API over whichever backs a given surface.
- Whether the Permissions and MCP tabs are read-only in 0080 or gain edit
  actions here vs. a follow-up.

## Testing

- Golden/alignment tests for the table primitive (blank cells hold columns;
  active marker; trailing tag; width clamp), mirroring the existing
  `models_command_test.go` column-offset assertions.
- Harness (teatest) captures for the `/models` listing, the slash menu, the
  `/config` screen, and tab switching — the same frame-capture approach used for
  0079, so the aligned result is visually verified, not just asserted on strings.
- Regression: `/models`, `/models edit`, and `/model` autocomplete keep working.
