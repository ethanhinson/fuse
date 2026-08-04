package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/muesli/reflow/wordwrap"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/skills"
)

// chromeHeight is the number of terminal rows reserved for the status line,
// separator, and input prompt; the viewport gets the rest.
const chromeHeight = 3

// AgentBuilder constructs an agent that renders through the given Renderer.
// approve is the HITL gate function for the current turn.
// cmd/fuse injects its buildAgent via this signature so the model stays
// decoupled from gateway/config wiring and is testable with a stub.
type AgentBuilder func(alias string, r agent.Renderer, approve permissions.ApprovalFunc) (*agent.Agent, error)

// approvalState holds the in-flight permission request and the channel to
// send the user's decision back to the waiting gate goroutine.
type approvalState struct {
	req    PermissionRequestMsg
	render string // pre-rendered approval block text
}

// ShellModel is the bubbletea model backing `fuse shell`: a scrollable
// transcript viewport above a single-line input prompt, driven by agent
// output arriving on a channel from a TeaRenderer.
type ShellModel struct {
	vp      viewport.Model
	input   textinput.Model
	spinner spinner.Model

	lines        []string
	pendingCall  string // styled tool-call text in flight; "" when idle
	alias        string
	verbose      bool
	running      bool
	ready        bool   // first WindowSizeMsg seen (viewport sized)
	glamourStyle string // fixed glamour style; detected before TUI starts

	runStart     time.Time
	inputTokens  int
	outputTokens int

	ch       chan tea.Msg
	history  []model.Message
	approval *approvalState // non-nil while waiting for user's y/s/n

	md    *glamour.TermRenderer // nil until first WindowSizeMsg; recreated on resize
	reg   *model.Registry
	slash map[string]skills.Skill
	build AgentBuilder
}

// NewShellModel builds a ShellModel. alias is the starting model alias;
// verbose controls tool arg/output truncation; slash is the skill slash-command
// map; build constructs an agent bound to a renderer. glamourStyle is a fixed
// glamour style name ("dark", "light", etc.) detected before the TUI starts so
// glamour never queries the terminal from inside the bubbletea event loop.
func NewShellModel(alias string, verbose bool, glamourStyle string, reg *model.Registry, slash map[string]skills.Skill, build AgentBuilder) ShellModel {
	in := textinput.New()
	in.Placeholder = "type a task, /model NAME, /verbose, /exit"
	in.Prompt = ""
	in.Focus()

	vp := viewport.New(0, 0)

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = spinnerStyle

	m := ShellModel{
		vp:           vp,
		input:        in,
		spinner:      sp,
		alias:        alias,
		verbose:      verbose,
		glamourStyle: glamourStyle,
		ch:           make(chan tea.Msg, 64),
		reg:          reg,
		slash:        slash,
		build:        build,
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
		if r, err := glamour.NewTermRenderer(
			glamour.WithStylePath(m.glamourStyle),
			glamour.WithWordWrap(msg.Width),
		); err == nil {
			m.md = r
		}
		m.refreshViewport(true)
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd

	case spinner.TickMsg:
		var spinCmd tea.Cmd
		m.spinner, spinCmd = m.spinner.Update(msg)
		// Refresh viewport when a tool call is in flight so the spinner frame
		// updates in place on the pending-call line.
		if m.pendingCall != "" {
			m.refreshViewport(m.vp.AtBottom())
		}
		if m.running {
			return m, spinCmd // keep spinning
		}
		return m, nil // let it stop once the agent is done

	case AssistantMsg:
		text := msg.Text
		if m.md != nil {
			if rendered, err := m.md.Render(text); err == nil {
				text = strings.TrimRight(rendered, "\n")
			}
		}
		m.appendLine(assistantStyle.Render(text))
		return m, waitForMsg(m.ch)

	case ToolCallMsg:
		args := msg.Args
		if !m.verbose {
			args = truncate(args, previewLimit)
		}
		// Blank line before each bullet for visual breathing room.
		if len(m.lines) > 0 {
			m.lines = append(m.lines, "")
		}
		// Store the call text as pending; it's rendered with the live spinner
		// frame in refreshViewport until the result arrives.
		m.pendingCall = toolNameStyle.Render(msg.Name) + toolArgsStyle.Render("("+args+")")
		m.refreshViewport(true)
		return m, waitForMsg(m.ch)

	case ToolResultMsg:
		// Settle the pending bullet to a static ● now that we have the result.
		if m.pendingCall != "" {
			m.lines = append(m.lines, toolBulletStyle.Render("●")+" "+m.pendingCall)
			m.pendingCall = ""
		}
		out := msg.Output
		if !m.verbose {
			out = truncate(out, previewLimit)
		}
		m.appendResultLines(out, msg.IsError, msg.Name)
		return m, waitForMsg(m.ch)

	case AgentErrMsg:
		// Settle any pending call before showing the error.
		if m.pendingCall != "" {
			m.lines = append(m.lines, toolBulletStyle.Render("●")+" "+m.pendingCall)
			m.pendingCall = ""
		}
		m.appendLine(agentErrStyle.Render("! " + msg.Err))
		return m, waitForMsg(m.ch)

	case AgentDoneMsg:
		m.pendingCall = ""
		m.history = msg.History
		m.running = false
		m.approval = nil
		m.input.Focus()
		m.refreshViewport(m.vp.AtBottom())
		return m, waitForMsg(m.ch)

	case PermissionRequestMsg:
		// Settle any pending spinner line before showing the block.
		if m.pendingCall != "" {
			m.lines = append(m.lines, toolBulletStyle.Render("●")+" "+m.pendingCall)
			m.pendingCall = ""
		}
		block := renderApprovalBlock(msg.Request.ToolName, msg.Request.Preview)
		m.appendLine(block)
		m.approval = &approvalState{req: msg, render: block}
		return m, waitForMsg(m.ch)

	case TokensMsg:
		m.inputTokens += msg.Input
		m.outputTokens += msg.Output
		return m, waitForMsg(m.ch)

	case tickMsg:
		if m.running {
			return m, tick()
		}
		return m, nil
	}

	// Forward anything else (e.g. cursor blink) to the input.
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// handleKey processes key bindings.
func (m ShellModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// While an approval is pending, intercept y/s/n/Escape before normal input.
	if m.approval != nil {
		return m.handleApprovalKey(msg)
	}

	switch msg.Type {
	case tea.KeyCtrlC, tea.KeyCtrlD:
		return m, tea.Quit
	case tea.KeyCtrlL:
		m.lines = nil
		m.pendingCall = ""
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

// handleApprovalKey handles y/s/n/Escape while waiting for permission input.
func (m ShellModel) handleApprovalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Type == tea.KeyCtrlC || msg.Type == tea.KeyCtrlD {
		return m, tea.Quit
	}
	ch := m.approval.req.RespCh
	switch strings.ToLower(msg.String()) {
	case "y":
		m.approval = nil
		ch <- approvalResponse{Approved: true, AllowForSession: false}
	case "s":
		m.approval = nil
		ch <- approvalResponse{Approved: true, AllowForSession: true}
	case "n", "escape":
		m.approval = nil
		ch <- approvalResponse{Approved: false}
	default:
		// Ignore any other key while awaiting approval.
		return m, nil
	}
	return m, waitForMsg(m.ch)
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
		body := sk.Body
		if len(fields) > 1 {
			body += "\n\nARGUMENTS: " + strings.Join(fields[1:], " ")
		}
		return m.startPrompt(body)
	}
	m.appendLine(fmt.Sprintf("unknown command %s", cmd))
	return m, nil
}

// startPrompt appends the user line, marks running, and returns a cmd that runs
// one agent turn in a goroutine, forwarding output onto the channel.
func (m ShellModel) startPrompt(line string) (tea.Model, tea.Cmd) {
	m.appendLine(headerStyle.Render(fmt.Sprintf("\n── %s ──────────────", m.alias)))
	m.history = append(m.history, model.Message{Role: "user", Content: line})
	m.running = true
	m.runStart = time.Now()
	m.inputTokens = 0
	m.outputTokens = 0

	ch := m.ch
	alias := m.alias
	history := m.history
	build := m.build

	run := func() tea.Msg {
		approve := NewTeaApprovalFunc(ch)
		a, err := build(alias, NewTeaRenderer(ch), approve)
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
	// run sends events onto the channel; waitForMsg picks them up.
	// tick drives the elapsed-time counter; spinner.Tick starts the animation.
	return m, tea.Batch(run, tick(), m.spinner.Tick)
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

// appendResultLines renders a tool result indented under the previous bullet.
// For file-reading tools it adds a line-number gutter:
//
//	  └ 1 │ package main
//	    2 │
//	    3 │ import …
//
// All other results use the plain prefix form:
//
//	  └ first line
//	    subsequent lines…
func (m *ShellModel) appendResultLines(out string, isError bool, toolName string) {
	atBottom := !m.ready || m.vp.AtBottom()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	useGutter := !isError && isFileReadTool(toolName) && len(lines) > 1
	gutterW := len(fmt.Sprintf("%d", len(lines))) // digits needed for widest line number
	for i, l := range lines {
		var rendered string
		if i == 0 {
			if isError {
				rendered = "  " + errorArrowStyle.Render("✗") + " " + errorTextStyle.Render(l)
			} else if useGutter {
				g := gutterStyle.Render(fmt.Sprintf("%*d │ ", gutterW, i+1))
				rendered = resultPrefixStyle.Render("  └") + " " + g + l
			} else {
				rendered = resultPrefixStyle.Render("  └") + " " + l
			}
		} else {
			if useGutter {
				g := gutterStyle.Render(fmt.Sprintf("%*d │ ", gutterW, i+1))
				rendered = "    " + g + l
			} else {
				rendered = "    " + l
			}
		}
		m.lines = append(m.lines, rendered)
	}
	m.refreshViewport(atBottom)
}

// isFileReadTool returns true for tools whose output is file content and
// therefore benefits from a line-number gutter.
func isFileReadTool(name string) bool {
	lower := strings.ToLower(name)
	return strings.Contains(lower, "read") || lower == "view" || strings.Contains(lower, "file")
}

// renderApprovalBlock returns the styled inline permission-request block.
func renderApprovalBlock(toolName, previewText string) string {
	return approvalBorderStyle.Render(
		approvalHeaderStyle.Render("⚠  Permission required") + "\n\n" +
			"  Tool:  " + toolName + "\n" +
			"  Cmd:   " + previewText + "\n\n" +
			approvalKeysStyle.Render("  [y] allow once   [s] allow for session   [n] deny"),
	)
}

// tick fires once per second while the agent is running, driving the elapsed
// timer in the status bar.
func tick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{} })
}

func formatElapsed(d time.Duration) string {
	s := int(d.Seconds())
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	return fmt.Sprintf("%dm %ds", s/60, s%60)
}

func formatTokens(n int) string {
	if n == 0 {
		return ""
	}
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}

// refreshViewport sets viewport content and, when followBottom is true, scrolls
// to the bottom. Content is word-wrapped to the viewport width so long lines
// (assistant prose, tool output) don't run off screen. When a tool call is in
// flight its animated spinner line is appended after the settled lines.
// No-op before the first WindowSizeMsg.
func (m *ShellModel) refreshViewport(followBottom bool) {
	if !m.ready {
		return
	}
	content := strings.Join(m.lines, "\n")
	if m.pendingCall != "" {
		content += "\n" + m.spinner.View() + " " + m.pendingCall
	}
	if m.vp.Width > 0 {
		content = wordwrap.String(content, m.vp.Width)
	}
	// Pad the top so sparse content sticks to the bottom like a chat interface.
	if m.vp.Height > 0 {
		if lineCount := strings.Count(content, "\n") + 1; lineCount < m.vp.Height {
			content = strings.Repeat("\n", m.vp.Height-lineCount) + content
		}
	}
	m.vp.SetContent(content)
	if followBottom {
		m.vp.GotoBottom()
	}
}

// View renders the transcript, a separator rule, the input prompt, and a fixed
// status line at the very bottom.
func (m ShellModel) View() string {
	width := m.vp.Width
	if width < 1 {
		width = 40
	}
	rule := ruleStyle.Render(strings.Repeat("─", width))
	prompt := promptAliasStyle.Render("["+m.alias+"]") + " > " + m.input.View()

	var status string
	switch {
	case m.approval != nil:
		status = approvalHeaderStyle.Render("⚠") + " " +
			statusRunStyle.Render("Awaiting permission…") + " " +
			ruleStyle.Render("press y · s · n")
	case m.running:
		elapsed := formatElapsed(time.Since(m.runStart))
		tok := formatTokens(m.inputTokens)
		meta := elapsed
		if tok != "" {
			meta += " · ↑ " + tok + " tokens"
		}
		status = m.spinner.View() + " " +
			statusRunStyle.Render("Thinking…") + " " +
			ruleStyle.Render("("+meta+")")
	default:
		status = statusModelStyle.Render(m.alias)
	}

	var b strings.Builder
	b.WriteString(m.vp.View())
	b.WriteByte('\n')
	b.WriteString(rule)
	b.WriteByte('\n')
	b.WriteString(prompt)
	b.WriteByte('\n')
	b.WriteString(status)
	return b.String()
}
