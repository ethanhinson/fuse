package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// screen.go: the shared full-screen frame behind every modal surface the shell
// opens (/config, /queue, /models edit). A screen is sticky - it replaces the
// whole terminal, input line and status bar included, until the host closes it
// with Esc - so these surfaces read as places you go to, not popups painted
// over the transcript.
//
// The frame owns layout only: a title row, a rule, the body (clipped or padded
// to the space left), a rule, and a one-line key hint pinned to the bottom row.
// What the body shows and which keys it answers stays with the host.
//
// Width invariant (change 0041): every emitted line is clamped to width cells.

// screenChromeRows is what the frame spends outside the body: title, top rule,
// bottom rule, hint.
const screenChromeRows = 4

// screenBodyHeight reports how many body rows a screen of the given total
// height leaves for its content.
func screenBodyHeight(height int) int {
	if h := height - screenChromeRows; h > 1 {
		return h
	}
	return 1
}

// renderScreenFrame composes a full-screen view exactly width x height cells.
// title and hint are pre-styled single lines; body is clipped to the rows the
// frame leaves and padded with blanks so the hint always sits on the last row.
func renderScreenFrame(title string, body []string, hint string, width, height int) string {
	if width < 1 || height < 1 {
		return ""
	}
	rule := lipgloss.NewStyle().Foreground(colMuted).Render(strings.Repeat("─", width))
	bodyH := screenBodyHeight(height)

	lines := make([]string, 0, height)
	lines = append(lines, fitTableLine(" "+title, width), rule)
	for i := 0; i < bodyH; i++ {
		if i < len(body) {
			lines = append(lines, fitTableLine(body[i], width))
		} else {
			lines = append(lines, "")
		}
	}
	lines = append(lines, rule, fitTableLine(" "+hint, width))
	if len(lines) > height {
		lines = lines[:height]
	}
	return strings.Join(lines, "\n")
}

// screenOpen reports whether a full-screen surface owns the display.
func (m ShellModel) screenOpen() bool {
	return m.queue != nil || m.config != nil || m.modelsEdit != nil
}

// screenView renders the open full-screen surface at the terminal size.
func (m ShellModel) screenView() string {
	width := m.vp.Width
	if width < 1 {
		width = 40
	}
	height := m.vp.Height + chromeHeight
	switch {
	case m.queue != nil:
		return queueScreen(m.queue, width, height)
	case m.config != nil:
		m.config.active = m.alias
		return configScreen(m.config, width, height)
	default:
		return modelsEditorScreen(m.modelsEdit, m.alias, width, height)
	}
}
