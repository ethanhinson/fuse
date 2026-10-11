package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/agent"
)

// orchestrator.go is the shell's side of the steering side-channel: the
// /orchestrator and /interrupt commands. The orchestrator itself (a chat loop with
// its own model and tools) lives behind OrchestratorSession; the shell only routes
// the person's words to it and renders what it says back.

const (
	defaultPlaceholder = "type a task, /model NAME, /verbose, /exit"
	orchPlaceholder    = "ask the orchestrator, or tell it what to steer - /orchestrator off to go back"
)

// OrchestratorSession is the shell's handle on the orchestrator chat. Say hands it one
// message from the person and returns at once: the orchestrator answers on its own
// goroutine, by sending OrchReplyMsg / OrchActionMsg / OrchErrMsg into the program.
// The session starts itself on the first Say.
type OrchestratorSession interface {
	Say(text string)
}

// OrchReplyMsg is the orchestrator speaking to the person.
type OrchReplyMsg struct{ Text string }

// OrchActionMsg reports something the orchestrator did to the working agent, such as
// steering it.
type OrchActionMsg struct{ Text string }

// OrchErrMsg is a problem in the orchestrator itself.
type OrchErrMsg struct{ Err string }

// WithOrchestrator attaches the orchestrator session. Nil leaves /orchestrator reporting
// that it is unavailable.
func (m ShellModel) WithOrchestrator(o OrchestratorSession) ShellModel {
	m.orch = o
	return m
}

// routeOrchestratorInput handles the lines that talk to the orchestrator or steer the
// agent directly. It runs before the busy-queue so these work while a turn is running,
// which is the whole point. handled is false for every other line.
func (m ShellModel) routeOrchestratorInput(line string) (next tea.Model, cmd tea.Cmd, handled bool) {
	if rest, ok := stripPrefix(line, "/orchestrator"); ok {
		m.resetInput()
		next, cmd = m.orchestratorCommand(rest)
		return next, cmd, true
	}
	if rest, ok := stripPrefix(line, "/interrupt"); ok {
		m.resetInput()
		next, cmd = m.interruptCommand(rest)
		return next, cmd, true
	}
	// Sticky mode: plain prose goes to the orchestrator. Slash commands keep working so
	// the person can still /exit, /agents, and so on.
	if m.orchMode && m.orch != nil && !strings.HasPrefix(line, "/") {
		m.resetInput()
		m.sayToOrchestrator(line)
		return m, nil, true
	}
	return m, nil, false
}

func (m *ShellModel) resetInput() {
	m.input.Reset()
	if m.completer != nil {
		m.completer.deactivate()
	}
}

func (m *ShellModel) sayToOrchestrator(text string) {
	m.appendLine(orchStyle.Render("you → orchestrator") + " " + sanitizeDisplay(text))
	m.orch.Say(text)
}

func (m ShellModel) orchestratorCommand(arg string) (tea.Model, tea.Cmd) {
	if m.orch == nil {
		m.appendLine(headerStyle.Render("orchestrator is not available in this session"))
		return m, nil
	}
	switch strings.ToLower(arg) {
	case "":
		m.orchMode = true
		m.input.Placeholder = orchPlaceholder
		m.appendLine(orchStyle.Render("orchestrator mode on") +
			headerStyle.Render(" - your messages go to the orchestrator, which can watch and steer the agent. /orchestrator off to go back."))
	case "off":
		m.orchMode = false
		m.input.Placeholder = defaultPlaceholder
		m.appendLine(headerStyle.Render("orchestrator mode off - your messages go to the agent again"))
	default:
		m.sayToOrchestrator(arg)
	}
	return m, nil
}

// interruptCommand steers the running agent right now: the message is queued with the
// interrupt flag, which cancels the agent's in-flight model call or tool run so the
// message lands at once instead of at the end of the current step. With nothing running
// it is an ordinary prompt.
func (m ShellModel) interruptCommand(text string) (tea.Model, tea.Cmd) {
	if text == "" {
		m.appendLine(headerStyle.Render("usage: /interrupt <message> - stop what the agent is doing and tell it this"))
		return m, nil
	}
	if !m.running || m.humanBus == nil || m.tree == nil {
		return m.startPrompt(text)
	}
	m.humanBus.Steer(m.tree.RootID(), agent.SourceHuman, true, text)
	m.appendLine(humanMsgStyle.Render("⚡ interrupt") + " " + sanitizeDisplay(text))
	return m, nil
}
