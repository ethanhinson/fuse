<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0078 — /models slash command + slash-menu column alignment](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0078-slash-command-models-listing.md)**
<!-- docket:backlink:end -->

# Results — `/models` listing + slash-menu column alignment (change 0078)

**Branch:** `feat/slash-command-models-listing` · **Base:** `origin/main` (`d7bddd1`)
**Plan:** `docs/superpowers/plans/2026-08-21-slash-command-models-listing.md`
**Suite:** `make test` — green at `d38b032`.

## What shipped

- **`/models` builtin** — read-only listing of every registry alias with its gateway ID and
  persona, marking the config default and the active model. New pure renderer
  `renderModelsListing` in `internal/tui/models_command.go`, dispatched from `handleSlash` and
  registered in the completer's builtin list.
- **Slash-menu column alignment** — the command column is padded to the widest command across the
  whole registry (new `SlashRegistry.MaxCommandWidth()`), so the kind-tag and description columns
  sit in stable gutters that do not shift while scrolling.
- **`model.Registry.DefaultAlias()`** — the change's only `internal/model` touch.

## Review findings and their disposition

The whole-branch review (rung: `docket-review-standard`) returned **5 findings — 0 blocker,
1 important, 4 minor**. All five entered the fix loop (`REVIEW_MIN_FIX_SEVERITY=minor`) and all
five are **fixed in-branch**; nothing was deferred, reverted, or recorded unfixed.

| # | Severity | Finding | State | Commit |
|---|---|---|---|---|
| 1 | important | Registry-wide `maxCmd` was never clamped against `width`, so an over-wide row wrapped at the pad run and destroyed the alignment the change exists to create | fixed | `d779252` |
| 2 | minor | The one test exercising the over-wide case never asserted the row fits the width it passed — the regression in #1 was invisible to a green suite | fixed | `d38b032` |
| 3 | minor | `commandWidth`'s "single source of truth" comment was false — `View` recomputed the command portion inline | fixed | `d779252` |
| 4 | minor | `MaxCommandWidth` copied the whole entry list on every render (every keystroke while the overlay is open) | fixed | `c67d273` |
| 5 | minor | `TestModelsListingUnresolvableAliasSkipped` could not reach the branch it was named for, and pinned an incidental map-aliasing property of `NewRegistry` instead | fixed | `d38b032` |

**Finding 1 is the one worth a human's attention.** It was a genuine regression, not a
theoretical one: `MaxCommandWidth()` spans `All()`, which includes MCP entries shaped
`/mcp:<server>/<tool>` — routinely 40+ cells. Because the overlay is composited through
`wordwrap`, an over-wide row broke at a space *inside* the new padding run, pushing the kind tag
and description onto a second line on **every** row and turning the 8-row overlay into 16+ lines.
One long MCP tool name did that at an ordinary 80 columns even when no MCP entry was in the
visible window. The fix clamps the pad target to
`width - (2 + 2 + kindTagWidth + 2 + completerMinDescCells)` and truncates the command portion to
the clamp.

## Plan deviations

1. **`truncateCells` already existed.** The plan assumed Task 3 would create it; it was already at
   package scope in `internal/tui/agents_model.go` with exactly the specified cell semantics.
   Defining a second one would not compile, so the existing helper was reused and only extended
   with an `n <= 0 → ""` guard (it previously returned `"…"`). Its sole prior caller,
   `turnPromptPreview`, clamps with `maxInt(1, budget-2)`, so that path is provably unchanged.
2. **Task order.** Plan Task 2 consumes a helper plan Task 3 defines, so they were executed
   3-before-2. Task boundaries and one-commit-per-task accounting are unchanged.
3. **Findings 1 and 3 were fixed in one commit** rather than two: they rewrite the same
   expression, so separate commits would have had the second contradict the first.
4. **The `/models` renderer does not use `truncateCells`.** It was listed as a prerequisite, but
   the renderer has no width budget to truncate against — its columns size to their own content.

## Residual risks and follow-ups

- **Very narrow terminals.** When `capCmd <= 0` the command column floors at 0 and the fixed
  `kindTagWidth` run alone can still exceed `width`. This is the responsive-collapse behavior the
  spec put explicitly out of scope; the new tests cover widths 40 and 80. If narrow-terminal
  rendering ever matters, that is a separate change.
- **Truncated commands lose their syntax highlight.** When a command portion exceeds the clamped
  budget it is truncated as one composed plain string, so that row renders without the amber
  `Syntax` styling. Rows within budget keep the split styling. Truncating a styled composite was
  not an option — ANSI escapes are not display cells.
- **One defensive branch is unreachable and untested.** `renderModelsListing` skips an alias that
  `Names()` returns but `Resolve()` rejects. Because both read the same `entries` map, that state
  is unconstructable through the exported API. The branch was kept as documented defensive code
  and its comment now says plainly that it is untested. It would become reachable — and would
  need real coverage — if `Registry` ever grew a second entry source.
- **Finding 4's guard is timing-tolerant.** The post-reload staleness assertion polls to absorb
  the registry's 50 ms fan-out debounce (2 s deadline), rather than being deterministic.

## Manual check at the merge gate (optional)

Automated tests cover the rendering contract, but the payoff here is visual. If you want to see
it: run the TUI, type `/` to open the slash menu and confirm the kind-tag and description columns
line up in fixed gutters as you scroll, then run `/models` and confirm the active model carries
`▸` and the correct `(default, active)` / `(default)` / `(active)` tag. Widening and narrowing the
terminal is the interesting case — that is where finding 1 lived.
