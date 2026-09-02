<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0080 — Shared TUI table component + tabbed /config UI — line up the menus like Claude](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0080-tui-table-tabbed-ui.md)**
<!-- docket:backlink:end -->

# Plan: shared TUI table component + tabbed `/config` UI (change 0080)

Spec: `docs/superpowers/specs/2026-09-01-tui-table-tabbed-ui-design.md` (on `docket`).
Base: `origin/main` @ `18243d0`. Branch: `feat/tui-table-tabbed-ui`.

> **Plan role degraded to `auto`.** `superpowers:writing-plans` (the configured
> `skills.plan`) is not invocable in this session's skill registry, so this plan
> was authored inline per the convention's missing-skill rule. Stop-point and
> artifact are unchanged.

## What this change is actually for

Reconcile established that the original motivation — "`/models` goes ragged when a
persona is blank" — is **already fixed** by 0078/0079. The real cost is
**duplication**: 33 non-test `padCells` / `truncateCells` / `lipgloss.Width` sites
across four private renderers. This change consolidates **18 of 33** (leaving
`agents_model.go`'s 15 out of scope) behind one primitive, and adds the tabbed
`/config` surface that grouped settings currently have no home for.

**Non-negotiable:** current rendering behavior must not regress. Every existing
alignment guarantee — the completer's registry-wide command column, its
`width` clamp, the `/models` tag alignment across blank personas — is a
**requirement of the primitive**, not something the primitive gets to redesign.

## Learnings that bind this plan

These are prior findings from this repo's ledger. They are requirements, not advice.

| Finding | Binding requirement |
|---|---|
| `global-max-column-width-must-clamp-to-render-width` (from **0078 — this exact code**) | The primitive's global-max column padding **must clamp against render width**. Tests must include an over-wide entry **scrolled off the visible page** and assert `lipgloss.Width(row) <= width`. |
| `fitline-width-invariant-hides-truncated-suffix` | A width-invariant assertion is the **wrong witness** for truncation. Assert the **trailing tag survives verbatim**. Budget truncation in **display cells reserving the ellipsis**, never bytes. Include a **pure-ASCII** long case. |
| `border-inside-fixed-width-manual-join` | Any border/separator glyph consumes cells from the **same** width budget. Never wrap a fitted row in `lipgloss.Border` or `JoinHorizontal`. |
| `teatest-final-frame-via-finalmodel-view` | Frame captures render `FinalModel().View()`, not `FinalOutput`, and force `termenv.TrueColor` around the call. |
| `completer-entry-bypass-dispatch` | `/config`'s Models tab dispatches on the **entry/row object**, never by re-parsing an expansion string. |
| `sanitize-untrusted-bytes-fixed-width-tui` | Cell values that can carry model/tool bytes route through the existing sanitizer before measurement. |

## Task 1 — `internal/tui/table.go`: the table primitive

**Files:** new `internal/tui/table.go`, new `internal/tui/table_test.go`.

Fuse-facing API over `lipgloss/table` for static rendering:

```go
type ColumnAlign int
const (AlignLeft ColumnAlign = iota; AlignRight)

type Column struct {
    Header   string
    Align    ColumnAlign
    MinWidth int          // 0 = natural
    MaxWidth int          // 0 = unbounded before the render-width clamp
    Blank    string       // substituted for an empty cell so the column holds
}

type Row struct {
    Cells  []string
    Active bool           // renders the "▸ " marker; others get the 2-cell indent
    Tag    string         // trailing parenthesised annotation, e.g. "(default, active)"
    Style  lipgloss.Style // per-row style, zero value = none
}

// Render returns display lines: optional header + one line per row, every line
// guaranteed <= width display cells.
func RenderTable(cols []Column, rows []Row, width int, opts TableOpts) []string
```

Behavior the implementation owns (all folded out of the four current renderers):

1. **Two-pass measurement in display cells** via `lipgloss.Width` on **unstyled**
   text — never `len`, never a styled string.
2. **Global-max widths clamped to `width`.** Compute each column's natural max
   across **all** rows (stable gutters while scrolling), then clamp the total
   against `width`, shrinking the widest flexible column first and reserving a
   minimum budget for the last column. This is the 0078 defect; it is designed in
   here rather than rediscovered.
3. **Truncation in display cells reserving the ellipsis** — reuse the existing
   `truncateCells`; a truncated cell is cut as one composed **plain** string
   (ANSI escapes are not cells), accepting the documented styling loss.
4. **Blank cells hold their column** — `Column.Blank` (default `""`, padded to
   width) so a missing value never collapses the gutter.
5. **Trailing tag** appended after the last padded column, so tag offsets align
   across rows regardless of blank cells. Tail whitespace trimmed **only** when
   no tag follows (today's `renderModelsListing:82-85` behavior, preserved).
6. **Active marker** `"▸ "` vs a 2-cell indent — the marker occupies budget, it
   does not add width.

`padCells` and `truncateCells` move into `table.go` as the single implementation;
their current call sites in the three adopted files are removed by tasks 2–4.
`agents_model.go` keeps its own copies for now (out of scope) — if that forces a
duplicate symbol, `agents_model.go` gets privately-renamed local copies with a
comment naming this change's follow-up.

**Tests (`table_test.go`) — written first:**
- `TestTableRowsFitWidth`: matrix of widths {40, 60, 80, 100, 120} × a fixture
  containing an entry **wider than the budget while scrolled off-page**; asserts
  `lipgloss.Width(line) <= width` for **every** line. *(0078 finding.)*
- `TestTableTagSurvivesTruncation`: over-wide first column; asserts the trailing
  tag appears **verbatim** in the output. Includes an explicit **pure-ASCII**
  long case alongside a CJK/emoji case. *(0066 finding — the ASCII case is the
  one that catches a byte-vs-cell budget bug.)*
- `TestTableBlankCellHoldsColumn`: rows with and without a middle value; asserts
  the following column's **start offset** is identical on both.
- `TestTableColumnOffsetsStable`: mirrors the existing
  `models_command_test.go` column-offset assertions.
- `TestTableActiveMarkerWidthNeutral`: active and inactive rows are the same width.

## Task 2 — adopt in the `/models` listing

**Files:** `internal/tui/models_command.go`, `internal/tui/models_command_test.go`.

Rewrite `renderModelsListing` to build `[]Column{alias, id, persona}` +
`[]Row` and call `RenderTable`. Keep `modelsTag`, `personaCell`, `humanTokens`,
`limitsCell` — only the measurement/padding block (`:37-88`) is replaced.
Wire `personaCell`'s dash through `Column.Blank` so the substitution becomes
declarative rather than a per-call-site helper.

**Gate:** `models_command_test.go` must pass **unmodified** except where a test
asserts on an implementation detail that no longer exists. Any behavioral
assertion that changes is a red flag to stop and re-check, not to edit the test.

## Task 3 — adopt in the slash-command completer

**Files:** `internal/tui/slash_completer.go`, `internal/tui/slash_completer_test.go`.

Replace `slashCompleter.View`'s body (`:160-259`) with a `RenderTable` call over
columns {command, kind-tag, description}. Preserved exactly:
- the **registry-wide** `MaxCommandWidth()` global max (not the visible page),
- the `width - (2+2+kindTagWidth+2+completerMinDescCells)` clamp — expressed as
  the description column's `MinWidth`,
- `commandText`/`commandWidth` as the single source of the command portion,
- scroll indicators (`↑ N more` / `↓ N more`), rendered outside the table.

The existing off-screen-over-wide-entry regression test must survive and pass.

## Task 4 — adopt in the `/models edit` list view

**Files:** `internal/tui/models_editor.go`, `internal/tui/models_editor_test.go`.

`renderModelsEditorOverlay` (`:343-394`) — the alias column measured at `:361`
and padded at `:368` — becomes a `RenderTable` call. The form (`renderModelForm`)
is untouched.

## Task 5 — `internal/tui/tabs.go`: the tabbed-container primitive

**Files:** new `internal/tui/tabs.go`, new `internal/tui/tabs_test.go`.

```go
type Pane struct {
    Title string
    View  func(width, height int) string
    Key   func(tea.KeyMsg) (handled bool, cmd tea.Cmd) // nil = inert pane
}

type Tabs struct { panes []Pane; active int }

func (t *Tabs) View(width, height int) string      // tab bar + active pane
func (t *Tabs) Update(tea.KeyMsg) (bool, tea.Cmd)  // switch, else delegate
func (t *Tabs) Next(); func (t *Tabs) Prev(); func (t *Tabs) SetActive(int)
```

**Key bindings — the spec's one remaining open question, settled here.**
Read against `shell_model.go`'s guard order: `Update:379` routes to
`handleOverlayMsg` first, whose `tea.KeyMsg` arm (`:535-548`) gives approvals
priority, then asks, then the active overlay. `/config` is a **full overlay** —
no text input is focused and the slash completer is inactive while it is open —
so `tab`/`shift+tab` carry no competing meaning inside it, and `bubbles/table`
binds up/down rather than tab.

- `tab` / `shift+tab` — next / previous tab (wrapping).
- `1` / `2` / `3` — jump directly to a tab.
- `esc` — close the overlay (matching `/queue` and the models editor).
- Everything else delegates to the active pane's `Key`.
- Approvals and asks keep priority — `/config` sits **below** them in the same
  guard order, changing nothing about it.

**Tests:** tab cycling wraps in both directions; direct-index keys; unhandled
keys delegate to the active pane and not to its neighbours; the tab bar plus
pane render within `width` (border glyphs inside the budget — 0041 finding).

## Task 6 — the `/config` screen

**Files:** new `internal/tui/config_view.go`, new `internal/tui/config_view_test.go`,
edits to `internal/tui/shell_model.go` and `internal/tui/slash_registry.go`.

- Register `/config` in `handleSlash`'s switch (`shell_model.go:942-1017`) and in
  the slash registry so it completes, alongside the existing builtins.
- Three panes:
  - **Models** — a `bubbles/table` over the registry (selection + scroll), with
    the editor's add/edit/remove actions available in-tab. `/models edit` keeps
    working as a direct entry point; both routes call the same handlers.
    Row actions dispatch on the **row object**, never a re-parsed string.
  - **Permissions** — read-only view of the session permission mode (what
    `/mode` reports).
  - **MCP** — read-only list of configured servers. There is no `/mcp` builtin
    to mirror; this is the first surface for it.
- Overlay state on `ShellModel` follows the existing `modelsEditState` pattern;
  the key guard is added next to `handleModelsEditorKey`.

**Tests:** `/config` opens and closes; each tab renders; tab switching works
through the real `ShellModel` key path; the Models tab's add/edit/remove reach
the same handlers `/models edit` does.

## Task 7 — teatest frame captures

**Files:** `internal/tui/models_harness_test.go` or a new `config_harness_test.go`.

Frame captures for the `/models` listing, the slash menu, `/config`, and a tab
switch, following `harness_test.go`'s `captureFrame`: render
`FinalModel().View()` (**not** `FinalOutput`), force `termenv.TrueColor` around
the call and restore it, write under `FUSE_SCREENSHOT_DIR` else `t.TempDir()`,
and skip `freeze` silently when absent so the suite stays hermetic.

## Task 8 — ADR

Dispatch `docket-adr`: *"Adopt charmbracelet table/tabs as the shared TUI
component layer."* Context = four divergent renderers, 33 sites, and the 0078
clamp bug that duplication made possible. Decision = the primitive plus the
boundary (new menus use it; `agents_model.go`, `/queue`, `/approvals` migrate
later). Consequences = one consistent look, a deprecation path for the
hand-rolled renderers, and a dependency-surface commitment to charm's table.

## Verification

- `make test` — the full suite, green, at branch HEAD (the build gate).
- Regression watch: `/models`, `/models edit`, `/model` autocomplete, and the
  slash menu all behave as before. The three width/alignment regression tests
  inherited from 0066 and 0078 are the load-bearing ones.
- `go vet ./...`.

## Out of scope (restated, so the build does not drift)

`agents_model.go`'s 15 render sites; `/queue` and `/approvals` rendering; any
model-resolution or provider-API change; edit actions on the Permissions and MCP
tabs; replacing the overlay/key-routing model.
