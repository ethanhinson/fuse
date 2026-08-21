---
id: 78
slug: slash-command-models-listing
title: /models slash command + slash-menu column alignment
status: in-progress
priority: medium
type: feat
created: 2026-08-20
updated: 2026-08-21
depends_on: []
related: []
discovered_from: []
adrs: []
spec: docs/superpowers/specs/2026-08-20-slash-command-models-listing-design.md
plan: docs/superpowers/plans/2026-08-21-slash-command-models-listing.md
results:
trivial: false
auto_groomable:
branch: feat/slash-command-models-listing
claimed_at: 2026-08-21T05:22:04Z
pr:
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-20-slash-command-models-listing-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-20-slash-command-models-listing-design.md) |
| Plan | [2026-08-21-slash-command-models-listing.md](https://github.com/ethanhinson/fuse/blob/feat/slash-command-models-listing/docs/superpowers/plans/2026-08-21-slash-command-models-listing.md) |
<!-- docket:artifacts:end -->

## Why

`fuse` is a multi-model harness — switching models is a core interaction (`/model NAME`).
But nothing surfaces *which* models exist: `/model` errors on an unknown name and never
enumerates the set, so a user has to read source or already know the aliases. `/mode`
already lists its options on a bare invocation; models deserve the same discoverability,
one level richer since each model carries a gateway ID and persona worth seeing.

Separately, the slash-command autocomplete menu renders ragged columns: each row is
`command + syntax`, then a kind tag, then a description, but the command column is
variable-width, so the kind-tag and description columns don't line up across rows. The
request is explicit — the menu spacing should align to the longest item in the list.

## What changes

Two self-contained TUI changes, both in `internal/tui` (design detail in the linked spec):

- **A `/models` builtin** that lists every registry alias in sorted order with its resolved
  gateway ID and persona, marking the config default and the currently-active model
  (`▸` prefix + a `(default, active)` trailing tag). Read-only; sources everything from the
  existing `model.Registry` (`Names()`, `Resolve()`, `Default`) and the shell's current
  `alias`. Registered in both `builtin_provider.go` (completer entry) and `handleSlash`
  (dispatch).
- **Column alignment in the slash menu** (`slash_completer.go`): pad the command column to
  the widest command across the whole registry so the kind-tag and description columns fall
  into stable gutters that don't shift while scrolling. Padding measures the *unstyled*
  command+syntax width so lipgloss styling never corrupts the alignment.

## Out of scope

- Changing which models exist, their IDs/personas, or the default — display only.
- Any change to `/model NAME` switching behavior.
- Configurable/persisted column widths, or responsive collapse of the menu on narrow
  terminals (existing truncation is retained).

## Open questions

<!-- Both build-time details were resolved by the 2026-08-21 reconcile pass; see the log below. -->
- ~~Max-command-width as a registry method vs. computed inline in the completer~~ → **registry
  method** `SlashRegistry.MaxCommandWidth() int`, per the spec's stated preference.
- ~~`Registry.DefaultAlias()` accessor vs. reading `Registry.Default` directly~~ → **add the
  accessor**; it is this change's only `internal/model` touch and gives the `/models` call site a
  testable seam without altering the `Registry` struct.

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->

### 2026-08-21 — reconcile before build

Re-read the change and its spec against current `main` (tip `d7bddd1`), the `related` list, the
recent archive, and the ADR ledger (0032–0050). **The design holds — no scope dropped, none added.**

Every seam the spec names was verified to exist as described:

- `internal/tui/builtin_provider.go` — `NewBuiltinProvider` present, `/model` entry present, no
  `/models` entry yet. `SlashEntry` carries one field the spec did not enumerate (`Server string`,
  populated only for `KindMCP`); the new builtin leaves it zero, so this is informational only.
- `internal/tui/shell_model.go` — `handleSlash` present with `case "/model":`, no `/models` case.
  Shell fields are `alias string` and `reg *model.Registry` exactly as assumed; `appendLine(s string)`
  and `headerStyle` (in `theme.go`) are both available.
- `internal/tui/slash_completer.go` — `View(width int)` composes rows as the spec describes;
  `kindTagWidth = 18`; the completer already holds `reg *SlashRegistry`.
- `internal/tui/slash_registry.go` — `SlashRegistry` exposes `All()`, `Filter()`, `Changes()`,
  `Close()`; no `MaxCommandWidth()` yet.
- `internal/model/registry.go` — `Names()`, `Resolve()`, and the `Default` field are as specified;
  `ModelConfig.ID` / `.Persona` confirmed. No `DefaultAlias()` yet.

**One constraint folded in that the spec only half-covered.** The spec correctly warns against
measuring a *styled* string for the new command-column padding, but the pre-existing description
truncation in `View` is **byte**-denominated — `used := len(cursor) + len(e.Command) + …` and
`desc = desc[:descWidth-1] + "…"` — while the column budget it spends is denominated in display
cells. The learnings ledger carries this exact defect class
(`fitline-width-invariant-hides-truncated-suffix`, change 0066): a cell budget handed to a byte
truncator over-runs on ASCII and under-fills on multibyte, and a width-invariant assertion cannot
see it. Because this change already rewrites that arithmetic to use the new command-column width,
the truncation moves to display cells (`lipgloss.Width` — already imported in the file and the
codebase's standard width primitive) reserving one cell for the ellipsis, and the tests assert on
**column start offsets and surviving content**, not on total row width alone. A correctness
constraint on work already in scope, not an expansion of it.

No `related` change, recent archive entry, or ADR constrains this work; the only recent TUI commit
(`d7bddd1`, PR #81) touches networked-runtime error spam and overlaps neither file. Both
`## Open questions` items are resolved above.
