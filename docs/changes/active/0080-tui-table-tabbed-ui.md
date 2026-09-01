---
id: 80
slug: tui-table-tabbed-ui
title: Shared TUI table component + tabbed /config UI — line up the menus like Claude
status: in-progress
priority: medium
type: feat
created: 2026-09-01
updated: 2026-09-01
depends_on: []
related: [10, 78, 79]
discovered_from: [79]
adrs: []
spec: docs/superpowers/specs/2026-09-01-tui-table-tabbed-ui-design.md
plan:
results:
trivial: false
auto_groomable:
branch: feat/tui-table-tabbed-ui
claimed_at: 2026-09-01T23:46:08Z
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-09-01-tui-table-tabbed-ui-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-09-01-tui-table-tabbed-ui-design.md) |
<!-- docket:artifacts:end -->

## Why

`fuse shell`'s menus don't line up. Every listing measures and pads its columns
its own way — the `/models` listing (0078), the models editor (0079), and the
slash-command completer (0078's alignment pass) each re-implement the same
`lipgloss.Width` + pad-cells logic, ~34 ad-hoc column-render sites in all. The
`/models` table goes visibly ragged whenever a persona is blank, because the
persona column is positional and a missing value leaves nothing to hold the gap.

We want the menus to read like Claude Code's: aligned tables and a tabbed
container for grouped settings. That means one shared table primitive instead of
N hand-rolled renderers, and a tabbed UI so related views (models, permissions,
MCP) live under one organized surface instead of scattered slash commands.

## What changes

- A **shared table component** (built on charmbracelet `lipgloss`/`bubbles`
  `table`, already vendored) that owns column measurement, alignment, header
  styling, active-row marking, trailing tags, width clamping, and blank-cell
  handling.
- A **shared tabbed-container primitive** (tab bar + active-pane routing).
- Adopt the table component in the **`/models` listing**, the **slash-command
  menu**, and the **`/models edit` list view** so all three line up.
- A new **`/config` tabbed screen** (Models | Permissions | MCP) hosting the
  models table as its first tab — the concrete surface that exercises the tabbed
  primitive.
- An **ADR** recording the decision to adopt charm's table/tabs as the shared TUI
  component layer and the boundary that replaces the ad-hoc renderers.

## Out of scope

- Migrating every ad-hoc renderer now — the `/agents` tree, `/queue` editor, and
  `/approvals` list keep their current rendering (candidate follow-up sweep).
- Any change to model resolution or a live provider-API model list; this is
  presentation only.
- Replacing the overlay/keyboard-routing model — the table and tabs plug into the
  existing `ShellModel` overlay pattern.

## Open questions

- Tab-switch key bindings vs. the existing completer/overlay key precedence.
- `lipgloss/table` (static) vs. `bubbles/table` (interactive) per surface, under
  one fuse-facing API.
- Whether the Permissions and MCP tabs are read-only in this change or gain edit
  actions here.
