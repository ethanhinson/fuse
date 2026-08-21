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
plan:
results:
trivial: false
auto_groomable:
branch: feat/slash-command-models-listing
claimed_at: 2026-08-21T05:16:03Z
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [2026-08-20-slash-command-models-listing-design.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/2026-08-20-slash-command-models-listing-design.md) |
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

<!-- Resolved in the linked spec (2026-08-20). Remaining items are build-time details: -->
- Whether the max-command-width is a new `SlashRegistry.MaxCommandWidth()` method or
  computed inline in the completer — spec prefers the registry method; final call at build.
- Whether a `Registry.DefaultAlias()` accessor reads cleaner than reaching `Registry.Default`
  directly at the `/models` call site — cosmetic, decided at build.

## Reconcile log

<!-- Appended by docket-implement-next's reconcile pass: dated entries of what changed. -->
