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
	"github.com/muesli/reflow/ansi"
	"github.com/muesli/reflow/wordwrap"
	"github.com/muesli/reflow/wrap"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/banner"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/ratelimit"
	"github.com/ethanhinson/fuse/internal/version"
)

// chromeHeight is the number of terminal rows reserved for the status line,
// separator, and input prompt; the viewport gets the rest.
const chromeHeight = 3

// AgentBuilder constructs an agent that renders through the given Renderer.
// approve is the HITL gate function for the current turn.
// cmd/fuse injects its buildAgent via this signature so the model stays
// decoupled from gateway/config wiring and is testable with a stub.
type AgentBuilder func(alias string, r agent.Renderer, approve permissions.ApprovalFunc) (*agent.Agent, error)

// approvalState holds one in-flight permission request and the channel to
// send the user's decision back to the waiting gate goroutine. Requests queue
// FIFO: with N parallel agents prompting concurrently, each request keeps its
// RespCh alive until the user answers it — overwriting would orphan the gate
// goroutine forever (the deadlock behind the historical TUI freezes).
// The pending modal renders as a live overlay (never appended to the
// transcript), so it vanishes the moment it is answered; the decision lands
// in the transcript and log as a compact timestamped record instead.
type approvalState struct {
	req PermissionRequestMsg
}

// approvalRecord is one answered permission request, kept for the session
// and recallable via /approvals.
type approvalRecord struct {
	ts       time.Time
	tool     string
	preview  string
	decision string // "allowed" | "allowed for session" | "denied" | "denied (turn ended)"
	approved bool
}

// renderApprovalRecord renders one compact decision line for the transcript.
func renderApprovalRecord(r approvalRecord) string {
	mark := agentErrStyle.Render("✗")
	if r.approved {
		mark = toolBulletStyle.Render("✓")
	}
	return headerStyle.Render(r.ts.Format("15:04:05")) + " " +
		approvalHeaderStyle.Render("⚠") + " " +
		sanitizeDisplay(truncate(r.preview, 70)) + " — " +
		mark + " " + headerStyle.Render(r.decision)
}

// registryReloadMsg fires when any CommandProvider signals a change.
type registryReloadMsg struct{}

// transcriptLine is one logical transcript row stored with its decoration kept
// separate from its content so continuation rows can carry a different prefix
// than the first row. Prefix and content are stored apart (rather than one
// pre-concatenated string) so refreshViewport can re-wrap with a hanging indent
// from the stored structure on every refresh and resize.
type transcriptLine struct {
	first string // styled prefix for the first visual row (may be "")
	cont  string // styled prefix for continuation rows; same printable width as first
	text  string // the content (sanitized, possibly styled)
	pre   bool   // pre-wrapped upstream (glamour); skip wordwrap, hard-wrap safety only
}

// pendingToolCall is an announced-but-unresolved tool call. A batched model
// response announces every call before any result arrives, so these queue
// FIFO and each result renders paired with its own call text — otherwise a
// 14-call batch shows one bullet followed by 14 unbroken result blocks.
type pendingToolCall struct{ name, text string }

// enqueueApproval appends a permission request to the FIFO queue. The head
// renders as a live overlay in View; nothing is written to the transcript
// until the request is answered.
func (m *ShellModel) enqueueApproval(msg PermissionRequestMsg) {
	m.approvals = append(m.approvals, approvalState{req: msg})
	m.refreshViewport(m.vp.AtBottom())
}

// answerApproval sends resp to the head request, pops it (the overlay shows
// the next queued request automatically), and records the decision as a
// compact timestamped transcript line.
func (m *ShellModel) answerApproval(resp approvalResponse) {
	if len(m.approvals) == 0 {
		return
	}
	req := m.approvals[0].req
	req.RespCh <- resp // RespCh is buffered(1); never blocks
	m.approvals = m.approvals[1:]

	decision := "denied"
	switch {
	case resp.Approved && resp.AllowForSession:
		decision = "allowed for session"
	case resp.Approved:
		decision = "allowed"
	}
	m.recordApproval(req, decision, resp.Approved)
}

// drainApprovals denies every queued request. Called when the turn ends so no
// gate goroutine is left blocked on a RespCh nobody will ever answer.
func (m *ShellModel) drainApprovals() {
	for _, a := range m.approvals {
		a.req.RespCh <- approvalResponse{Approved: false}
		m.recordApproval(a.req, "denied (turn ended)", false)
	}
	m.approvals = nil
}

// recordApproval appends the decision to the session log and the transcript.
func (m *ShellModel) recordApproval(req PermissionRequestMsg, decision string, approved bool) {
	rec := approvalRecord{
		ts:       time.Now(),
		tool:     req.Request.ToolName,
		preview:  req.Request.Preview,
		decision: decision,
		approved: approved,
	}
	m.approvalLog = append(m.approvalLog, rec)
	m.appendLine(renderApprovalRecord(rec))
}

// settlePendingCalls flushes queued call texts as settled bullets. Used when
// the call/result pairing is interrupted (turn error or end) so announced
// calls aren't silently lost from the transcript.
func (m *ShellModel) settlePendingCalls() {
	for _, pc := range m.pendingCalls {
		if len(m.lines) > 0 {
			m.lines = append(m.lines, transcriptLine{})
		}
		m.lines = append(m.lines, transcriptLine{text: toolBulletStyle.Render("●") + " " + pc.text})
	}
	m.pendingCalls = nil
}

// ShellModel is the bubbletea model backing `fuse shell`: a scrollable
// transcript viewport above a single-line input prompt, driven by agent
// output arriving on a channel from a TeaRenderer.
type ShellModel struct {
	vp      viewport.Model
	input   textinput.Model
	spinner spinner.Model

	lines        []transcriptLine
	pendingCalls []pendingToolCall // FIFO of announced-but-unresolved tool calls
	alias        string
	verbose      bool
	running      bool
	ready        bool   // first WindowSizeMsg seen (viewport sized)
	glamourStyle string // fixed glamour style; detected before TUI starts

	runStart     time.Time
	turnFailed   bool // an AgentErrMsg arrived this turn; root node ends as error
	inputTokens  int
	outputTokens int

	ch          chan tea.Msg
	history     []model.Message
	approvals   []approvalState  // FIFO; head is the request awaiting y/s/n
	approvalLog []approvalRecord // answered requests, recallable via /approvals

	md        *glamour.TermRenderer // nil until first WindowSizeMsg; recreated on resize
	reg       *model.Registry
	slashReg  *SlashRegistry
	completer *slashCompleter
	build     AgentBuilder

	// sessionMode is the read handle on the session's active permission mode —
	// the single source shared with per-turn gate construction (Shift+Tab and
	// /mode mutate it in later tasks). The View indicator reads Get() each render.
	sessionMode *permissions.SessionMode
	// classifierAvailable is the degraded fact the shell learns ONCE at startup:
	// was an auto-mode classifier constructible? It is a plain flag threaded in,
	// never re-derived in the view. When auto is active but this is false, the
	// indicator marks the deterministic-only (fail-closed) posture.
	classifierAvailable bool

	// Subagent tree and inline summary tracking.
	tree          *agent.AgentTree
	blackboard    *agent.Blackboard // session blackboard for the /agents Blackboard tab
	agentsActive  bool
	agentsModel   *AgentsModel
	overlayGen    int // increments per overlay entry; stale overlay ticks are dropped
	inlineByLabel map[string]*inlineAgentState
	inlineByNode  map[string]*inlineAgentState
	inlineSeq     int // monotonic creation counter for inline blocks

	// rateGate is the session's shared rate-gate bucket, or nil when no rpm/tpm
	// axis is configured (change 0036). Read-only here: the observability surface
	// consults its live utilization for the status bar / agents overlay.
	rateGate *ratelimit.Bucket
}

// NewShellModel builds a ShellModel. alias is the starting model alias;
// verbose controls tool arg/output truncation; slashReg is the slash command
// registry; build constructs an agent bound to a renderer. glamourStyle is a
// fixed glamour style name ("dark", "light", etc.) detected before the TUI
// starts so glamour never queries the terminal from inside the bubbletea event
// loop. slashReg may be nil (all slash commands then return unknown).
//
// sessionMode is the session's active-permission-mode holder the status-line
// indicator reads live; classifierAvailable is the startup degraded fact (was
// an auto-mode classifier constructible?) used to mark the fail-closed posture
// when auto is active with no classifier.
func NewShellModel(alias string, verbose bool, glamourStyle string, reg *model.Registry, slashReg *SlashRegistry, build AgentBuilder, sessionMode *permissions.SessionMode, classifierAvailable bool) ShellModel {
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
		vp:                  vp,
		input:               in,
		spinner:             sp,
		alias:               alias,
		verbose:             verbose,
		glamourStyle:        glamourStyle,
		ch:                  make(chan tea.Msg, 64),
		reg:                 reg,
		slashReg:            slashReg,
		completer:           completer,
		build:               build,
		sessionMode:         sessionMode,
		classifierAvailable: classifierAvailable,
	}
	m.appendLine(banner.String(version.Version))
	m.appendLine(fmt.Sprintf("model: %s — /model NAME to switch, /verbose to toggle, /exit to quit", alias))
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

// WithBlackboard attaches the session blackboard so the /agents overlay can show
// its snapshot in a Blackboard tab (change 0023). Nil renders no snapshot.
func (m ShellModel) WithBlackboard(bb *agent.Blackboard) ShellModel {
	m.blackboard = bb
	return m
}

// WithRateGate attaches the session's shared rate-gate bucket so the
// observability surface (status bar / agents overlay) can show its live
// utilization (change 0036). A nil bucket is the unlimited fast path — the
// rate-gate segment simply renders nothing.
func (m ShellModel) WithRateGate(b *ratelimit.Bucket) ShellModel {
	m.rateGate = b
	return m
}

// Init focuses input and starts cursor blink. Event delivery needs no
// subscription commands: StartBridges pumps every session channel into the
// program via Program.Send.
func (m ShellModel) Init() tea.Cmd {
	return textinput.Blink
}

// agentOverlayTickMsg is a periodic tick used to refresh the agents overlay.
// gen ties the tick to one overlay session: stale ticks from a previous
// entry are dropped instead of re-armed, so Tab in/out cannot multiply chains.
type agentOverlayTickMsg struct{ gen int }

// agentOverlayTick fires once after 250ms, keeping the overlay live even when
// the agent emits no events (e.g. during LLM thinking between tool calls).
func agentOverlayTick(gen int) tea.Cmd {
	return tea.Tick(250*time.Millisecond, func(time.Time) tea.Msg { return agentOverlayTickMsg{gen: gen} })
}

// Update handles keys, window resize, and agent events. Messages arrive from
// bubbletea itself (keys, resize, ticks) and from StartBridges (agent events,
// tree updates, registry reloads) — there is no per-channel subscription
// state to maintain, so no case needs to "re-arm" anything.
func (m ShellModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Overlay-specific messages. Everything else falls through to the shared
	// switch below, so agent events keep updating the transcript (and timers
	// keep ticking) while the overlay is open.
	if m.agentsActive && m.agentsModel != nil {
		switch msg := msg.(type) {
		case agentsExitMsg:
			m.agentsActive = false
			m.agentsModel = nil
			m.refreshViewport(m.vp.AtBottom())
			return m, nil
		case agentOverlayTickMsg:
			// Periodic 250ms tick: re-read tree state so elapsed times update even
			// when the agent is quiet (thinking, no events emitted). Stale ticks
			// from a previous overlay session are dropped, not re-armed.
			if msg.gen != m.overlayGen {
				return m, nil
			}
			m.agentsModel.refreshSnapshot()
			return m, agentOverlayTick(m.overlayGen)
		case tea.KeyMsg:
			// A pending approval owns the keyboard regardless of the active view;
			// the queue lives in ShellModel so overlay exit can't orphan a RespCh.
			if len(m.approvals) > 0 {
				return m.handleApprovalKey(msg)
			}
			newModel, cmd := m.agentsModel.Update(msg)
			m.agentsModel = newModel.(*AgentsModel)
			return m, cmd
		case tea.MouseMsg:
			// Wheel scrolls the overlay's detail pane, not the hidden shell viewport.
			newModel, _ := m.agentsModel.Update(msg)
			m.agentsModel = newModel.(*AgentsModel)
			return m, nil
		}
	}

	switch msg := msg.(type) {
	case treeUpdateMsg:
		if m.tree == nil {
			return m, nil
		}
		if node := m.tree.Node(msg.nodeID); node != nil && node.Depth == 1 {
			m.updateInlineAgent(node)
		}
		// Keep the overlay's snapshot fresh too: a child that finishes while
		// the overlay is open must not leave its inline block "Running" forever.
		if m.agentsActive && m.agentsModel != nil {
			m.agentsModel.refreshSnapshot()
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
		if m.agentsModel != nil {
			m.agentsModel.width = msg.Width
			m.agentsModel.height = msg.Height
		}
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
		if len(m.pendingCalls) > 0 || m.hasRunningInline() {
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
		return m, nil

	case AssistantMsg:
		text := msg.Text
		if m.md != nil {
			if rendered, err := m.md.Render(text); err == nil {
				text = strings.TrimRight(rendered, "\n")
			}
		}
		m.appendPre(assistantStyle.Render(text))
		return m, nil

	case ToolCallMsg:
		// spawn_agent: insert a live inline block instead of the spinner line.
		if msg.Name == "spawn_agent" && m.tree != nil {
			var input struct {
				Label string `json:"label"`
			}
			_ = json.Unmarshal([]byte(msg.Args), &input)
			if input.Label != "" {
				if len(m.lines) > 0 {
					m.lines = append(m.lines, transcriptLine{})
				}
				block := &inlineAgentState{label: input.Label, lineIdx: len(m.lines), seq: m.inlineSeq}
				m.inlineSeq++
				if m.inlineByLabel == nil {
					m.inlineByLabel = make(map[string]*inlineAgentState)
				}
				m.inlineByLabel[input.Label] = block
				runLine := renderInlineRunning(input.Label, "0s", 0, 0)
				parts := strings.SplitN(runLine, "\n", 2)
				m.lines = append(m.lines, transcriptLine{text: parts[0]})
				if len(parts) > 1 {
					m.lines = append(m.lines, transcriptLine{text: parts[1]})
				} else {
					m.lines = append(m.lines, transcriptLine{})
				}
				m.refreshViewport(true)
				return m, nil
			}
		}
		args := sanitizeDisplay(msg.Args)
		if !m.verbose {
			args = truncate(args, previewLimit)
		}
		// Queue the call; its bullet renders together with its result so a
		// batch of N announced calls yields N separated call+result pairs.
		m.pendingCalls = append(m.pendingCalls, pendingToolCall{
			name: msg.Name,
			text: toolNameStyle.Render(msg.Name) + toolArgsStyle.Render("("+args+")"),
		})
		m.refreshViewport(true)
		return m, nil

	case ToolResultMsg:
		// Labelled spawn_agent calls render via the inline block, not the
		// queue — skip their results unless a queued spawn_agent entry exists
		// (the unlabelled fallback path does queue). One exception: a spawn
		// rejected by the budget/depth backstop never creates a tree node, so
		// no node event will ever settle its block — the error result is the
		// only signal, and it must flip the block out of "Running".
		if msg.Name == "spawn_agent" && m.tree != nil &&
			(len(m.pendingCalls) == 0 || m.pendingCalls[0].name != "spawn_agent") {
			if msg.IsError && m.settleRejectedSpawn(msg.Output) {
				m.refreshViewport(m.vp.AtBottom())
			}
			return m, nil
		}
		// Render this result's own call bullet, popped FIFO — results arrive
		// in call order, so the head is always the matching call.
		if len(m.pendingCalls) > 0 {
			pc := m.pendingCalls[0]
			m.pendingCalls = m.pendingCalls[1:]
			if len(m.lines) > 0 {
				m.lines = append(m.lines, transcriptLine{})
			}
			m.lines = append(m.lines, transcriptLine{text: toolBulletStyle.Render("●") + " " + pc.text})
		}
		out := msg.Output
		if !m.verbose {
			out = previewResult(out)
		}
		m.appendResultLines(out, msg.IsError, msg.Name)
		return m, nil

	case AgentErrMsg:
		// Settle any still-queued calls before showing the error; their
		// results may never arrive.
		m.settlePendingCalls()
		m.turnFailed = true
		m.appendLine(agentErrStyle.Render("! " + msg.Err))
		return m, nil

	case AgentDoneMsg:
		m.settlePendingCalls()
		// No block may outlive its turn as "Running": any still-unadopted
		// inline block has no node to settle it and never will.
		for m.settleRejectedSpawn("spawn did not complete") {
		}
		m.history = msg.History
		m.running = false
		m.drainApprovals()
		m.input.Focus()
		// Freeze the root node's clock — the loop is over; the agents view
		// must stop counting.
		if m.tree != nil {
			m.tree.EndTurn(m.turnFailed)
		}
		// Persist this turn's agent tree into the chat history: the live
		// inline blocks scroll away, but the run's shape shouldn't.
		if m.tree != nil {
			if summary := renderTreeSummary(m.tree, m.runStart); len(summary) > 0 {
				m.lines = append(m.lines, transcriptLine{text: subagentFooterStyle.Render(
					fmt.Sprintf("  ↳ %d subagent(s) this turn — Tab to inspect events", len(summary)),
				)})
				for _, s := range summary {
					m.lines = append(m.lines, transcriptLine{text: s})
				}
			}
		}
		m.refreshViewport(m.vp.AtBottom())
		return m, nil

	case PermissionRequestMsg:
		// Keep the call queued: after approval its execution continues and the
		// result will pop it. The spinner line below the block still shows
		// which call is waiting.
		m.enqueueApproval(msg)
		return m, nil

	case TokensMsg:
		m.inputTokens += msg.Input
		m.outputTokens += msg.Output
		return m, nil

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
	// While an approval is pending, intercept y/s/n/Esc before normal input.
	if len(m.approvals) > 0 {
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
	case tea.KeyShiftTab:
		// Cycle the session permission mode smart<->auto. The pending-approval
		// guard above already owns the keyboard first; the completer guard above
		// only consumes the keys handleCompleterKey handles (Up/Down/Esc/Enter),
		// so re-check completer state here to avoid flipping mode mid-completion.
		// The shell holds no long-lived root gate: mutating the shared SessionMode
		// holder IS the switch — the next per-turn gate is built reading it.
		if m.completer != nil && m.completer.active {
			return m, nil
		}
		if m.sessionMode == nil {
			return m, nil
		}
		next := permissions.ModeSmart
		switch m.sessionMode.Get() {
		case permissions.ModeSmart:
			next = permissions.ModeAuto
		case permissions.ModeAuto:
			next = permissions.ModeSmart
		default:
			// prompt-all / off land predictably on smart; the next Shift+Tab
			// then toggles smart<->auto.
			next = permissions.ModeSmart
		}
		m.sessionMode.Set(next)
		m.appendLine("mode: " + next.String())
		return m, nil
	case tea.KeyCtrlL:
		m.lines = nil
		m.pendingCalls = nil
		// Drop inline-block tracking with the lines it indexed into: stale
		// lineIdx values would let later ticks overwrite arbitrary new content.
		m.inlineByLabel = make(map[string]*inlineAgentState)
		m.inlineByNode = make(map[string]*inlineAgentState)
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

// handleApprovalKey handles y/s/n/Esc for the head of the approval queue.
// Note bubbletea names the escape key "esc", not "escape".
func (m ShellModel) handleApprovalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Type == tea.KeyCtrlC || msg.Type == tea.KeyCtrlD {
		return m, tea.Quit
	}
	switch strings.ToLower(msg.String()) {
	case "y":
		m.answerApproval(approvalResponse{Approved: true, AllowForSession: false})
	case "s":
		// "allow for session" is meaningless for a loop check (the session bool
		// is discarded), and the popup does not offer it there — ignore the key.
		if len(m.approvals) > 0 && isLoopApproval(m.approvals[0].req) {
			return m, nil
		}
		m.answerApproval(approvalResponse{Approved: true, AllowForSession: true})
	case "n", "esc":
		m.answerApproval(approvalResponse{Approved: false})
	default:
		// Ignore any other key while awaiting approval.
		return m, nil
	}
	return m, nil
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
	case "/approvals":
		if len(m.approvalLog) == 0 {
			m.appendLine(headerStyle.Render("No permission decisions this session."))
			return m, nil
		}
		m.appendLine(headerStyle.Render(fmt.Sprintf("Permission decisions (%d):", len(m.approvalLog))))
		for _, rec := range m.approvalLog {
			m.appendLine("  " + renderApprovalRecord(rec))
		}
		return m, nil
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
		// Keep the agents-tab tree root in sync so it renders the current model,
		// not the one snapshotted at construction.
		if m.tree != nil {
			m.tree.SetRootModel(name)
		}
		m.appendLine(fmt.Sprintf("switched to %s", name))
		return m, nil
	case "/mode":
		if len(fields) == 1 {
			// Bare /mode: name the current mode and list all options with a hint.
			cur := "(unknown)"
			if m.sessionMode != nil {
				cur = m.sessionMode.Get().String()
			}
			m.appendLine(fmt.Sprintf("mode: %s", cur))
			m.appendLine("options: smart, auto, prompt-all, off")
			m.appendLine("usage: /mode NAME")
			return m, nil
		}
		name := fields[1]
		// ParseMode defaults unknown tokens to smart, so validate by round-trip
		// rather than trusting it blindly.
		parsed := permissions.ParseMode(name)
		if parsed.String() != name {
			m.appendLine(fmt.Sprintf("unknown mode %q; usage: /mode NAME (smart/auto/prompt-all/off)", name))
			return m, nil
		}
		if m.sessionMode != nil {
			m.sessionMode.Set(parsed)
		}
		m.appendLine("mode: " + name)
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
	// Echo the user's prompt into the transcript — without this the submitted
	// text vanishes the moment the input clears.
	m.appendLine(promptAliasStyle.Render("> ") + sanitizeDisplay(line))
	m.history = append(m.history, model.Message{Role: "user", Content: line})
	m.running = true
	m.turnFailed = false
	m.runStart = time.Now()
	m.inputTokens = 0
	m.outputTokens = 0
	// Reset per-turn inline tracking so the footer counter starts fresh.
	m.inlineByLabel = make(map[string]*inlineAgentState)
	m.inlineByNode = make(map[string]*inlineAgentState)
	// Restart the root node's clock: its elapsed measures this turn.
	if m.tree != nil {
		m.tree.BeginTurn()
	}

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
	// run sends events onto the channel; the StartBridges pump delivers them.
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
		if n == nil {
			continue
		}
		if s := n.Snapshot().Status; s == agent.StatusRunning || s == agent.StatusPending {
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
		live := m.tree.Node(nodeID)
		if live == nil || block.lineIdx+1 >= len(m.lines) {
			continue
		}
		node := live.Snapshot()
		if node.Status != agent.StatusRunning && node.Status != agent.StatusPending {
			continue
		}
		elapsed := "0s"
		if !node.StartedAt.IsZero() {
			elapsed = fmt.Sprintf("%.0fs", time.Since(node.StartedAt).Seconds())
		}
		runLine := renderInlineRunning(node.Label, elapsed, node.TokensIn, node.TokensOut)
		parts := strings.SplitN(runLine, "\n", 2)
		m.lines[block.lineIdx] = transcriptLine{text: parts[0]}
		if len(parts) > 1 {
			m.lines[block.lineIdx+1] = transcriptLine{text: parts[1]}
		}
	}
}

// enterAgentsView switches to the AgentsModel overlay.
func (m ShellModel) enterAgentsView() (tea.Model, tea.Cmd) {
	if m.tree == nil {
		m.appendLine("no agent tree active")
		return m, nil
	}
	m.agentsModel = NewAgentsModel(m.tree, m.rateGate).WithBlackboard(m.blackboard)
	m.agentsModel.width = m.vp.Width
	m.agentsModel.height = m.vp.Height + chromeHeight
	m.agentsActive = true
	// Start the overlay tick for elapsed-time / low-event refreshes. Tree
	// updates arrive via the StartBridges pump; nothing to subscribe here.
	m.overlayGen++
	return m, agentOverlayTick(m.overlayGen)
}

// settleRejectedSpawn settles the oldest inline block that no tree node ever
// adopted, rendering it as an error in place. A spawn rejected by the
// budget/depth backstop fails before node creation, so the node lifecycle that
// normally settles blocks never runs for it. Results arrive in call order, so
// oldest-first matches error to block. Returns false when every block is
// node-owned (those settle via node events).
func (m *ShellModel) settleRejectedSpawn(errMsg string) bool {
	var victim *inlineAgentState
	for _, b := range m.inlineByLabel {
		if b.nodeID != "" {
			continue
		}
		if victim == nil || b.seq < victim.seq {
			victim = b
		}
	}
	if victim == nil || victim.lineIdx+1 >= len(m.lines) {
		return false
	}
	msg := sanitizeDisplay(truncate(firstLine(errMsg), 100))
	parts := strings.SplitN(renderInlineError("0s", 0, 0, msg), "\n", 2)
	m.lines[victim.lineIdx] = transcriptLine{text: parts[0]}
	if len(parts) > 1 {
		m.lines[victim.lineIdx+1] = transcriptLine{text: parts[1]}
	}
	delete(m.inlineByLabel, victim.label)
	return true
}

// updateInlineAgent refreshes the two-line inline block for a depth-1 child node.
// It reads only race-safe copies of node state (Snapshot + CopyEvents): the
// live node's fields are concurrently mutated by the child agent's goroutine.
func (m *ShellModel) updateInlineAgent(live *agent.AgentNode) {
	if m.inlineByLabel == nil {
		return
	}
	node := live.Snapshot()
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
		rendered = renderInlineRunning(node.Label, elapsed, node.TokensIn, node.TokensOut)
	case agent.StatusDone:
		result := ""
		for _, e := range live.CopyEvents() {
			if e.Kind == agent.KindDone {
				if r, ok2 := e.Payload["result"].(string); ok2 {
					result = r
				}
			}
		}
		rendered = renderInlineDone(elapsed, node.TokensIn, node.TokensOut, result)
	case agent.StatusError:
		errMsg := ""
		for _, e := range live.CopyEvents() {
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
	m.lines[block.lineIdx] = transcriptLine{text: parts[0]}
	if len(parts) > 1 {
		m.lines[block.lineIdx+1] = transcriptLine{text: parts[1]}
	}
	m.refreshViewport(m.vp.AtBottom())
}

// appendLine adds one logical line (which may itself contain newlines) and
// refreshes the viewport.
func (m *ShellModel) appendLine(s string) {
	atBottom := !m.ready || m.vp.AtBottom()
	for _, l := range strings.Split(s, "\n") {
		m.lines = append(m.lines, transcriptLine{text: l})
	}
	m.refreshViewport(atBottom)
}

// appendPre adds pre-wrapped content (glamour assistant output) one row per
// line with pre:true, so refreshViewport skips wordwrap and applies only the
// hard-wrap safety net — glamour's margins and indented blocks are preserved
// instead of being re-folded to column 0.
func (m *ShellModel) appendPre(s string) {
	atBottom := !m.ready || m.vp.AtBottom()
	for _, l := range strings.Split(s, "\n") {
		m.lines = append(m.lines, transcriptLine{text: l, pre: true})
	}
	m.refreshViewport(atBottom)
}

// appendResultLines renders a tool result indented under the previous bullet.
// For file-reading tools it adds a line-number gutter:
//
//	└ 1 │ package main
//	  2 │
//	  3 │ import …
//
// All other results use the plain prefix form:
//
//	└ first line
//	  subsequent lines…
func (m *ShellModel) appendResultLines(out string, isError bool, toolName string) {
	atBottom := !m.ready || m.vp.AtBottom()
	// Raw tool output can carry control bytes (binary content, ESC sequences)
	// that corrupt terminal state — strip before it touches the viewport.
	out = sanitizeDisplay(out)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	useGutter := !isError && isFileReadTool(toolName) && len(lines) > 1
	gutterW := len(fmt.Sprintf("%d", len(lines))) // digits needed for widest line number
	// Continuation prefix for gutter rows: the gutter rule with a blank number,
	// styled to match. Its printable width equals the first-row gutter prefix
	// ("  └ " / "    " + "N │ "), so content columns align across wrapped rows.
	gutterCont := gutterStyle.Render("    " + strings.Repeat(" ", gutterW) + " │ ")
	for i, l := range lines {
		var tl transcriptLine
		tl.text = l
		switch {
		case isError:
			tl.first = "  " + errorArrowStyle.Render("✗") + " "
			tl.cont = "    "
			tl.text = errorTextStyle.Render(l)
		case useGutter:
			g := gutterStyle.Render(fmt.Sprintf("%*d │ ", gutterW, i+1))
			if i == 0 {
				tl.first = resultPrefixStyle.Render("  └") + " " + g
			} else {
				tl.first = "    " + g
			}
			tl.cont = gutterCont
		default:
			if i == 0 {
				tl.first = resultPrefixStyle.Render("  └") + " "
			} else {
				tl.first = "    "
			}
			tl.cont = "    "
		}
		m.lines = append(m.lines, tl)
	}
	m.refreshViewport(atBottom)
}

// isFileReadTool returns true for tools whose output is file content and
// therefore benefits from a line-number gutter.
func isFileReadTool(name string) bool {
	lower := strings.ToLower(name)
	return strings.Contains(lower, "read") || lower == "view" || strings.Contains(lower, "file")
}

// agentsHint renders the status-bar agents segment: a live running/queued
// counter when children are active, so users see fan-out without opening the
// overlay; otherwise just the Tab affordance.
func (m ShellModel) agentsHint() string {
	if m.tree == nil {
		return ""
	}
	running, pending := m.tree.ActiveCounts()
	// Compact per-pool scheduler segment (change 0036): slots/queue/token spend for
	// the busiest engaged pool, appended before the Tab affordance so users see the
	// scheduler brakes without opening the overlay. Empty when nothing is engaged.
	sched := ""
	if seg := schedulerStatusSegment(m.tree.Scheduler().Snapshot()); seg != "" {
		sched = ruleStyle.Render(" · ") + statusRunStyle.Render(seg)
	}
	switch {
	case running > 0 && pending > 0:
		return "  " + statusRunStyle.Render(fmt.Sprintf("⚒ %d running · %d queued", running, pending)) +
			sched + ruleStyle.Render(" · Tab → inspect")
	case running > 0:
		return "  " + statusRunStyle.Render(fmt.Sprintf("⚒ %d agent(s) running", running)) +
			sched + ruleStyle.Render(" · Tab → inspect")
	case pending > 0:
		return "  " + statusRunStyle.Render(fmt.Sprintf("⚒ %d agent(s) queued", pending)) +
			sched + ruleStyle.Render(" · Tab → inspect")
	default:
		return "  " + ruleStyle.Render("Tab → agents")
	}
}

// approvalStatusText renders the status-bar label, including queue depth when
// more than one request is waiting.
func approvalStatusText(n int) string {
	if n > 1 {
		return fmt.Sprintf("Awaiting permission… (1 of %d)", n)
	}
	return "Awaiting permission…"
}

// isLoopApproval reports whether an approval request is a doom-loop
// force-through (tagged with the permissions sentinel ToolName) rather than a
// real tool call — the popup and the key handler render/handle it differently.
func isLoopApproval(req PermissionRequestMsg) bool {
	return req.Request.ToolName == permissions.LoopApprovalToolName
}

// overlayApprovalOnView paints the approval popup over the bottom rows of a
// full-screen view (the agents overlay). The queue is owned by ShellModel, so
// the popup follows the user across view switches instead of being dropped.
func overlayApprovalOnView(base string, req PermissionRequestMsg, queued, width int) string {
	if width < 8 {
		return base
	}
	loop := isLoopApproval(req)
	// A loop check is not a real tool call: its ToolName is the sentinel and its
	// preview already reads "possible loop: … — continue?", so show it as a loop
	// check and drop the "allow for session" option (its bool is discarded).
	header, field, keys := "⚠  Permission required", "  Tool:  ", "  [y] allow once   [s] allow for session   [n] deny"
	if loop {
		header, field, keys = "⚠  Possible loop", "  Loop:  ", "  [y] continue once   [n] abort"
	}
	label := req.Request.ToolName
	if loop {
		label = "detected"
	}
	if queued > 1 {
		label = fmt.Sprintf("%s   (1 of %d pending)", label, queued)
	}
	inner := approvalHeaderStyle.Render(header) + "\n\n" +
		field + label + "\n" +
		"  Cmd:   " + req.Request.Preview + "\n\n" +
		approvalKeysStyle.Render(keys)
	block := approvalBorderStyle.Width(width - 4).Render(inner)
	overlay := strings.Split(block, "\n")

	lines := strings.Split(base, "\n")
	start := len(lines) - len(overlay)
	if start < 0 {
		start = 0
	}
	for i, ol := range overlay {
		if start+i < len(lines) {
			lines[start+i] = fitLine(ol, width)
		}
	}
	return strings.Join(lines, "\n")
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

// hangWrap wraps a single transcriptLine to width with a hanging indent: the
// first visual row carries l.first, every continuation row carries l.cont (of
// equal printable width), so wrapped content stays inside the prefix column
// instead of escaping to column 0. It guarantees no emitted row exceeds the
// viewport width (constraint 1) — the hard-wrap pass is the safety net for long
// unbreakable tokens (URLs, minified JSON).
//
// Width math is printable-width aware: styled prefixes carry ANSI escapes that
// must not count toward the column budget (constraint 3).
func hangWrap(l transcriptLine, width int) []string {
	// contentW is the room left for text after the first-row prefix. Clamp to a
	// floor (matching buildEventViewLines) so pathological widths degrade to
	// narrow-but-correct instead of emitting empty rows.
	contentW := width - ansi.PrintableRuneWidth(l.first)
	if contentW < 8 {
		contentW = 8
	}

	if l.pre {
		// Glamour already wrapped at its render width; don't re-wordwrap (that
		// folds code blocks and blockquote indents to column 0). Apply only the
		// hard-wrap safety net so a shrink resize can't overflow the viewport.
		wrapped := wrap.String(l.text, width)
		return strings.Split(wrapped, "\n")
	}

	wrapped := wrap.String(wordwrap.String(l.text, contentW), contentW)
	rows := strings.Split(wrapped, "\n")
	out := make([]string, len(rows))
	for i, r := range rows {
		if i == 0 {
			out[i] = l.first + r
		} else {
			out[i] = l.cont + r
		}
	}
	return out
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
	// Settled transcript lines wrap per-line with a hanging indent from their
	// stored structure — re-flowed on every refresh and resize so continuations
	// stay in the gutter and no emitted row exceeds the viewport width.
	var rows []string
	if m.vp.Width > 0 {
		for _, l := range m.lines {
			rows = append(rows, hangWrap(l, m.vp.Width)...)
		}
	} else {
		for _, l := range m.lines {
			rows = append(rows, l.first+l.text)
		}
	}
	content := strings.Join(rows, "\n")

	// Transient rows the refresh composes itself (spinner + pending-call line,
	// completer overlay) are not stored transcriptLines; keep them on the flat
	// wordwrap+wrap path, appended after the wrapped settled block.
	var transient string
	if n := len(m.pendingCalls); n > 0 {
		line := m.spinner.View() + " " + m.pendingCalls[0].text
		if n > 1 {
			line += ruleStyle.Render(fmt.Sprintf("  (+%d queued)", n-1))
		}
		transient += "\n" + line
	}
	if m.completer != nil && m.completer.active {
		overlay := m.completer.View(m.vp.Width)
		if overlay != "" {
			transient += "\n" + overlay
		}
	}
	if transient != "" {
		if m.vp.Width > 0 {
			// wordwrap breaks at spaces; wrap hard-breaks anything still wider
			// than the viewport (long URLs, minified JSON) — overflowing lines
			// wrap in the terminal itself and desync the bottom-anchor math.
			transient = wrap.String(wordwrap.String(transient, m.vp.Width), m.vp.Width)
		}
		content += transient
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
// delegates to AgentsModel.View(), composing the pending approval popup on top
// (the queue lives here, not in the overlay, so it survives view switches).
func (m ShellModel) View() string {
	if m.agentsActive && m.agentsModel != nil {
		base := m.agentsModel.View()
		if len(m.approvals) > 0 {
			base = overlayApprovalOnView(base, m.approvals[0].req, len(m.approvals), m.agentsModel.width)
		}
		return base
	}
	width := m.vp.Width
	if width < 1 {
		width = 40
	}
	rule := ruleStyle.Render(strings.Repeat("─", width))
	prompt := promptAliasStyle.Render("["+m.alias+"]") + " > " + m.input.View()

	var status string
	switch {
	case len(m.approvals) > 0:
		status = approvalHeaderStyle.Render("⚠") + " " +
			statusRunStyle.Render(approvalStatusText(len(m.approvals))) + " " +
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
		status += m.agentsHint()
	default:
		status = statusModelStyle.Render(m.alias) + " " + statusModelStyle.Render(m.statusLine())
		status += m.agentsHint()
	}

	vpView := m.vp.View()
	// Pending approval renders as an overlay on the viewport — it disappears
	// the moment it is answered, leaving only the compact decision record.
	if len(m.approvals) > 0 {
		vpView = overlayApprovalOnView(vpView, m.approvals[0].req, len(m.approvals), width)
	}

	var b strings.Builder
	b.WriteString(vpView)
	b.WriteByte('\n')
	b.WriteString(rule)
	b.WriteByte('\n')
	b.WriteString(prompt)
	b.WriteByte('\n')
	b.WriteString(status)
	return b.String()
}

// statusLine produces the session-mode indicator fragment for the default
// status branch — the mode token plus, for auto-without-classifier, a static
// degraded marker. It reads the live session mode and the startup degraded flag
// so the indicator tracks a mid-session mode switch. Extracted so tests assert
// on this fragment rather than a full-screen View() snapshot.
func (m ShellModel) statusLine() string {
	mode := permissions.ModeSmart
	if m.sessionMode != nil {
		mode = m.sessionMode.Get()
	}
	return modeStatus(mode, m.classifierAvailable)
}

// modeStatus renders just the mode token (always the fixed
// PermissionMode.String() token, never a hand-written label) and, when mode is
// auto and no classifier is available, a static plain-ASCII degraded marker
// signalling the deterministic-only fail-closed posture. Pure and side-effect
// free so it is unit-testable in isolation.
func modeStatus(mode permissions.PermissionMode, classifierAvailable bool) string {
	s := "mode: " + mode.String()
	if mode == permissions.ModeAuto && !classifierAvailable {
		s += " (degraded - no classifier)"
	}
	return s
}
