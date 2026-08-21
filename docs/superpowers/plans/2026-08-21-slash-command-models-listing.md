<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0078 — /models slash command + slash-menu column alignment](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0078-slash-command-models-listing.md)**
<!-- docket:backlink:end -->

# Plan — `/models` listing + slash-menu column alignment (change 0078)

**Branch:** `feat/slash-command-models-listing`
**Worktree:** `/Users/ethanhinson/dev/fuse/.worktrees/slash-command-models-listing`
**Spec:** `docs/superpowers/specs/2026-08-20-slash-command-models-listing-design.md` on `origin/docket`
(read it including its three **Reconciled 2026-08-21** annotations — those are binding decisions).
**Test command:** `make test` (runs `observability-validate`, then `go test ./...`).

> **Plan-role degradation:** the configured plan skill (`superpowers:writing-plans`) was not
> invocable on this machine, so this plan was authored directly by `docket-implement-next` under
> the convention's missing-skill rule (degrade to `auto` + warn). The artifact contract is
> unchanged; only the authoring path differs.

## Scope

Two self-contained TUI features plus one accessor:

| Area | File | Change |
|---|---|---|
| `/models` builtin | `internal/tui/models_command.go` (new) | pure renderer for the listing |
| `/models` dispatch | `internal/tui/shell_model.go` | new `case "/models":` in `handleSlash` |
| `/models` completion | `internal/tui/builtin_provider.go` | new `SlashEntry` beside `/model` |
| column alignment | `internal/tui/slash_completer.go` | pad command column; cell-denominated truncation |
| max-width source | `internal/tui/slash_registry.go` | new `MaxCommandWidth() int` |
| default accessor | `internal/model/registry.go` | new `DefaultAlias() string` |

Nothing else is touched. No config, no wire, no gateway.

## Binding decisions carried from the reconcile pass

1. Max command width is a **registry method**, `SlashRegistry.MaxCommandWidth() int` — no caching.
2. **Add** `model.Registry.DefaultAlias() string`; do not alter the `Registry` struct.
3. All width math in `slashCompleter.View` moves to **display cells** via `lipgloss.Width`
   (already imported), reserving one cell for the ellipsis. The current code is byte-denominated
   (`len(cursor) + len(e.Command)`, `desc[:descWidth-1]`) against a cell budget — the
   `fitline-width-invariant-hides-truncated-suffix` defect class. `desc[:descWidth-1]` is also an
   unguarded byte slice that can panic mid-rune; the rewrite removes that.
4. Never measure a **styled** string. Measure the plain command+syntax, style it, then pad by the
   difference.

## Tasks

Each task is TDD: write the failing test first, then the minimal implementation, then verify.
Tasks are ordered so each builds on a green predecessor.

---

### Task 1 — `model.Registry.DefaultAlias()`

**Test** (`internal/model/registry_test.go`, extend or create):
- `DefaultAlias()` on a registry built by `NewRegistry("glm", …)` returns `"glm"`.
- `DefaultAlias()` on `DefaultRegistry()` returns `"deepseek-flash"` and equals the `Default` field.
- `DefaultAlias()` on a zero-value/empty-default registry returns `""` (no panic).

**Implement** in `internal/model/registry.go`:

```go
// DefaultAlias returns the configured default alias.
func (r *Registry) DefaultAlias() string { return r.Default }
```

**Verify:** `go test ./internal/model/...`

---

### Task 2 — `SlashRegistry.MaxCommandWidth()`

**Test** (`internal/tui/slash_registry_test.go`):
- A registry over a stub provider with entries `/a`, `/bbbb`, `/cc` + `Syntax: "NAME"` returns the
  width of the widest **command portion**, where the command portion is `Command`, plus
  `" " + Syntax` when `Syntax != ""`. For `/cc NAME` that is 8 cells, which must beat `/bbbb` (5).
- An **empty** registry returns `0` (guards a `-1` pad later).
- The width is measured in **display cells**: an entry whose `Syntax` contains a multibyte
  character is measured by `lipgloss.Width`, not `len`. (Assert a CJK/emoji case is not
  over-counted by its byte length.)
- The result reflects `All()`, i.e. **every** entry — not a filtered subset. Assert by filtering
  the completer to a short command and still getting the wide registry-scoped width in Task 4.

**Implement** in `internal/tui/slash_registry.go`:

```go
// MaxCommandWidth returns the display-cell width of the widest command portion
// (Command plus " "+Syntax when present) across every entry. 0 when empty.
func (r *SlashRegistry) MaxCommandWidth() int {
    max := 0
    for _, e := range r.All() {
        if w := commandWidth(e); w > max {
            max = w
        }
    }
    return max
}
```

with the shared helper in `slash_completer.go` (Task 3) so both sides measure identically.

**Verify:** `go test ./internal/tui/ -run SlashRegistry`

---

### Task 3 — width helpers in `slash_completer.go`

**Test** (`internal/tui/slash_completer_test.go`):
- `commandWidth(SlashEntry{Command: "/model", Syntax: "NAME"})` == 11; with empty `Syntax` == 6.
- `truncateCells(s, n)`:
  - a string already within `n` cells is returned **unchanged** (no ellipsis appended);
  - an over-long ASCII string comes back at **exactly** `n` cells and ends in `…`;
  - an over-long **multibyte** string comes back at exactly `n` cells and never splits a rune
    (round-trips through `[]rune` cleanly / `utf8.ValidString` holds);
  - `n <= 0` returns `""`; `n == 1` returns `"…"`.

**Implement** (unexported, same file):

```go
// commandWidth is the display-cell width of an entry's command portion,
// measured UNSTYLED. Single source of truth for both the registry max and
// the per-row pad.
func commandWidth(e SlashEntry) int {
    s := e.Command
    if e.Syntax != "" {
        s += " " + e.Syntax
    }
    return lipgloss.Width(s)
}

// truncateCells clips s to at most n display cells, reserving one cell for
// the ellipsis when it actually truncates. Never splits a rune.
func truncateCells(s string, n int) string { … }
```

`truncateCells` accumulates `lipgloss.Width(string(r))` per rune until adding the next would
exceed `n-1`, then appends `…`. Returns `s` untouched when `lipgloss.Width(s) <= n`.

**Verify:** `go test ./internal/tui/ -run 'CommandWidth|TruncateCells'`

---

### Task 4 — align the command column in `View`

This rewrites the row-composition block of `slashCompleter.View` (currently lines ~121–159).

**Test** (`internal/tui/slash_completer_test.go`) — all assertions run on `stripANSIString(...)`
of the rendered rows, since the helper already exists in this package's tests:

1. **Kind-tag column is stable.** With entries of differing command widths (`/a`, `/exit`,
   `/blackboard`, `/model NAME`), every rendered row has its kind tag starting at the *same*
   rune/cell offset. Assert offsets are all equal — do **not** assert on total row width, which
   the `fitline-width-invariant-hides-truncated-suffix` finding shows is blind to this class.
2. **Pad is registry-scoped, not page-scoped.** Build a registry with >`completerMaxRows` entries
   where the single widest command sorts **below** the visible window. Assert the visible rows are
   still padded to that off-screen width — i.e. the gutter does not shift when scrolling.
   This is the assertion that pins `MaxCommandWidth()` reading `All()` rather than `c.visible`.
3. **Styled and unstyled rows align identically.** The selected row (rendered through
   `completerSelectedStyle`) and a normal row put their kind tag at the same stripped offset.
   This is the "never `len()` a styled string" guard.
4. **Long command does not eat the description.** With a very wide command and a narrow `width`,
   the description is truncated (or empty) but the kind tag still renders in full — assert the tag
   text **survives verbatim**, per the suffix-survival rule in the learnings finding.
5. **Multibyte description does not panic** and lands at the right cell budget (regression against
   the old `desc[:descWidth-1]` byte slice).

**Implement:**

- At the top of `View`, once per call: `maxCmd := 0; if c.reg != nil { maxCmd = c.reg.MaxCommandWidth() }`.
- Per row: build `plain := e.Command` (+ `" " + e.Syntax` when set) for measurement; build the
  styled label exactly as today; then `pad := maxCmd - lipgloss.Width(plain)`, clamped at `>= 0`,
  appended as `strings.Repeat(" ", pad)` **after** the styled label.
- Replace the `used` computation with cell-denominated arithmetic:
  `used := lipgloss.Width(cursor) + maxCmd + 2 + kindTagWidth + 2`.
- Replace the truncation with `desc = truncateCells(e.Description, descWidth)` guarded by
  `descWidth > 0` (and `desc = ""` when `descWidth <= 0`).
- Apply the identical pad on **both** branches (normal and selected) so the two stay aligned —
  factor the label+pad construction so it is written once, not duplicated across the `if`.

**Verify:** `go test ./internal/tui/ -run SlashCompleter`

---

### Task 5 — `/models` renderer

**Test** (`internal/tui/models_command_test.go`, new) — table-driven over a small fixed registry
built with `model.NewRegistry`, e.g. default `"glm"` with aliases `glm`, `sonnet-5`, `claude-max`:

- Header line is `Available models:`.
- One row per alias, in `Names()` (sorted) order — assert the exact order.
- The **active** alias row carries the `▸ ` prefix; every other row is indented two spaces.
- Tag vocabulary is exactly `(default, active)`, `(default)`, `(active)`, or absent — assert all
  four cases across fixtures (active==default, default-not-active, active-not-default, plain).
- Alias / ID / persona columns are padded so each column starts at the same offset on every row.
- An alias whose `Resolve` fails is impossible via `Names()`, but the renderer must not panic if
  it does — assert a registry with an entry removed underneath still renders without panicking
  (defensive skip).
- Empty registry renders just the header (or a "no models" line) and does not panic.

**Implement** `internal/tui/models_command.go`:

```go
// renderModelsListing returns the /models output as display lines: a header
// followed by one aligned row per registry alias. Pure — no ShellModel, no I/O.
func renderModelsListing(reg *model.Registry, active string) []string
```

Two passes: measure the widest alias / ID / persona in display cells, then emit
`prefix + padded(alias) + "  " + padded(id) + "  " + padded(persona) + tag`. Use `lipgloss.Width`
for the padding (same discipline as Task 3/4) and `headerStyle` for the header. `reg == nil`
returns a single "no models" line.

**Verify:** `go test ./internal/tui/ -run Models`

---

### Task 6 — wire `/models` into dispatch and completion

**Test:**
- `internal/tui/shell_model_test.go` (or a new `shell_models_command_test.go`) — drive
  `handleSlash("/models")` through the existing harness and assert the output block contains the
  header and the active model's row with its marker. Also assert `/models` does **not** disturb
  `m.alias` (it is read-only) and that `/model NAME` still switches as before.
- `internal/tui/slash_completer_test.go` — typing `/model` lists **both** `/model` and `/models`
  (prefix coexistence, per the spec: correct and desired, not a collision). Assert both commands
  are present in `visible`.

**Implement:**
- `builtin_provider.go` — add immediately after the `/model` entry:
  ```go
  {Command: "/models", Description: "List available models and the active/default", Kind: KindBuiltin, expand: func() string { return "/models" }},
  ```
  No `Syntax` (takes no argument).
- `shell_model.go` — add `case "/models":` adjacent to `case "/model":`:
  ```go
  case "/models":
      for _, l := range renderModelsListing(m.reg, m.alias) {
          m.appendLine(l)
      }
      return m, nil
  ```
  Note `handleSlash` has a value receiver and `appendLine` a pointer receiver — follow the
  existing `/approvals` case, which already does exactly this, rather than inventing a new shape.

**Ordering caution:** Go's `switch` on a string matches whole values, so `"/models"` and
`"/model"` are distinct cases and order between them is irrelevant — but keep them adjacent for
readability, as the spec asks.

**Verify:** `go test ./internal/tui/...`

---

### Task 7 — full-suite gate

Run `make test` from the worktree root. All packages green. No skipped or newly-flaky tests.
If `observability-validate` fails for reasons unrelated to this diff, report it rather than
working around it — this change touches neither observability config nor its schema.

## Out of scope (do not drift)

- Which models exist, their IDs, personas, or the default — display only.
- Any change to `/model NAME` switching behavior.
- Configurable/persisted column widths; responsive collapse or horizontal scroll on narrow
  terminals (existing truncation behavior is retained, only its denomination changes).
- Reflowing the kind-tag or description columns beyond what the command-column fix requires.

## Risks

- **Low overall** — read-only display code in one package plus a one-line accessor.
- The one real trap is measuring styled-string width for padding; Task 4 test 3 pins it.
- Secondary trap: applying the pad on only one of `View`'s two render branches. Task 4 test 3
  catches that too, which is why the label+pad construction is factored rather than duplicated.
