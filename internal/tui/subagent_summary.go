package tui

import (
	"fmt"

	"github.com/ethanhinson/fuse/internal/agent"
)

// inlineAgentState tracks the inline summary for a depth-1 child agent.
type inlineAgentState struct {
	nodeID  string
	lineIdx int // index into ShellModel.lines where the block starts
	label   string
	status  agent.NodeStatus
	result  string
}

// renderInlineRunning returns the two-line "running" block for a depth-1 child.
func renderInlineRunning(label, elapsed string, tokIn, tokOut int) string {
	return fmt.Sprintf("▶ spawn_agent(%q)\n  ● Running · %s · ↑%s ↓%s tokens",
		label, elapsed, formatTokens(tokIn), formatTokens(tokOut))
}

// renderInlineDone returns the two-line "done" block for a depth-1 child.
func renderInlineDone(elapsed string, tokIn, tokOut int, result string) string {
	summary := result
	if len(summary) > 80 {
		summary = summary[:80] + "…"
	}
	return fmt.Sprintf("✓ spawn_agent · %s · ↑%s ↓%s tokens\n  %s",
		elapsed, formatTokens(tokIn), formatTokens(tokOut), summary)
}

// renderInlineError returns the two-line "error" block for a depth-1 child.
func renderInlineError(elapsed string, tokIn, tokOut int, errMsg string) string {
	return fmt.Sprintf("✕ spawn_agent · %s · ↑%s ↓%s tokens\n  error: %s",
		elapsed, formatTokens(tokIn), formatTokens(tokOut), errMsg)
}

// renderInlineRemoteRunning is the running block with a ☁ prefix for remote agents.
func renderInlineRemoteRunning(label, elapsed string, tokIn, tokOut int) string {
	return fmt.Sprintf("▶ spawn_agent(%q) ☁\n  ● Running · %s · ↑%s ↓%s tokens",
		label, elapsed, formatTokens(tokIn), formatTokens(tokOut))
}
