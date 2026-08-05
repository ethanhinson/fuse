package tui

import (
	"context"
	"encoding/json"
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

// registryReloadMsg fires when any CommandProvider signals a change.
type registryReloadMsg struct{}

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

	md        *glamour.TermRenderer // nil until first WindowSizeMsg; recreated on resize
	reg       *model.Registry
	slashReg  *SlashRegistry
	completer *slashCompleter
	build     AgentBuilder

	// Subagent tree and inline summary tracking.
	tree         *agent.AgentTree
	agentsActive bool
	agentsModel  *AgentsModel
	inlineByLabel map[string]*inlineAgentState
	inlineByNode  map[string]*inlineAgentState
}

// NewShellModel builds a ShellModel. alias is the starting model alias;
// verbose controls tool arg/output truncation; slashReg is the slash command
// registry; build constructs an agent bound to a renderer. glamourStyle is a
// fixed glamour style name ("dark", "light", etc.) detected before the TUI
// starts so glamour never queries the terminal from inside the bubbletea event
// loop. slashReg may be nil (all slash commands then return unknown).
func NewShellModel(alias string, verbose bool, glamourStyle string, reg *model.Registry, slashReg *SlashRegistry, build AgentBuilder) ShellModel {
	in := textinput.New()
	in.Placeholder = "type a task, /model NAME, /verbose, /exit"
	in.Prompt = ""
	in.Focus()

	vp := viewport.New(0, 0)

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = spinnerStyle

	var completer *slashCompleter
	if slashReg != nil {
		completer = newSlashCompleter(slashReg)
	}

	m := ShellModel{
		vp:           vp,
		input:        in,
		spinner:      sp,
		alias:        alias,
		verbose:      verbose,
		glamourStyle: glamourStyle,
		ch:           make(chan tea.Msg, 64),
		reg:          reg,
		slashReg:     slashReg,
		completer:    completer,
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

// WithTree attaches an agent tree for subagent spawn tracking and live /agents view.
func (m ShellModel) WithTree(t *agent.AgentTree) ShellModel {
	m.tree = t
	m.inlineByLabel = make(map[string]*inlineAgentState)
	m.inlineByNode = make(map[string]*inlineAgentState)
	return m
}

// Init focuses input and arms the first message wait plus cursor blink.
func (m ShellModel) Init() tea.Cmd {
	cmds := []tea.Cmd{textinput.Blink, waitForMsg(m.ch)}
	if m.slashReg != nil {
		cmds = append(cmds, waitForRegistryReload(m.slashReg.Changes()))
	}
	if m.tree != nil {
		cmds = append(cmds, waitForTreeUpdate(m.tree))
	}
	return tea.Batch(cmds...)
}

// waitForMsg blocks on the channel and delivers the next event into Update.
// Re-armed after every received event so the loop stays subscribed.
func waitForMsg(ch <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

// waitForRegistryReload blocks on the registry change channel and returns a
// registryReloadMsg. Re-armed on every receipt.
func waitForRegistryReload(ch <-chan struct{}) tea.Cmd {
	return func() tea.Msg {
		<-ch
		return registryReloadMsg{}
	}
}

// Update handles keys, window resize, and agent events.
func (m ShellModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Agents overlay: most messages delegate to AgentsModel.
	if m.agentsActive && m.agentsModel != nil {
		switch msg := msg.(type) {
		case agentsExitMsg:
			m.agentsActive = false
			m.agentsModel = nil
			return m, nil
		case treeUpdateMsg:
			newModel, _ := m.agentsModel.Update(msg)
			m.agentsModel = newModel.(*AgentsModel)
			if m.tree != nil {
				return m, waitForTreeUpdate(m.tree)
			}
			return m, nil
		case tea.WindowSizeMsg:
			m.agentsModel.width = msg.Width
			m.agentsModel.height = msg.Height
			// Also update the shell model's own size for when we return.
			h := msg.Height - chromeHeight
			if h < 1 {
				h = 1
			}
			m.vp.Width = msg.Width
			m.vp.Height = h
			m.input.Width = msg.Width
			m.ready = true
			return m, nil
		case tea.KeyMsg:
			newModel, cmd := m.agentsModel.Update(msg)
			m.agentsModel = newModel.(*AgentsModel)
			return m, cmd
		default:
			return m, nil
		}
	}

	switch msg := msg.(type) {
	case treeUpdateMsg:
		if m.tree != nil {
			node := m.tree.Node(msg.nodeID)
			if node != nil && node.Depth == 1 {
				m.updateInlineAgent(node)
			}
			return m, waitForTreeUpdate(m.tree)
		}
		return m, nil

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
		// Refresh when a tool call is in flight (spinner glyph) or when inline
		// agent blocks are running (elapsed counter updates on every frame).
		if m.pendingCall != "" || m.hasRunningInline() {
			m.refreshViewport(m.vp.AtBottom())
		}
		if m.running {
			return m, spinCmd // keep spinning
		}
		return m, nil // let it stop once the agent is done

	case registryReloadMsg:
		if m.completer != nil {
			m.completer.refresh()
		}
		m.refreshViewport(m.vp.AtBottom())
		var cmd tea.Cmd
		if m.slashReg != nil {
			cmd = waitForRegistryReload(m.slashReg.Changes())
		}
		return m, cmd

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
		// spawn_agent: insert a live inline block instead of the spinner line.
		if msg.Name == "spawn_agent" && m.tree != nil {
			var input struct {
				Label  string `json:"label"`
				Remote bool   `json:"remote"`
			}
			_ = json.Unmarshal([]byte(msg.Args), &input)
			if input.Label != "" {
				if len(m.lines) > 0 {
					m.lines = append(m.lines, "")
				}
				block := &inlineAgentState{label: input.Label, lineIdx: len(m.lines)}
				if m.inlineByLabel == nil {
					m.inlineByLabel = make(map[string]*inlineAgentState)
				}
				m.inlineByLabel[input.Label] = block
				var runLine string
				if input.Remote {
					runLine = renderInlineRemoteRunning(input.Label, "0s", 0, 0)
				} else {
					runLine = renderInlineRunning(input.Label, "0s", 0, 0)
				}
				parts := strings.SplitN(runLine, "\n", 2)
				m.lines = append(m.lines, parts[0])
				if len(parts) > 1 {
					m.lines = append(m.lines, parts[1])
				} else {
					m.lines = append(m.lines, "")
				}
				m.refreshViewport(true)
				return m, waitForMsg(m.ch)
			}
		}
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
		// spawn_agent: the inline block already shows the final state; skip.
		if msg.Name == "spawn_agent" && m.tree != nil {
			return m, waitForMsg(m.ch)
		}
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
		// After the synthesis text potentially scrolls inline blocks off screen,
		// append a compact footer so the user knows agents ran and can Tab to inspect.
		if len(m.inlineByNode) > 0 {
			m.lines = append(m.lines, subagentFooterStyle.Render(
				fmt.Sprintf("  ↳ %d child agent(s) ran — Tab to inspect tree", len(m.inlineByNode)),
			))
		}
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
			m.refreshInlineBlocks()
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

	// While the completer is active, intercept navigation keys.
	if m.completer != nil && m.completer.active {
		if handled, model, cmd := m.handleCompleterKey(msg); handled {
			return model, cmd
		}
	}

	switch msg.Type {
	case tea.KeyCtrlC, tea.KeyCtrlD:
		return m, tea.Quit
	case tea.KeyTab:
		if m.completer == nil || !m.completer.active {
			return m.enterAgentsView()
		}
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
		if m.completer != nil {
			m.completer.deactivate()
		}
		if strings.HasPrefix(line, "/") {
			return m.handleSlash(line)
		}
		return m.startPrompt(line)
	}

	// Let the text input handle the key first.
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)

	// Update completer filter based on new input.
	if m.completer != nil && !m.running {
		val := m.input.Value()
		if strings.HasPrefix(val, "/") {
			m.completer.activate(val)
		} else {
			m.completer.deactivate()
		}
		m.refreshViewport(m.vp.AtBottom())
	}

	return m, cmd
}

// handleCompleterKey handles Up/Down/Esc/Enter while the completer is active.
// Returns (true, model, cmd) when the key was consumed, (false, _, _) otherwise.
func (m ShellModel) handleCompleterKey(msg tea.KeyMsg) (bool, tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyUp:
		m.completer.moveUp()
		m.refreshViewport(m.vp.AtBottom())
		return true, m, nil
	case tea.KeyDown:
		m.completer.moveDown()
		m.refreshViewport(m.vp.AtBottom())
		return true, m, nil
	case tea.KeyEsc:
		m.completer.deactivate()
		m.input.Reset()
		m.refreshViewport(m.vp.AtBottom())
		return true, m, nil
	case tea.KeyEnter:
		if len(m.completer.visible) == 0 || m.running {
			return false, m, nil
		}
		entry := m.completer.selected()
		expansion := entry.Expansion()
		m.completer.deactivate()

		if entry.Kind == KindMCP {
			// MCP expansion is a complete prompt template — inject into input
			// for the user to append arguments, then leave (don't submit).
			m.input.SetValue(expansion)
			m.input.CursorEnd()
			m.refreshViewport(m.vp.AtBottom())
			return true, m, nil
		}
		// Builtin / skill: inject the command and submit immediately.
		m.input.Reset()
		if !m.running {
			next, cmd := m.dispatchSlashEntry(entry, expansion)
			return true, next, cmd
		}
		return true, m, nil
	}
	return false, m, nil
}

// dispatchSlashEntry runs the slash logic for a selected completer entry.
func (m ShellModel) dispatchSlashEntry(entry SlashEntry, expansion string) (tea.Model, tea.Cmd) {
	// Builtins have a clean "/command" expansion — route through handleSlash
	// to hit the existing switch. Skills and MCP carry their payload in the
	// entry itself, so delegate directly to handleSlashEntry.
	if entry.Kind != KindBuiltin {
		return m.handleSlashEntry(entry, strings.Fields(entry.Command))
	}
	line := strings.TrimSpace(expansion)
	if line == "" {
		line = entry.Command
	}
	return m.handleSlash(line)
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
	case "/agents":
		return m.enterAgentsView()
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

	// Look up in the slash registry.
	if m.slashReg != nil {
		matches := m.slashReg.Filter(cmd)
		for _, e := range matches {
			if e.Command == cmd {
				return m.handleSlashEntry(e, fields)
			}
		}
	}

	m.appendLine(fmt.Sprintf("unknown command %s", cmd))
	return m, nil
}

// handleSlashEntry dispatches a resolved SlashEntry from the registry.
func (m ShellModel) handleSlashEntry(e SlashEntry, fields []string) (tea.Model, tea.Cmd) {
	switch e.Kind {
	case KindBuiltin:
		// Builtins were already handled in handleSlash's switch; this path
		// handles custom builtins added via the registry (currently none).
		return m, nil
	case KindSkill:
		// Skills store their body in the registry snapshot via filter.
		// The SlashEntry.expand() for skills returns "/cmd " — we need the body.
		// Since skills are in the SlashRegistry from SkillProvider which uses
		// skills.Load(), we re-look up the full body via the entry's command.
		// The SkillProvider doesn't store the body in SlashEntry.expand() — it
		// only stores the command string for injection. For actual execution we
		// call startPrompt with the body obtained from the skills.Set.
		// HOWEVER: the ShellModel no longer has a direct reference to the
		// skills.Set. The spec says "Skills and built-ins continue to dispatch
		// via the existing switch + skill-body injection."
		//
		// To support skill body injection without a direct Set reference, the
		// SkillProvider stores body in the expand() closure. We call expand()
		// and trim — if it's just "/cmd ", it means we need the skill body.
		// The SkillProvider must store the actual body in expand(). Let's
		// update SkillProvider to do that.
		body := strings.TrimSpace(e.Expansion())
		if len(fields) > 1 {
			body += "\n\nARGUMENTS: " + strings.Join(fields[1:], " ")
		}
		return m.startPrompt(body)
	case KindMCP:
		expansion := e.Expansion()
		m.input.SetValue(expansion)
		m.input.CursorEnd()
		m.refreshViewport(m.vp.AtBottom())
		return m, nil
	}
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
	// Reset per-turn inline tracking so the footer counter starts fresh.
	m.inlineByLabel = make(map[string]*inlineAgentState)
	m.inlineByNode = make(map[string]*inlineAgentState)

	ch := m.ch
	alias := m.alias
	history := m.history
	build := m.build

	tree := m.tree // capture for closure
	run := func() tea.Msg {
		approve := NewTeaApprovalFunc(ch)
		var r agent.Renderer = NewTeaRenderer(ch)
		if tree != nil {
			rootNode := tree.Node(tree.RootID())
			if rootNode != nil {
				r = NewMultiRenderer(r, NewNodeRenderer(rootNode, tree))
			}
		}
		a, err := build(alias, r, approve)
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

// hasRunningInline returns true if any tracked inline agent block is still running.
func (m *ShellModel) hasRunningInline() bool {
	if m.tree == nil {
		return false
	}
	for nodeID := range m.inlineByNode {
		n := m.tree.Node(nodeID)
		if n != nil && (n.Status == agent.StatusRunning || n.Status == agent.StatusPending) {
			return true
		}
	}
	return false
}

// refreshInlineBlocks updates the elapsed counter for every running inline block.
// Called on each tickMsg so the display doesn't appear frozen during slow LLM calls.
func (m *ShellModel) refreshInlineBlocks() {
	if m.tree == nil || len(m.inlineByNode) == 0 {
		return
	}
	for nodeID, block := range m.inlineByNode {
		node := m.tree.Node(nodeID)
		if node == nil || block.lineIdx+1 >= len(m.lines) {
			continue
		}
		if node.Status != agent.StatusRunning && node.Status != agent.StatusPending {
			continue
		}
		elapsed := "0s"
		if !node.StartedAt.IsZero() {
			elapsed = fmt.Sprintf("%.0fs", time.Since(node.StartedAt).Seconds())
		}
		var runLine string
		if node.RemoteExec {
			runLine = renderInlineRemoteRunning(node.Label, elapsed, node.TokensIn, node.TokensOut)
		} else {
			runLine = renderInlineRunning(node.Label, elapsed, node.TokensIn, node.TokensOut)
		}
		parts := strings.SplitN(runLine, "\n", 2)
		m.lines[block.lineIdx] = parts[0]
		if len(parts) > 1 {
			m.lines[block.lineIdx+1] = parts[1]
		}
	}
}

// enterAgentsView switches to the AgentsModel overlay.
func (m ShellModel) enterAgentsView() (tea.Model, tea.Cmd) {
	if m.tree == nil {
		m.appendLine("no agent tree active")
		return m, nil
	}
	m.agentsModel = NewAgentsModel(m.tree)
	m.agentsModel.width = m.vp.Width
	m.agentsModel.height = m.vp.Height + chromeHeight
	m.agentsActive = true
	return m, nil
}

// updateInlineAgent refreshes the two-line inline block for a depth-1 child node.
func (m *ShellModel) updateInlineAgent(node *agent.AgentNode) {
	if m.inlineByLabel == nil {
		return
	}
	// Find or create the tracking entry.
	block, ok := m.inlineByNode[node.ID]
	if !ok {
		if b, ok2 := m.inlineByLabel[node.Label]; ok2 {
			b.nodeID = node.ID
			if m.inlineByNode == nil {
				m.inlineByNode = make(map[string]*inlineAgentState)
			}
			m.inlineByNode[node.ID] = b
			block = b
		}
	}
	if block == nil || block.lineIdx+1 >= len(m.lines) {
		return
	}

	elapsed := "0s"
	if !node.StartedAt.IsZero() {
		dur := time.Since(node.StartedAt)
		if !node.EndedAt.IsZero() {
			dur = node.EndedAt.Sub(node.StartedAt)
		}
		elapsed = fmt.Sprintf("%.0fs", dur.Seconds())
	}

	var rendered string
	switch node.Status {
	case agent.StatusRunning, agent.StatusPending:
		if node.RemoteExec {
			rendered = renderInlineRemoteRunning(node.Label, elapsed, node.TokensIn, node.TokensOut)
		} else {
			rendered = renderInlineRunning(node.Label, elapsed, node.TokensIn, node.TokensOut)
		}
	case agent.StatusDone:
		result := ""
		for _, e := range node.Events {
			if e.Kind == agent.KindDone {
				if r, ok2 := e.Payload["result"].(string); ok2 {
					result = r
				}
			}
		}
		rendered = renderInlineDone(elapsed, node.TokensIn, node.TokensOut, result)
	case agent.StatusError:
		errMsg := ""
		for _, e := range node.Events {
			if e.Kind == agent.KindError {
				if s, ok2 := e.Payload["error"].(string); ok2 {
					errMsg = s
				}
			}
		}
		rendered = renderInlineError(elapsed, node.TokensIn, node.TokensOut, errMsg)
	default:
		return
	}

	parts := strings.SplitN(rendered, "\n", 2)
	m.lines[block.lineIdx] = parts[0]
	if len(parts) > 1 {
		m.lines[block.lineIdx+1] = parts[1]
	}
	m.refreshViewport(m.vp.AtBottom())
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
	// Prepend completer overlay above the content.
	if m.completer != nil && m.completer.active {
		overlay := m.completer.View(m.vp.Width)
		if overlay != "" {
			content = content + "\n" + overlay
		}
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
// status line at the very bottom. When the agents overlay is active it
// delegates entirely to AgentsModel.View().
func (m ShellModel) View() string {
	if m.agentsActive && m.agentsModel != nil {
		return m.agentsModel.View()
	}
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
		if m.tree != nil {
			status += "  " + ruleStyle.Render("Tab → agents")
		}
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
