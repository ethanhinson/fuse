package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/skills"
)

// chromeHeight is the number of terminal rows reserved for the status line,
// separator, and input prompt; the viewport gets the rest.
const chromeHeight = 3

// AgentBuilder constructs an agent that renders through the given Renderer.
// cmd/fuse injects its buildAgent via this signature so the model stays
// decoupled from gateway/config wiring and is testable with a stub.
type AgentBuilder func(alias string, r agent.Renderer) (*agent.Agent, error)

// ShellModel is the bubbletea model backing `fuse shell`: a scrollable
// transcript viewport above a single-line input prompt, driven by agent
// output arriving on a channel from a TeaRenderer.
type ShellModel struct {
	vp    viewport.Model
	input textinput.Model

	lines   []string
	alias   string
	verbose bool
	running bool
	ready   bool // first WindowSizeMsg seen (viewport sized)

	ch      chan tea.Msg
	history []model.Message

	reg   *model.Registry
	slash map[string]skills.Skill
	build AgentBuilder
}

// NewShellModel builds a ShellModel. alias is the starting model alias;
// verbose controls tool arg/output truncation; slash is the skill slash-command
// map; build constructs an agent bound to a renderer.
func NewShellModel(alias string, verbose bool, reg *model.Registry, slash map[string]skills.Skill, build AgentBuilder) ShellModel {
	in := textinput.New()
	in.Placeholder = "type a task, /model NAME, /verbose, /exit"
	in.Prompt = ""
	in.Focus()

	vp := viewport.New(0, 0)

	m := ShellModel{
		vp:      vp,
		input:   in,
		alias:   alias,
		verbose: verbose,
		ch:      make(chan tea.Msg, 64),
		reg:     reg,
		slash:   slash,
		build:   build,
	}
	m.appendLine(fmt.Sprintf("Fuse  %s", alias))
	m.appendLine("Type a task, /model NAME to switch, /verbose to toggle, /exit to quit.")
	return m
}

// Channel exposes the model's event channel so callers (or a TeaRenderer built
// outside the goroutine) can send onto it. The renderer used per-turn is built
// against this channel.
func (m ShellModel) Channel() chan tea.Msg { return m.ch }

// Init focuses input and arms the first message wait plus cursor blink.
func (m ShellModel) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, waitForMsg(m.ch))
}

// waitForMsg blocks on the channel and delivers the next event into Update.
// Re-armed after every received event so the loop stays subscribed.
func waitForMsg(ch <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

// Update handles keys, window resize, and agent events.
func (m ShellModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		h := msg.Height - chromeHeight
		if h < 1 {
			h = 1
		}
		m.vp.Width = msg.Width
		m.vp.Height = h
		m.input.Width = msg.Width
		m.ready = true
		m.refreshViewport(true)
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd

	case AssistantMsg:
		m.appendLine(msg.Text)
		return m, waitForMsg(m.ch)

	case ToolCallMsg:
		args := msg.Args
		if !m.verbose {
			args = truncate(args, previewLimit)
		}
		m.appendLine(fmt.Sprintf("→ %s(%s)", msg.Name, args))
		return m, waitForMsg(m.ch)

	case ToolResultMsg:
		prefix := "←"
		if msg.IsError {
			prefix = "✗"
		}
		out := msg.Output
		if !m.verbose {
			out = truncate(out, previewLimit)
		}
		m.appendLine(fmt.Sprintf("%s %s", prefix, out))
		return m, waitForMsg(m.ch)

	case AgentErrMsg:
		m.appendLine("! " + msg.Err)
		return m, waitForMsg(m.ch)

	case AgentDoneMsg:
		m.history = msg.History
		m.running = false
		m.input.Focus()
		return m, waitForMsg(m.ch)
	}

	// Forward anything else (e.g. cursor blink) to the input.
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// handleKey processes key bindings.
func (m ShellModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlC, tea.KeyCtrlD:
		return m, tea.Quit
	case tea.KeyCtrlL:
		m.lines = nil
		m.refreshViewport(true)
		return m, nil
	case tea.KeyPgUp:
		m.vp.HalfViewUp()
		return m, nil
	case tea.KeyPgDown:
		m.vp.HalfViewDown()
		return m, nil
	case tea.KeyEnter:
		line := strings.TrimSpace(m.input.Value())
		if line == "" || m.running {
			return m, nil
		}
		m.input.Reset()
		if strings.HasPrefix(line, "/") {
			return m.handleSlash(line)
		}
		return m.startPrompt(line)
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// handleSlash ports the pre-TUI slash semantics: /exit & /quit quit; /verbose
// toggles; /model NAME validates + switches; a known skill command injects its
// body as the next prompt; anything else prints an error line.
func (m ShellModel) handleSlash(line string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(line)
	cmd := fields[0]
	switch cmd {
	case "/exit", "/quit":
		return m, tea.Quit
	case "/verbose":
		m.verbose = !m.verbose
		m.appendLine(fmt.Sprintf("verbose = %v", m.verbose))
		return m, nil
	case "/model":
		if len(fields) < 2 {
			m.appendLine("usage: /model NAME")
			return m, nil
		}
		name := fields[1]
		if _, err := m.reg.Resolve(name); err != nil {
			m.appendLine(fmt.Sprintf("unknown model %q", name))
			return m, nil
		}
		m.alias = name
		m.appendLine(fmt.Sprintf("switched to %s", name))
		return m, nil
	}
	if sk, ok := m.slash[cmd]; ok {
		return m.startPrompt(sk.Body)
	}
	m.appendLine(fmt.Sprintf("unknown command %s", cmd))
	return m, nil
}

// startPrompt appends the user line, marks running, and returns a cmd that runs
// one agent turn in a goroutine, forwarding output onto the channel.
func (m ShellModel) startPrompt(line string) (tea.Model, tea.Cmd) {
	m.appendLine(fmt.Sprintf("\n── %s ──────────────", m.alias))
	m.history = append(m.history, model.Message{Role: "user", Content: line})
	m.running = true

	ch := m.ch
	alias := m.alias
	history := m.history
	build := m.build

	run := func() tea.Msg {
		a, err := build(alias, NewTeaRenderer(ch))
		if err != nil {
			ch <- AgentErrMsg{Err: err.Error()}
			ch <- AgentDoneMsg{History: history}
			return nil
		}
		updated, rerr := a.Run(context.Background(), history)
		if rerr != nil {
			ch <- AgentErrMsg{Err: rerr.Error()}
		}
		if updated == nil {
			updated = history
		}
		ch <- AgentDoneMsg{History: updated}
		return nil
	}
	// run returns nil and only sends onto the channel; waitForMsg picks up the
	// events. Launch it as a tea.Cmd so bubbletea runs it off the event loop.
	return m, run
}

// appendLine adds one logical line (which may itself contain newlines) and
// refreshes the viewport.
func (m *ShellModel) appendLine(s string) {
	atBottom := !m.ready || m.vp.AtBottom()
	for _, l := range strings.Split(s, "\n") {
		m.lines = append(m.lines, l)
	}
	m.refreshViewport(atBottom)
}

// refreshViewport sets viewport content and, when followBottom is true,
// scrolls to the bottom. No-op before the first WindowSizeMsg sizes the viewport.
func (m *ShellModel) refreshViewport(followBottom bool) {
	if !m.ready {
		return
	}
	m.vp.SetContent(strings.Join(m.lines, "\n"))
	if followBottom {
		m.vp.GotoBottom()
	}
}

var (
	statusStyle = lipgloss.NewStyle().Bold(true)
	ruleStyle   = lipgloss.NewStyle().Faint(true)
)

// View renders the status line, transcript, a separator rule, and the prompt.
func (m ShellModel) View() string {
	status := m.alias
	if m.running {
		status += "  running…"
	}
	width := m.vp.Width
	if width < 1 {
		width = 40
	}
	rule := ruleStyle.Render(strings.Repeat("─", width))
	var b strings.Builder
	b.WriteString(statusStyle.Render(status))
	b.WriteByte('\n')
	b.WriteString(m.vp.View())
	b.WriteByte('\n')
	b.WriteString(rule)
	b.WriteByte('\n')
	b.WriteString(fmt.Sprintf("[%s] > %s", m.alias, m.input.View()))
	return b.String()
}
