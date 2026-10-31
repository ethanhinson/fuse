package tui

import (
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
)

// The agent owns token accounting (Agent.SetUsageSink, change 0090); the renderer
// must not also count, or a TUI run would double-count every model call.
func TestNodeRendererDoesNotCountTokens(t *testing.T) {
	tree := agent.NewAgentTree("root", "m")
	node := tree.Node(tree.RootID())
	r := NewNodeRenderer(node, tree)
	r.Tokens(600, 400)
	if got := tree.SessionTokens(); got != 0 {
		t.Fatalf("SessionTokens = %d after renderer.Tokens, want 0 (agent sink owns counting)", got)
	}
	node.UpdateTokens(600, 400) // what the agent's usage sink does
	r.Tokens(600, 400)
	if got := tree.SessionTokens(); got != 1000 {
		t.Fatalf("SessionTokens = %d, want 1000 (counted once)", got)
	}
}
