<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0002 — Bubbletea TUI MVP + Makefile](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0002-tui-mvp-makefile.md)**
<!-- docket:backlink:end -->

# Bubbletea TUI MVP + Makefile — results
Change: #2 · Branch: feat/tui-mvp-makefile · PR: (opened at close-out) · Plan: docs/superpowers/plans/2026-08-04-tui-mvp-makefile.md · ADRs: none

## Verify (human)

Automated tests fully cover the model logic via `Update`/`View` with synthetic messages; the one thing tests cannot exercise is the live terminal. At the merge gate:

- [ ] `make build && ./fuse shell` in a real TTY — confirm the alt-screen opens, the transcript viewport scrolls (↑/↓, PgUp/PgDn), the input bar accepts text, and Enter runs a turn with a `running…` status indicator.
- [ ] In the live shell, confirm `/model NAME`, `/verbose`, `/exit`, `Ctrl+L` (clear), and `Ctrl+C`/`Ctrl+D` (quit) behave as before.
- [ ] Confirm `fuse shell` in a non-TTY context (e.g. piped stdout) fails cleanly with a `tui error:` message and exit 1 (bubbletea's own non-TTY detection; documented, not special-cased).

## Findings

- Whole-branch self-review found no correctness bugs. The bubbletea value-receiver Model with pointer-receiver `appendLine`/`refreshViewport` helpers is correct: the helpers mutate the local copy that `Update` then returns. The agent-goroutine-as-`tea.Cmd` + re-armed `waitForMsg` drain over a buffered channel (cap 64) is the idiomatic non-blocking pattern and is deadlock-free.
- No ADR minted: the channel-based renderer bridge was prescribed by the spec, not a novel build-time decision.

## Follow-ups

- The orphaned `*tui.Renderer.ModelHeader` (flagged in the change-0001 results file) was removed in this change, along with its test — no separate follow-up needed.
- Deferred by spec (not this change): two-pane layout, sidebar/artifacts panel, mouse beyond scroll, subagent inline blocks, inline (non-alt-screen) mode, one-shot TUI treatment.

## Notes / plan deviations

- Skill-layer degradation: the `superpowers` plugin skills (plan/build/review/finish) are not installed in this environment, so each degraded to `auto` per the docket missing-skill rule — the plan was authored, the build executed with TDD, and the branch reviewed all by the implementer directly. Same artifacts, same stop-points.
- Dependency mechanics: `go get` of the three Charm modules followed by `go mod tidy` prunes the requires until code actually imports them; the deps were therefore pinned by `go mod tidy` after the source files that import `bubbletea`/`bubbles`/`lipgloss` were written, rather than as a bare pre-add. Final `go.mod` lists them correctly.
