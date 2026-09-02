---
id: 80
slug: tui-table-tabbed-ui
title: Shared TUI table component + tabbed /config UI — line up the menus like Claude
status: done
priority: medium
type: feat
created: 2026-09-01
updated: 2026-09-02
depends_on: []
related: [10, 78, 79]
discovered_from: [79]
adrs: [54]
spec: docs/superpowers/specs/2026-09-01-tui-table-tabbed-ui-design.md
plan: docs/superpowers/plans/2026-09-01-tui-table-tabbed-ui-plan.md
results: docs/results/2026-09-01-tui-table-tabbed-ui-results.md
trivial: false
auto_groomable:
branch: feat/tui-table-tabbed-ui
claimed_at: 
pr: https://github.com/ethanhinson/fuse/pull/85
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-09-01-tui-table-tabbed-ui-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-09-01-tui-table-tabbed-ui-design.md) |
| Plan | [2026-09-01-tui-table-tabbed-ui-plan.md](https://github.com/ethanhinson/fuse/blob/feat/tui-table-tabbed-ui/docs/superpowers/plans/2026-09-01-tui-table-tabbed-ui-plan.md) |
| Results | [2026-09-01-tui-table-tabbed-ui-results.md](https://github.com/ethanhinson/fuse/blob/feat/tui-table-tabbed-ui/docs/results/2026-09-01-tui-table-tabbed-ui-results.md) |
| PR | [#85](https://github.com/ethanhinson/fuse/pull/85) |
| ADRs | [ADR-0054](https://github.com/ethanhinson/fuse/blob/docket/docs/adrs/0054-charm-table-tabs-as-shared-tui-component-layer.md) |
<!-- docket:artifacts:end -->

## Why

Every `fuse shell` listing measures and pads its columns its own way — the
`/models` listing (0078), the models editor (0079), the slash-command completer
(0078's alignment pass), and the `/agents` tree each re-implement the same
`lipgloss.Width` + pad-cells logic. Confirmed at reconcile: **33 non-test
`padCells` / `truncateCells` / `lipgloss.Width` call sites**, concentrated in
`internal/tui/agents_model.go` (15), `models_command.go` (10),
`slash_completer.go` (6), and `models_editor.go` (2). Four private, divergent
copies of one algorithm.

**Correction (reconcile, 2026-09-01):** the original "ragged when a persona is
blank" premise no longer holds. `renderModelsListing` pads the persona column
with `padCells` and only trims the tail when no trailing tag follows, so the tag
column stays aligned across blank personas; `models_command.go` additionally
carries a `personaCell` dash substitute. The motivation is therefore
**consolidation and a home for grouped settings**, not an open alignment bug —
the raggedness was already closed by 0078/0079. The cost being paid today is
duplication: four renderers that must each be fixed, styled, and tested
separately, with no shared surface for the next menu.

We want the menus to read like Claude Code's: aligned tables and a tabbed
container for grouped settings. That means one shared table primitive instead of
N hand-rolled renderers, and a tabbed UI so related views (models, permissions,
MCP) live under one organized surface instead of scattered slash commands.

## What changes

- A **shared table component** (built on charmbracelet `lipgloss`/`bubbles`
  `table`, already vendored) that owns column measurement, alignment, header
  styling, active-row marking, trailing tags, width clamping, and blank-cell
  handling. `lipgloss/table` and `bubbles/table` are both present in the module
  cache at the pinned versions (`lipgloss v1.1.1-0.20250404203927`,
  `bubbles v1.0.0`) — no dependency bump needed.
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

- Tab-switch key bindings vs. the existing completer/overlay key precedence —
  settled at build against `shell_model.go`'s key-routing guard order.

**Settled at reconcile (2026-09-01):**

- *`lipgloss/table` vs `bubbles/table`* — use `lipgloss/table` for the static
  listings (`/models` output, slash-completer menu) and `bubbles/table` only for
  the `/config` Models tab, which wants selection and scroll. One fuse-facing
  API over both.
- *Permissions and MCP tab depth* — **read-only in 0080.** `/mode` already
  exists as the permissions surface to mirror, but there is **no `/mcp` builtin
  at all** (`shell_model.go`'s `handleSlash` switch registers `/agents`,
  `/blackboard`, `/approvals`, `/queue`, `/queuedemo`, `/questions`, `/verbose`,
  `/models`, `/model`, `/mode`, `/exit`), so the MCP tab renders configured
  servers read-only. Edit actions are a follow-up.
- *Scope of adoption* — `agents_model.go`'s 15 sites stay out of scope as
  written; this change adopts the primitive in the three named surfaces only.

## Reconcile log

### 2026-09-01

Reconciled against `origin/main` @ 18243d0 and the docket backlog after the 0064
sweep. Related changes 0078 (`/models` listing + slash-menu alignment) and 0079
(models UI / `/model` autocomplete / interactive editor) are both merged and
`done`; 0010 is likewise closed. Nothing in this change's scope was built
elsewhere.

Findings that moved the change:

1. **The motivating bug is already fixed.** The change and spec both led with
   "the `/models` table goes visibly ragged whenever a persona is blank."
   Reading `internal/tui/models_command.go:32-88`, `renderModelsListing` pads
   the persona column via `padCells` and trims the tail only when no trailing
   tag follows (`:82-85`), so tag offsets stay aligned across blank personas;
   `personaCell` (`:148-158`) additionally substitutes a dash. 0078/0079 closed
   it. Body rewritten to drop the stale premise and re-anchor on consolidation.
   This is a scope adjustment, not an invalidation — the duplication and the
   missing grouped-settings surface are unchanged and still justify the work.
2. **Duplication count confirmed, and localized.** 33 non-test
   `padCells`/`truncateCells`/`lipgloss.Width` sites (spec said "~34"):
   `agents_model.go` 15, `models_command.go` 10, `slash_completer.go` 6,
   `models_editor.go` 2. `agents_model.go` holds the largest share and is
   explicitly out of scope, so this change consolidates 18 of 33.
3. **No prior art to fold in.** No table or tabs primitive exists in
   `internal/tui`; no `/config` builtin exists. The work is greenfield.
4. **Dependencies already satisfied.** `lipgloss/table` and `bubbles/table` are
   both present in the module cache at the pinned versions. No `go.mod` change.
5. **Two of three open questions settled** from current code (backend split per
   surface; read-only Permissions/MCP tabs, since no `/mcp` builtin exists to
   mirror). Only the tab-switch key binding stays open, to be settled at build
   against `shell_model.go`'s guard order.

Adjacent work observed and NOT captured (`auto_capture.enabled` is `false`, so
reported in prose only): migrating `agents_model.go`'s 15 render sites onto the
primitive, and adding edit actions to the `/config` Permissions and MCP tabs.

`reconciled: true`; scope otherwise unchanged.
