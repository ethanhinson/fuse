<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0078 — /models slash command + slash-menu column alignment](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0078-slash-command-models-listing.md)**
<!-- docket:backlink:end -->

# `/models` listing + slash-menu column alignment — design

## Summary

Two small, self-contained TUI improvements that ship together because they touch the
same surface (the shell's slash-command system) and neither warrants its own change:

1. **`/models` slash command** — a read-only command that lists every model alias the
   session's registry knows, with its resolved gateway ID, persona, and markers for the
   config default and the currently-active model. Today the only way to discover an alias
   is to already know it (`/model NAME` errors on an unknown name but never enumerates the
   set).
2. **Slash-menu column alignment** — the autocomplete overlay renders each row as
   `command + syntax`, then a kind tag, then a description, but the command column is
   *variable width*, so the kind-tag and description columns are ragged across rows. Pad
   the command column to the widest command in the whole registry so the later columns
   line up into stable gutters.

Both live entirely in `internal/tui`. No config, no wire, no gateway changes.

## Motivation

- **Discoverability.** `fuse` is a multi-model harness — the whole point is switching models
  (`deepseek-flash`, `glm`, `sonnet-5`, `claude-max`, …). Yet nothing surfaces the list.
  A user has to read source or guess. `/mode` already lists its options on a bare invocation;
  `/models` gives `/model` the same courtesy, one level richer (it lists rows, not a
  one-line enum, because each model carries an ID and persona worth seeing).
- **Alignment.** The ragged columns read as unfinished. The request is explicit: the
  spacing on the slash menu should align with the longest item in the list.

## Part 1 — `/models` command

### Data source

Everything needed already exists on `internal/model.Registry`, which the shell holds as
`m.reg`:

- `(*Registry).Names() []string` — all aliases, already sorted.
- `(*Registry).Resolve(alias) (ModelConfig, error)` — per-alias `ID`, `Persona`, etc.
- `Registry.Default string` — the config default alias.
- The shell's current selection is `m.alias` (the field `/model NAME` writes).

No new registry method is required. If a small accessor reads cleaner than reaching into
fields at the call site, add `Registry.DefaultAlias()` — but do not otherwise change the
registry.

### Behavior

`/models` takes no arguments. It prints a header line, then one aligned row per alias in
`Names()` order:

```
Available models:
  ▸ deepseek-flash  cloud/deepseek-v4-flash  coding   (default, active)
    glm             cloud/glm-5.2            general
    sonnet-5        claude/sonnet-5          general
    claude-max      cli/claude-max           coding
    ...
```

- **Columns:** alias, gateway `ID`, `Persona`. Each column is padded to the widest value in
  that column across the listed models so they line up (same alignment discipline as Part 2,
  applied to the output rows rather than the menu).
- **Active marker:** the `▸ ` cursor-style prefix (matching the completer's own selected
  glyph) marks the row whose alias equals `m.alias`; all other rows get a two-space indent.
- **Default/active tag:** a trailing parenthesised tag. `(default, active)` when the alias
  is both the registry default and the current selection; `(default)` / `(active)` when only
  one applies; nothing otherwise. Keep the tag vocabulary to exactly these words.
- Rendering uses the shell's existing line-append path (`m.appendLine`) and existing styles
  where they fit (e.g. `headerStyle` for the "Available models:" header). No new colour
  constants unless an existing one is clearly wrong.

### Registration

`/models` is a builtin, so it is added in **two** places (the codebase keeps the completer
list and the dispatch switch separate):

1. `internal/tui/builtin_provider.go` — a new `SlashEntry` in `NewBuiltinProvider`:
   `{Command: "/models", Description: "List available models and the active/default", Kind: KindBuiltin, expand: func() string { return "/models" }}`.
   No `Syntax` (it takes no argument). Place it adjacent to the `/model` entry so the two
   read together.
2. `internal/tui/shell_model.go` — a new `case "/models":` in `handleSlash`, before or after
   the existing `case "/model":`.

Because `/models` shares the `/model` prefix, confirm the completer's prefix filter still
lists both when the user types `/model` (it filters by prefix, so both match — this is
correct and desired, not a collision).

## Part 2 — slash-menu column alignment

### The defect

`slashCompleter.View` (`internal/tui/slash_completer.go`) builds each row as:

```
cursor + label + "  " + paddedTag + "  " + desc
```

`label` is `e.Command` optionally followed by a space and the styled `Syntax`. Its width
varies per entry (`/exit` vs `/blackboard` vs `/mcp:everything/echo`), so the `"  "` gap
places `paddedTag` at a different column on every row — ragged. `paddedTag` is already
padded to `kindTagWidth`, so the kind and description columns *would* align if the command
column were fixed-width; it is the command column that is not.

### The fix

Pad the command column to the **widest command across the whole registry**, not just the
currently-visible page, so the column position is stable as the user scrolls (the chosen
scope: a fixed gutter that does not shift when a long off-screen command scrolls into or
out of view).

- Compute the pad width once per `View` call (or cache it on refresh): the max, over
  **all** entries the registry can produce, of the *display width* of the command portion
  (`e.Command` plus, when `Syntax != ""`, `" " + e.Syntax`). Use a rune/display-width count,
  not `len()` on bytes — command names are ASCII today but the width helper should not
  assume it (mirror whatever width primitive the TUI already uses; if none, `utf8.RuneCountInString`
  is acceptable for the current ASCII-only set and noted as such).
- Pad the composed command-portion string to that width with trailing spaces, then append
  the existing `"  " + paddedTag + "  " + desc`. The **styled** substrings (amber syntax,
  selected-row background) must be preserved — pad based on the *unstyled* width but render
  the styled text, so styling never corrupts the measured width (a known lipgloss footgun:
  never `len()` a styled string). Measure the plain command+syntax, then style, then pad by
  the difference.
- The `descWidth` truncation math at the current line 139 must be updated to use the new
  command-column width instead of `len(e.Command)`, so long commands don't over-run the
  description budget.

### Registry access for the max width

`View(width int)` currently reads only `c.visible`. "Widest across all entries" needs the
full set — `c.reg` (the `*SlashRegistry`) is already held on the completer. Add a small
method on the registry (e.g. `MaxCommandWidth() int`) that iterates every entry once and
returns the max command-portion display width, or compute it in the completer from a
registry snapshot. Prefer a registry method: it keeps the width definition next to the
data and is trivially testable. Cache it if the registry exposes a change signal; otherwise
recomputing per `View` is cheap for the command counts involved.

## Out of scope

- Changing what models exist, their IDs, personas, or the default — this is display only.
- Any change to `/model NAME` switching behavior.
- Reflowing the description or kind-tag columns beyond what the command-column fix requires.
- Persisting or configuring column widths — they are derived, not settings.
- Horizontal-scroll or responsive collapse of the menu on very narrow terminals (the
  existing truncation behavior is retained as-is).

## Testing

- **`/models` output** — a table-driven test over a small fixed `Registry` asserting: every
  alias appears in `Names()` order; the active row carries `▸` and `(active)`/`(default, active)`;
  the default-but-inactive row carries `(default)`; columns are aligned (assert on the exact
  rendered block or on column start offsets). Reuse the existing shell-model test harness
  (`harness_test.go` / `shell_mode_command_test.go`).
- **Alignment** — a `slashCompleter.View` test with entries of differing command widths
  asserting the kind-tag column starts at the same offset on every row, and that the pad is
  driven by the widest *registry* entry even when it is scrolled off the visible page.
- **Prefix coexistence** — a `slash_completer_test.go` case asserting typing `/model` lists
  both `/model` and `/models`.
- Styled-width regression — assert the selected (styled) row and a normal row align
  identically, guarding the "don't `len()` a styled string" footgun.

## Risks

- Low. Read-only display code in one package. The one real trap is measuring styled string
  width for padding; the test above pins it.
