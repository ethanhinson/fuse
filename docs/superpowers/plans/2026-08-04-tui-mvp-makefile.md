<!-- docket:backlink:start (generated — do not hand-edit) -->
> ↩ **[Change 0002 — Bubbletea TUI MVP + Makefile](https://github.com/ethanhinson/fuse/blob/docket/docs/changes/active/0002-tui-mvp-makefile.md)**
<!-- docket:backlink:end -->

# Implementation Plan: Bubbletea TUI MVP + Makefile (Change 0002)

**Goal:** Add a repo-root `Makefile` and replace the plain `bufio.Scanner` REPL in `fuse shell` with a bubbletea-powered interactive TUI — scrollable transcript viewport, bottom input bar, live status indicator while the agent runs, and the existing slash commands. One-shot `fuse "<task>"` mode is untouched.

**Architecture:** A new `Makefile` at the repo root wraps the `go build/install/test/vet` incantations. Three pure-Go Charm dependencies (`bubbletea`, `bubbles`, `lipgloss`) are added. In `internal/tui`, a small channel-based bridge carries agent output from the agent goroutine into the bubbletea update loop: `events.go` defines five `tea.Msg` types, `tea_renderer.go` implements the existing `agent.Renderer` interface by sending those messages onto a buffered channel, and `shell_model.go` holds the bubbletea `Model` (a `bubbles/viewport` transcript + a `bubbles/textinput` prompt) that drains the channel via the idiomatic `waitForMsg` command and re-arms after each event. `cmd/fuse/shell.go` is rewired: `runShell` builds the model and runs a `tea.Program` with alt-screen; `replLoop`/`handleSlash`/`runPrompt` are removed and their slash-command + agent-turn logic moves into `Update()`. The agent turn runs in a goroutine so the UI stays responsive; `AgentDoneMsg` carries the updated history back.

**Tech Stack:** Go 1.26.5 (darwin/arm64), module `github.com/ethanhinson/fuse`. New deps: `github.com/charmbracelet/bubbletea` (v1.x), `github.com/charmbracelet/bubbles` (v0.x — `viewport`, `textinput`), `github.com/charmbracelet/lipgloss` (v1.x) — all pure Go, no CGO. Testing: standard `testing`; bubbletea models are tested directly via `Update`/`View` on constructed `ShellModel` values with synthetic `tea.Msg`s (no live terminal needed).

**Global Constraints:**
- The `agent.Renderer` interface is fixed: `Assistant(text string)`, `ToolCall(name, args string)`, `ToolResult(name string, res tools.Result)`, `Errorf(format string, a ...any)`. `TeaRenderer` must implement exactly these; it is NOT required to implement `ModelHeader` (concrete-only on `*tui.Renderer`).
- `agent.Agent.Run(ctx context.Context, history []model.Message) ([]model.Message, error)` — the goroutine wraps this call and forwards `AgentErrMsg`/`AgentDoneMsg`.
- Reuse the existing `previewLimit = 120` truncation constant / helper from `renderer.go` for non-verbose `ToolCall`/`ToolResult` formatting; do NOT redefine it.
- One-shot mode (`cmd/fuse/main.go` → `tui.NewRenderer`) and the plain `*tui.Renderer` type stay untouched and must keep compiling/passing tests.
- Slash-command semantics are preserved exactly: `/exit` & `/quit` quit; `/verbose` toggles; `/model NAME` validates via `reg.Resolve` and switches alias; a skill slash command injects the skill body as the next prompt; unknown `/cmd` prints an error line.
- Key bindings: Enter submits (ignored while running), Ctrl+C / Ctrl+D quit, ↑/↓ & PgUp/PgDn scroll the viewport, Ctrl+L clears the transcript.
- **Out of scope (do not plan):** two-pane layout, sidebar, artifacts panel, `/artifacts`, mouse beyond scroll, drag-to-resize, subagent inline blocks, inline (non-alt-screen) mode, one-shot TUI treatment.

> **For agentic workers:** implement this plan task-by-task with TDD — write the focused test(s), then the code, verify per task, then a single full-suite gate at the end. Steps use checkbox (`- [ ]`) syntax for tracking.

---

## File Structure

Files created or modified, grouped by the task that owns them.

**Task 1 — Makefile**
- `Makefile` — repo root; `.PHONY` `build`, `install`, `test`, `lint` targets.

**Task 2 — Charm dependencies**
- `go.mod` / `go.sum` — add `bubbletea`, `bubbles`, `lipgloss` via `go get`.

**Task 3 — event types**
- `internal/tui/events.go` — five `tea.Msg` types.
- `internal/tui/events_test.go` — compile/shape assertions for the message types.

**Task 4 — channel-based renderer**
- `internal/tui/tea_renderer.go` — `TeaRenderer` implementing `agent.Renderer` over a `chan tea.Msg`.
- `internal/tui/tea_renderer_test.go` — each method sends the expected message onto the channel.

**Task 5 — shell model**
- `internal/tui/shell_model.go` — `ShellModel`, `NewShellModel`, `Init`, `Update`, `View`, `waitForMsg`, slash handling, agent goroutine.
- `internal/tui/shell_model_test.go` — Update/View behavior for key bindings, slash commands, message handling, resize.

**Task 6 — wire shell.go to the TUI**
- `cmd/fuse/shell.go` — rewrite `runShell` to run a `tea.Program`; remove `replLoop`/`handleSlash`/`runPrompt`/`shellState` REPL plumbing (fold needed state into `ShellModel`).
- `cmd/fuse/shell_test.go` — update tests that exercised the old REPL to the new construction path.

**Task 7 — final gate + cleanup**
- Whole-suite `go build ./... && go test ./... && go vet ./...`; resolve the now-fully-dead `ModelHeader` per the reconcile note.

---

## Task 1 — Makefile

- [ ] Create `Makefile` at the repo root:
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
- [ ] Verify (no unit test needed for a Makefile, but confirm it works): `make build` produces a `fuse` binary; `make test` runs the suite; `make lint` runs `go vet`. Add `fuse` (the built binary) to `.gitignore` if not already ignored.

**Verification:** `make build && ls -l fuse && make lint` all succeed.

---

## Task 2 — Charm dependencies

- [ ] From the repo root: `go get github.com/charmbracelet/bubbletea github.com/charmbracelet/bubbles github.com/charmbracelet/lipgloss`, then `go mod tidy`.
- [ ] Confirm `go.mod` lists all three (direct) and `go build ./...` still succeeds (nothing consumes them yet — a bare add).

**Verification:** `go build ./... && go mod verify` succeed; `go.mod`/`go.sum` updated.

---

## Task 3 — event types (`internal/tui/events.go`)

- [ ] Write `internal/tui/events_test.go` first: construct each message value and assert its exported fields round-trip (a simple compile+field test; these are dumb structs so the test mostly guards the names/types the model depends on).
- [ ] Create `internal/tui/events.go`:
  ```go
  package tui

  import (
      tea "github.com/charmbracelet/bubbletea"
      "github.com/ethanhinson/fuse/internal/model"
  )

  type AssistantMsg  struct{ Text string }
  type ToolCallMsg   struct{ Name, Args string }
  type ToolResultMsg struct{ Name string; IsError bool; Output string }
  type AgentErrMsg   struct{ Err string }
  type AgentDoneMsg  struct{ History []model.Message }

  // compile-time assertion that each is a tea.Msg (tea.Msg is any, so this is
  // documentary; the real constraint is that Update switches on these types).
  var _ = []tea.Msg{AssistantMsg{}, ToolCallMsg{}, ToolResultMsg{}, AgentErrMsg{}, AgentDoneMsg{}}
  ```

**Verification:** `go test ./internal/tui/` passes; package compiles.

---

## Task 4 — channel-based renderer (`internal/tui/tea_renderer.go`)

- [ ] Write `internal/tui/tea_renderer_test.go` first: build a `TeaRenderer` over a buffered `chan tea.Msg`, call each method, and assert the corresponding message with the right fields lands on the channel. Cover `Assistant`, `ToolCall`, `ToolResult` (both `IsError` false/true), and `Errorf` (format args expanded).
- [ ] Create `internal/tui/tea_renderer.go`:
  ```go
  package tui

  import (
      "fmt"

      tea "github.com/charmbracelet/bubbletea"
      "github.com/ethanhinson/fuse/internal/tools"
  )

  // TeaRenderer implements agent.Renderer by forwarding events onto a channel
  // that the bubbletea ShellModel drains.
  type TeaRenderer struct{ ch chan<- tea.Msg }

  func NewTeaRenderer(ch chan<- tea.Msg) *TeaRenderer { return &TeaRenderer{ch: ch} }

  func (r *TeaRenderer) Assistant(text string)      { r.ch <- AssistantMsg{Text: text} }
  func (r *TeaRenderer) ToolCall(name, args string) { r.ch <- ToolCallMsg{Name: name, Args: args} }
  func (r *TeaRenderer) ToolResult(name string, res tools.Result) {
      r.ch <- ToolResultMsg{Name: name, IsError: res.IsError, Output: res.Output}
  }
  func (r *TeaRenderer) Errorf(format string, a ...any) { r.ch <- AgentErrMsg{Err: fmt.Sprintf(format, a...)} }
  ```
- [ ] Add a compile-time interface assertion in the file: `var _ agent.Renderer = (*TeaRenderer)(nil)` (import `internal/agent`). If this creates an import cycle (tui importing agent), instead place the assertion in the test file or in a small `cmd/fuse` wiring test — verify no cycle exists first (agent does NOT import tui today, so tui→agent is safe).

**Verification:** `go test ./internal/tui/` passes; `var _ agent.Renderer = (*TeaRenderer)(nil)` compiles.

---

## Task 5 — shell model (`internal/tui/shell_model.go`)

- [ ] Write `internal/tui/shell_model_test.go` first, covering (construct a `ShellModel` with a stub agent-builder / injected channel):
  - `tea.WindowSizeMsg` sets viewport height to `Height - 3` (or the chosen chrome height) and width.
  - Enter with non-empty input while not running: appends the user line, sets `running = true`, returns a non-nil cmd; Enter while running is a no-op.
  - Enter on `/exit` and `/quit`: returns `tea.Quit`.
  - `/verbose`: toggles the flag and appends a confirmation line.
  - `/model NAME`: valid name switches `alias`; unknown name appends an error line and does not switch (inject a `*model.Registry` with a known alias).
  - A skill slash command (present in the `slash` map): triggers a prompt run of the skill body.
  - Unknown `/cmd`: appends an "unknown command" line.
  - `AssistantMsg` / `ToolCallMsg` / `ToolResultMsg` (error + non-error, verbose + non-verbose truncation at `previewLimit`) / `AgentErrMsg`: append the correctly-formatted line and re-arm `waitForMsg` (assert a non-nil follow-up cmd).
  - `AgentDoneMsg`: updates `history`, clears `running`, keeps input focused.
  - Ctrl+L clears transcript lines; Ctrl+C / Ctrl+D return `tea.Quit`.
  - `View()` on a sized model contains the status indicator when `running`, the transcript content, and the prompt.
- [ ] Create `internal/tui/shell_model.go`:
  - Fields: `vp viewport.Model`, `input textinput.Model`, `lines []string`, `alias string`, `running bool`, `ch chan tea.Msg` (owned; the renderer gets the send side), `history []model.Message`, plus forwarded shell config needed to build an agent: `cfg config.Config`, `reg *model.Registry`, `verbose bool`, `skillBlock string`, `slash map[string]skills.Skill`, and a `buildAgent` function value injected so tests can stub it.
  - `NewShellModel(...)` constructor taking the same inputs `runShell` currently assembles (cfg, reg, alias, verbose, skillBlock, slash) plus the agent-builder func; initializes viewport + textinput (focused).
  - `Init()` returns `tea.Batch(textinput.Blink, waitForMsg(m.ch))`.
  - `Update()` implements the key bindings, slash handling (ported verbatim from `handleSlash`), message handling, and window resize per the spec's table. On Enter with a real prompt, append the user `model.Message`, set `running`, and return a `tea.Cmd` that starts the agent goroutine (goroutine sends `AgentErrMsg` on error then `AgentDoneMsg{updated}`).
  - `View()` uses lipgloss for a thin status line (`running…` when in flight), the viewport body, a separator rule, and the input prompt (`[alias] > `).
  - `waitForMsg(ch <-chan tea.Msg) tea.Cmd { return func() tea.Msg { return <-ch } }`.
  - A `refreshViewport()` helper that joins `lines`, sets viewport content, and scrolls to bottom.
  - Line formatting mirrors `*tui.Renderer`: `→ name(args)` (args truncated to `previewLimit` unless verbose), `← output` / `✗ output` (truncated unless verbose), `! error`, assistant text verbatim.

**Verification:** `go test ./internal/tui/` passes.

---

## Task 6 — wire `cmd/fuse/shell.go` to the TUI

- [ ] Read the current `cmd/fuse/shell_test.go` to see which behaviors are asserted against the old REPL, and adapt those tests to the new path (construct via `NewShellModel` and drive `Update`, or test `runShell`'s wiring with an injected non-TTY guard). Update tests first.
- [ ] Rewrite `runShell`: keep the flag parsing (`--model`, `--verbose`) and skills loading; build `NewShellModel(...)` (passing `buildAgent` as the injected builder) and run `tea.NewProgram(model, tea.WithAltScreen()).Run()`. Return 0 on clean quit, 1 on program error. Remove `replLoop`, `handleSlash`, `runPrompt`, and the `shellState`-based REPL (fold any still-needed helpers like `systemPrompt()` into the model or a small free function).
- [ ] Keep the startup banner semantics (model name + hint) — render it as initial transcript content in the model rather than `fmt.Fprintf(stdout, ...)`.

**Verification:** `go build ./cmd/fuse && go test ./cmd/fuse/` pass; `internal/tui` still green.

---

## Task 7 — final gate + `ModelHeader` cleanup

- [ ] Per the reconcile note: `*tui.Renderer.ModelHeader` was already orphaned (only its own test referenced it) once `shell.go` inlined the header literal; this change removes that inlined header entirely. Remove `ModelHeader` and its test, OR document why it stays. Prefer removal (dead code) unless one-shot mode needs it — confirm one-shot mode does not call it.
- [ ] Full gate: `go build ./... && go test ./... && go vet ./...` all green. `make build && make lint` succeed.
- [ ] Sanity: `go run ./cmd/fuse models` still works (non-shell path unaffected); confirm one-shot `fuse "<task>"` wiring compiles unchanged.

**Verification:** whole suite green; `make build`, `make test`, `make lint` succeed; one-shot mode untouched.
