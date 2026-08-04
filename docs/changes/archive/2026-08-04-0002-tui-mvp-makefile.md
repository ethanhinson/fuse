---
id: 2
slug: tui-mvp-makefile
title: Bubbletea TUI MVP + Makefile
status: done
priority: high
type: feat
created: 2026-08-03
updated: 2026-08-04
depends_on: []
related: [1]
discovered_from: [1]
adrs: []
spec: docs/superpowers/specs/0002-tui-mvp-makefile.md
plan: docs/superpowers/plans/2026-08-04-tui-mvp-makefile.md
results: docs/results/2026-08-04-tui-mvp-makefile-results.md
trivial: false
auto_groomable: false
branch: feat/tui-mvp-makefile
claimed_at: 
pr: https://github.com/ethanhinson/fuse/pull/2
blocked_by:
reconciled: true
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [0002-tui-mvp-makefile.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/0002-tui-mvp-makefile.md) |
| Plan | [2026-08-04-tui-mvp-makefile.md](https://github.com/ethanhinson/fuse/blob/main/docs/superpowers/plans/2026-08-04-tui-mvp-makefile.md) |
| Results | [2026-08-04-tui-mvp-makefile-results.md](https://github.com/ethanhinson/fuse/blob/main/docs/results/2026-08-04-tui-mvp-makefile-results.md) |
| PR | [#2](https://github.com/ethanhinson/fuse/pull/2) |
<!-- docket:artifacts:end -->

## Why

Phase 1 shipped a plain line-based renderer and a raw `bufio.Scanner` REPL. There is no build script and no interactive TUI. The binary can be compiled but there is no `make build` and `fuse shell` prints raw text with no visual structure.

The bubbletea TUI makes the shell feel like a real tool: scrollable transcript, status indicator while the agent runs, and a persistent input bar. The Makefile removes the friction of knowing which `go build` incantation to use.

## What changes

- `Makefile` at repo root with `build`, `install`, `test`, `lint` targets.
- Three new Charm dependencies: `bubbletea`, `bubbles`, `lipgloss`.
- `internal/tui/events.go` — five `tea.Msg` event types for agent output.
- `internal/tui/tea_renderer.go` — `agent.Renderer` implementation that pipes events onto a channel.
- `internal/tui/shell_model.go` — bubbletea `Model`: scrollable viewport + bottom input bar; handles all keyboard nav and slash commands.
- `cmd/fuse/shell.go` — replaces `replLoop`/`bufio.Scanner` with a `tea.Program`; one-shot mode (`main.go`) is untouched.

## Out of scope

- Two-pane layout with sidebar, artifacts panel, drag-to-resize.
- Mouse events beyond scroll.
- Subagent inline progress blocks.
- One-shot `fuse "<task>"` TUI treatment.

## Open questions

- Fallback behavior when stdout is not a TTY (piped output) — bubbletea handles it; document rather than special-case.

## Reconcile log

### 2026-08-04

Reconciled against `origin/main` before planning. Findings:

- **Premises hold.** No `Makefile`, no Charm dependencies in `go.mod` (only `gopkg.in/yaml.v3`), and none of `internal/tui/events.go`, `internal/tui/tea_renderer.go`, `internal/tui/shell_model.go` exist yet. `cmd/fuse/shell.go` still uses the `bufio.Scanner` `replLoop`; one-shot mode (`cmd/fuse/main.go` → `tui.NewRenderer`) is untouched. No work has been done elsewhere; scope stands as specced.
- **Interface match confirmed.** `agent.Renderer` (in `internal/agent/agent.go`) is exactly `Assistant(text)`, `ToolCall(name, args)`, `ToolResult(name, tools.Result)`, `Errorf(format, ...any)` — the four methods the spec's `TeaRenderer` implements. `func (a *Agent) Run(ctx, []model.Message) ([]model.Message, error)` matches the spec's agent goroutine signature verbatim. `ModelHeader` is a concrete-only `*tui.Renderer` method, not part of the interface, so `TeaRenderer` need not implement it.
- **Pre-existing note (not new scope):** the Phase 1 results file records that `*tui.Renderer.ModelHeader` is now orphaned and `cmd/fuse/shell.go` inlines the same rule literal. This change removes `replLoop` and its inlined header, so the plan should decide whether the TUI status line supersedes `ModelHeader` and whether to drop the now-fully-dead method. Folded into planning as a cleanup consideration, not a scope expansion.
- Go version is `go 1.26.5`; Charm deps are pure-Go (no CGO), consistent with the spec.

No scope change; spec left as-is. `reconciled: true`.
