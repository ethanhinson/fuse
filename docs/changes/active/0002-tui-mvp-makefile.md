---
id: 2
slug: tui-mvp-makefile
title: Bubbletea TUI MVP + Makefile
status: in-progress
priority: high
type: feat
created: 2026-08-03
updated: 2026-08-04
depends_on: []
related: [1]
discovered_from: [1]
adrs: []
spec: docs/superpowers/specs/0002-tui-mvp-makefile.md
plan:
results:
trivial: false
auto_groomable: false
branch: feat/tui-mvp-makefile
claimed_at: 2026-08-04T04:53:56Z
pr:
blocked_by:
reconciled: false
---

## Artifacts

<!-- docket:artifacts:start (generated — do not hand-edit) -->
| Artifact | Link |
|---|---|
| Spec | [0002-tui-mvp-makefile.md](https://github.com/ethanhinson/fuse/blob/docket/docs/superpowers/specs/0002-tui-mvp-makefile.md) |
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
