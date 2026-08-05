package tui

import "github.com/charmbracelet/lipgloss"

// Dark-theme palette matching the Claude Code aesthetic.
var (
	colCyan   = lipgloss.Color("#56b6c2")
	colGreen  = lipgloss.Color("#98c379")
	colRed    = lipgloss.Color("#e06c75")
	colAmber  = lipgloss.Color("#e5c07b")
	colMuted  = lipgloss.Color("#5c6370")
	colNormal = lipgloss.Color("#abb2bf")

	// Status bar
	statusModelStyle = lipgloss.NewStyle().Bold(true).Foreground(colNormal)
	statusRunStyle   = lipgloss.NewStyle().Foreground(colAmber)
	spinnerStyle     = lipgloss.NewStyle().Foreground(colAmber)

	// Chrome
	ruleStyle        = lipgloss.NewStyle().Foreground(colMuted)
	promptAliasStyle = lipgloss.NewStyle().Foreground(colCyan)

	// Transcript — model turn header
	headerStyle = lipgloss.NewStyle().Foreground(colMuted)

	// Transcript — tool calls  (● name(args))
	toolBulletStyle = lipgloss.NewStyle().Foreground(colGreen)
	toolNameStyle   = lipgloss.NewStyle().Foreground(colCyan)
	toolArgsStyle   = lipgloss.NewStyle().Foreground(colMuted)

	// Transcript — tool results (  └ …)
	resultPrefixStyle = lipgloss.NewStyle().Foreground(colMuted)
	resultArrowStyle  = lipgloss.NewStyle().Foreground(colMuted) // kept for compat
	errorArrowStyle   = lipgloss.NewStyle().Foreground(colRed)
	errorTextStyle    = lipgloss.NewStyle().Foreground(colRed)

	// Transcript — agent error lines (! prefix)
	agentErrStyle = lipgloss.NewStyle().Foreground(colRed)

	// Transcript — assistant prose
	assistantStyle = lipgloss.NewStyle().Foreground(colNormal)

	// Transcript — file-content gutter (line numbers + │ bar)
	gutterStyle = lipgloss.NewStyle().Foreground(colMuted)

	// Transcript — subagent batch footer (shown after AgentDone when agents ran)
	subagentFooterStyle = lipgloss.NewStyle().Foreground(colMuted).Italic(true)

	// Permission approval block
	approvalBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(colAmber).
				Padding(0, 1).
				MarginTop(1)
	approvalHeaderStyle = lipgloss.NewStyle().Foreground(colAmber).Bold(true)
	approvalKeysStyle   = lipgloss.NewStyle().Foreground(colMuted)
)
