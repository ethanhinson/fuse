package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/agent"
)

// routeInterruptInput handles /interrupt. It runs before the busy-queue so it works
// while a turn is running, which is the whole point. handled is false for every other
// line.
func (m ShellModel) routeInterruptInput(line string) (next tea.Model, cmd tea.Cmd, handled bool) {
	if rest, ok := stripPrefix(line, "/interrupt"); ok {
		m.resetInput()
		next, cmd = m.interruptCommand(rest)
		return next, cmd, true
	}
	return m, nil, false
}

func (m *ShellModel) resetInput() {
	m.input.Reset()
	if m.completer != nil {
		m.completer.deactivate()
	}
}

// appendSpacer puts one blank line before a block unless the transcript already ends in
// one.
func (m *ShellModel) appendSpacer() {
	if n := len(m.lines); n > 0 && strings.TrimSpace(m.lines[n-1].text) != "" {
		m.appendLine("")
	}
}

// interruptCommand steers the running agent right now: the message is queued with the
// interrupt flag, which cancels the agent's in-flight model call or tool run so the
// message lands at once instead of at the end of the current step. With nothing running
// it is an ordinary prompt.
func (m ShellModel) interruptCommand(text string) (tea.Model, tea.Cmd) {
	if text == "" {
		m.appendLine(headerStyle.Render("usage: /interrupt <message> - stop what the agent is doing and tell it this now"))
		return m, nil
	}
	if !m.running || m.humanBus == nil || m.tree == nil {
		return m.startPrompt(text)
	}
	m.humanBus.Steer(m.tree.RootID(), agent.SourceHuman, true, text)
	m.appendSpacer()
	m.appendLine(humanMsgStyle.Render("⚡ interrupt") + " " + sanitizeDisplay(text))
	return m, nil
}
