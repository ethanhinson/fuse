<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0080 — Shared TUI table component + tabbed /config UI — line up the menus like Claude](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0080-tui-table-tabbed-ui.md)**
<!-- docket:backlink:end -->

# Shared TUI table component + tabbed /config UI — results

Change: #0080 · Branch: feat/tui-table-tabbed-ui · PR: (opened at close-out) ·
Plan: docs/superpowers/plans/2026-09-01-tui-table-tabbed-ui-plan.md · ADRs: 0054

## Verify (human)

Genuinely manual checks — the automated suite covers width invariants and key routing, but
nothing automated can judge whether the menus actually *look* aligned in a real terminal.

- [ ] Run `fuse shell` and type `/` — the slash menu columns line up, and stay aligned while
      scrolling past an MCP entry (`/mcp:<server>/<tool>`) that is wider than the pane.
- [ ] Resize the terminal narrow (~50 cols) with the slash menu open — rows must not wrap onto
      a second line. This is the change-0078 failure mode; it is the single most valuable
      manual check here.
- [ ] `/models` with at least one model that has no persona — the persona column holds and the
      trailing `(default, active)` tags line up across rows. Note the persona now renders as `-`
      rather than blank (see Findings).
- [ ] `/config` — the tab bar renders, `tab`/`shift+tab` cycle Models → Permissions → MCP and
      wrap, `1`/`2`/`3` jump directly, `esc` closes.
- [ ] `/config` Models tab with **more models than fit the pane** — arrow down past the bottom
      and confirm the highlighted row stays visible. This was a shipped blocker caught in review
      (finding #1); the delete action has no confirmation, so an invisible selection was
      genuinely dangerous.
- [ ] `/config` open, then Ctrl+C — the shell should now quit. (Only `/config` was changed;
      `/queue` and `/models edit` still swallow it, deliberately out of scope.)
- [ ] `/models edit` still works as a direct entry point, and its add/edit/remove reach the same
      handlers the `/config` Models tab uses.

## Findings

The whole-branch deep review returned **11 findings: 1 blocker, 2 important, 8 minor**. Nine were
fixed in-branch; two needed no fix. Full disposition is in the PR body. The three that carry a
lesson beyond this change:

- **Two of the three adopted surfaces initially opted out of the very protection the change
  exists to provide** (findings #2, #3) — both passed `width: 0` into the new primitive, leaving
  the `/models` listing to be laid out unbounded and then wordwrapped, and the editor overlay to
  be clipped from the right by `fitLine`. Consolidating a clamp behind a primitive does not
  deliver it; the width has to be *threaded through*. Both are fixed and now carry width tests.
- **The `/config` Models tab never scrolled its selection into view** (finding #1, blocker) —
  `bubbles/table`'s `SetCursor` updates `m.start` and calls `viewport.SetContent`, but only
  `MoveUp`/`MoveDown` move `YOffset`, so a table rebuilt per render kept the cursor permanently
  one row below the window. Fixed by windowing the rows explicitly rather than relying on
  bubbles/table's offset bookkeeping.
- **`/models` output changed visibly**: a blank persona now renders as `-` instead of an empty
  padded cell, because the dash moved from a call-site helper into the column's declarative
  `Blank`. Deliberate, and it makes the listing consistent with the editor, but it is a
  user-visible diff and should not be read as a regression.

**ADR-0054** — *Adopt charmbracelet table/tabs as the shared TUI component layer* — records the
decision and, importantly, the **boundary**: new menus use the primitives; `agents_model.go`,
`/queue`, and `/approvals` migrate later.

Reconcile also corrected the change's own premise: the "ragged when a persona is blank" bug the
change was written to fix had already been closed by changes 0078/0079. The work stands on
consolidation and the grouped-settings surface instead. Recorded in the change's Reconcile log.

## Follow-ups

Not captured as stubs — `auto_capture.enabled` is `false` in this repo, so these are recorded here
for a human to file if wanted:

- **Migrate `agents_model.go`'s 15 render sites onto the table primitive.** This change
  consolidated 18 of 33 sites; `agents_model.go` holds the largest remaining share and was
  explicitly out of scope. It is the named next step in ADR-0054's boundary.
- **Edit actions for the `/config` Permissions and MCP tabs.** Both are read-only in this change.
  There is no `/mcp` builtin at all today, so the MCP tab is the first surface for it.
- **`/config` panes do not refresh while open** (review finding #11). Unlike the agents overlay's
  tick, `/config` repaints only on a keystroke, so an MCP server that connects or drops while the
  screen is open shows a stale tool count. Harmless for read-only panes; it needs solving before
  the MCP tab gains actions.
- **Ctrl+C is still swallowed by `/queue` and `/models edit`** (review finding #10). Fixed for
  `/config` only; the other two overlays were left alone as pre-existing and out of scope.

## Plan deviations

- The **plan role degraded to `auto`** — `superpowers:writing-plans` was not invocable in the run
  session, so the plan was authored inline per the convention's missing-skill rule.
- Task 1 additionally folded `agents_model.go`'s duplicate `padCells`/`truncateCells` onto the
  shared implementation. That was authorized by the task as the resolution of a duplicate-symbol
  conflict; `agents_model.go`'s *rendering* remains out of scope and unmigrated.
