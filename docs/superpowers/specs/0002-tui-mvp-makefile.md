<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0002 — Bubbletea TUI MVP + Makefile](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0002-tui-mvp-makefile.md)**
<!-- docket:backlink:end -->

# Fuse TUI MVP + Makefile

**Change:** [0002-tui-mvp-makefile](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0002-tui-mvp-makefile.md)

## Context

Phase 1 (change 0001) shipped a plain line-based renderer (`internal/tui/renderer.go`) and a `bufio.Scanner` REPL (`cmd/fuse/shell.go`). There is no build script, and no interactive TUI. This change adds both: a `Makefile` for building/installing/testing, and a bubbletea-powered interactive shell replacing the plain REPL.

The full Phase 2 TUI vision (two-pane layout, sidebar, drag-to-resize, subagent inline blocks) is deferred; it requires subagents and hooks that don't exist yet. This change builds the core interactive surface only.

## Goals

1. `make build / install / test` — quick build commands, no `go install` archaeology.
2. `fuse shell` opens a bubbletea TUI: scrollable transcript pane, prompt input bar at the bottom, streaming agent output rendered as lines. Replaces the raw `bufio.Scanner` loop without changing any behavior the user can observe other than layout and keyboard UX.

## Out of scope

- Two-pane layout, sidebar, artifacts panel, `/artifacts` command.
- Mouse events, drag-to-resize pane divider.
- Subagent inline progress blocks.
- Alt-screen vs inline mode choice (alt-screen only in this change).
- One-shot `fuse "<task>"` — keeps the existing plain `Renderer`.

## Architecture

### Makefile

```makefile
.PHONY: build install test lint

build:
	go build -o fuse ./cmd/fuse

install:
	go install ./cmd/fuse

test:
	go test ./...

lint:
	go vet ./...
```

Lives at the repo root. No external tooling required.

### New dependencies

```
github.com/charmbracelet/bubbletea   v1.x
github.com/charmbracelet/bubbles     v0.x  (viewport, textinput)
github.com/charmbracelet/lipgloss    v1.x
```

Added via `go get`; all are pure Go, no CGO.

### Event types (`internal/tui/events.go`)

New file. Five `tea.Msg` types carry agent output from the agent goroutine back to the bubbletea model:

```go
type AssistantMsg  struct{ Text string }
type ToolCallMsg   struct{ Name, Args string }
type ToolResultMsg struct{ Name string; IsError bool; Output string }
type AgentErrMsg   struct{ Err string }
type AgentDoneMsg  struct{ History []model.Message }
```

### Channel-based renderer (`internal/tui/tea_renderer.go`)

New file. Implements `agent.Renderer`. Each method sends a `tea.Msg` onto a buffered `chan tea.Msg` that the bubbletea model drains. No reference to `*tea.Program` needed — the bubbletea model holds the channel and uses the idiomatic `waitForMsg` cmd pattern to forward events into the update loop.

```go
type TeaRenderer struct{ ch chan<- tea.Msg }

func (r *TeaRenderer) Assistant(text string)                { r.ch <- AssistantMsg{text} }
func (r *TeaRenderer) ToolCall(name, args string)           { r.ch <- ToolCallMsg{name, args} }
func (r *TeaRenderer) ToolResult(name string, res tools.Result) {
    r.ch <- ToolResultMsg{name, res.IsError, res.Output}
}
func (r *TeaRenderer) Errorf(format string, a ...any)      { r.ch <- AgentErrMsg{fmt.Sprintf(format, a...)} }
```

### Shell model (`internal/tui/shell_model.go`)

New file. The bubbletea `Model` for `fuse shell`. 

**Layout** (full terminal height, no alt-screen panes):
```
┌──────────────────────────────────┐
│  scrollable transcript           │  ← viewport.Model, fills height-3
│  (agent output accumulates here) │
├──────────────────────────────────┤
│ [alias] >  _                     │  ← textinput.Model, 1 line
└──────────────────────────────────┘
```

**State fields:**
```go
type ShellModel struct {
    vp       viewport.Model
    input    textinput.Model
    lines    []string          // raw content lines
    alias    string
    running  bool              // agent goroutine in flight
    ch       <-chan tea.Msg    // event channel from TeaRenderer
    history  []model.Message
    // shell config forwarded from runShell
    cfg      config.Config
    reg      *model.Registry
    verbose  bool
    skillBlock string
    slash    map[string]skills.Skill
}
```

**Init:** focus textinput, return `waitForMsg(ch)`.

**Key bindings:**
| Key | Action |
|---|---|
| Enter | Submit prompt (if not already running) |
| Ctrl+C / Ctrl+D | Quit |
| ↑ / ↓ or PgUp / PgDn | Scroll transcript |
| Ctrl+L | Clear transcript |

**Update:**

- `tea.WindowSizeMsg` → resize viewport height to `Height - 3`.
- `tea.KeyMsg` → handle bindings above; on Enter, start agent goroutine (see below), set `running = true`.
- `AssistantMsg` → append text line(s) to `lines`, refresh viewport, re-arm `waitForMsg`.
- `ToolCallMsg` → append `→ name(truncated-args)` line.
- `ToolResultMsg` → append `← output` or `✗ output` line (respects `verbose` flag).
- `AgentErrMsg` → append `! error` line.
- `AgentDoneMsg` → update `history`, set `running = false`, re-focus input.

**Agent goroutine (started on Enter):**
```go
go func() {
    updated, err := a.Run(ctx, history)
    if err != nil {
        ch <- AgentErrMsg{err.Error()}
    }
    ch <- AgentDoneMsg{updated}
}()
```

`buildAgent` is called with a `*TeaRenderer` instead of `*Renderer`.

**waitForMsg cmd:**
```go
func waitForMsg(ch <-chan tea.Msg) tea.Cmd {
    return func() tea.Msg { return <-ch }
}
```

Re-armed after every received event so the bubbletea loop stays subscribed.

**View:** lipgloss for a thin status line (`running...` indicator when agent is in flight), viewport content, separator rule, input prompt.

### Integration in `cmd/fuse/shell.go`

`runShell` constructs the `ShellModel` and starts a `tea.Program` with `tea.WithAltScreen()`. The `replLoop` / `bufio.Scanner` path is removed entirely. Slash command handling (`/model`, `/verbose`, `/exit`, skill commands) moves into the bubbletea `Update()` — same logic, same slash map, just no `fmt.Fprintf(out, ...)`.

### One-shot mode unchanged

`fuse "<task>"` in `cmd/fuse/main.go` continues to use `tui.NewRenderer(stdout, verbose)`. No change.

## Open questions

- Should `fuse shell` fall back to the plain REPL when stdout is not a TTY (e.g. piped)? Bubbletea handles this gracefully itself (it detects non-TTY and errors), so probably not needed — document the behavior.
- Truncation limit for `ToolCallMsg` / `ToolResultMsg` in non-verbose mode: keep the existing `previewLimit = 120` constant from `renderer.go`.
