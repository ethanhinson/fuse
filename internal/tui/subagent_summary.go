package tui

import (
	"fmt"
	"strings"
	"time"

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
		sanitizeDisplay(label), elapsed, formatTokens(tokIn), formatTokens(tokOut))
}

// renderInlineDone returns the two-line "done" block for a depth-1 child.
func renderInlineDone(elapsed string, tokIn, tokOut int, result string) string {
	// firstLine + rune-safe truncate: the block must stay exactly two lines,
	// and byte slicing mid-rune would render mojibake. Sanitized: child
	// results can carry control bytes that corrupt the terminal.
	summary := sanitizeDisplay(truncate(firstLine(result), 80))
	return fmt.Sprintf("✓ spawn_agent · %s · ↑%s ↓%s tokens\n  %s",
		elapsed, formatTokens(tokIn), formatTokens(tokOut), summary)
}

// renderInlineError returns the two-line "error" block for a depth-1 child.
func renderInlineError(elapsed string, tokIn, tokOut int, errMsg string) string {
	return fmt.Sprintf("✕ spawn_agent · %s · ↑%s ↓%s tokens\n  error: %s",
		elapsed, formatTokens(tokIn), formatTokens(tokOut), errMsg)
}

// renderTreeSummary renders a compact tree of this turn's child agents for
// the transcript, so the run's shape survives in chat history after the
// live inline blocks scroll away. Nodes from earlier turns (started before
// `since`) are excluded.
func renderTreeSummary(tree *agent.AgentTree, since time.Time) []string {
	var nodes []agent.NodeView
	for _, v := range tree.SnapshotAll() {
		if v.Depth == 0 {
			continue
		}
		// This turn's nodes: started during it, or never started (queued/
		// cancelled before getting a slot).
		if v.StartedAt.IsZero() || !v.StartedAt.Before(since) {
			nodes = append(nodes, v)
		}
	}
	if len(nodes) == 0 {
		return nil
	}

	// Last-child lookup for └─ vs ├─ edges (DFS order groups siblings).
	lastChild := make(map[string]string, len(nodes))
	for _, n := range nodes {
		lastChild[n.ParentID] = n.ID
	}

	minDepth := nodes[0].Depth
	for _, n := range nodes {
		if n.Depth < minDepth {
			minDepth = n.Depth
		}
	}

	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		branch := "├─"
		if lastChild[n.ParentID] == n.ID {
			branch = "└─"
		}
		indent := strings.Repeat("   ", n.Depth-minDepth)
		meta := nodeElapsed(n)
		if n.TokensIn > 0 || n.TokensOut > 0 {
			meta += " ↑" + formatTokens(n.TokensIn) + " ↓" + formatTokens(n.TokensOut)
		}
		out = append(out, "  "+gutterStyle.Render(indent+branch)+" "+
			glyphStyle(n.Status).Render(glyphForStatus(n.Status))+" "+
			sanitizeDisplay(truncate(n.Label, 48))+" "+
			gutterStyle.Render(meta))
	}
	return out
}
