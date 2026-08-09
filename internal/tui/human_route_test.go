package tui

import (
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
)

func routeFixture(t *testing.T) (*agent.AgentTree, *agent.HandleRegistry, *agent.Blackboard) {
	t.Helper()
	tree := agent.NewAgentTree("root", "test")
	reg := agent.NewHandleRegistry()
	// Register the root and one child handle.
	reg.Register(tree.RootID(), "root")
	child := agent.NewAgentNodeForTest("coder-1", tree.RootID(), "coder")
	tree.AddNodeForTest(child)
	reg.Register("coder-1", "coder")
	bb := agent.NewBlackboard(tree)
	return tree, reg, bb
}

func TestClassify_Rungs(t *testing.T) {
	tree, reg, bb := routeFixture(t)
	sel := ""

	// /btw → aside with a harness answer.
	r := classifyInput("/btw how many running", tree, reg, bb, true, false, sel)
	if r.Kind != routeAside || r.AsideAnswer == "" {
		t.Errorf("/btw not routed to aside: %+v", r)
	}

	// @all → broadcast.
	r = classifyInput("@all pause and review", tree, reg, bb, true, false, sel)
	if r.Kind != routeBroadcast || r.Text != "pause and review" {
		t.Errorf("@all not routed to broadcast: %+v", r)
	}

	// @coder → direct, resolved to the node ID.
	r = classifyInput("@coder use streaming", tree, reg, bb, true, false, sel)
	if r.Kind != routeDirect || r.Target != "coder-1" || r.Text != "use streaming" {
		t.Errorf("@coder not routed to direct: %+v", r)
	}

	// @ghost → direct but unresolved, falls back to selected.
	r = classifyInput("@ghost hi", tree, reg, bb, true, false, "coder-1")
	if r.Kind != routeDirect || !r.Unresolved || r.Target != "coder-1" {
		t.Errorf("@ghost should be unresolved+fallback: %+v", r)
	}

	// pending ask + free text → respond.
	r = classifyInput("option two, actually", tree, reg, bb, true, true, sel)
	if r.Kind != routeRespond {
		t.Errorf("free text with pending ask should respond: %+v", r)
	}

	// bare prose while busy → queued (default target = root when no selection).
	r = classifyInput("also handle errors", tree, reg, bb, true, false, sel)
	if r.Kind != routeQueued || r.Target != tree.RootID() {
		t.Errorf("bare prose while busy should queue to root: %+v", r)
	}

	// bare prose while idle → normal prompt.
	r = classifyInput("new task", tree, reg, bb, false, false, sel)
	if r.Kind != routeNormal {
		t.Errorf("bare prose while idle should be normal: %+v", r)
	}

	// /rename → rename admin.
	r = classifyInput("/rename @coder @scout", tree, reg, bb, true, false, sel)
	if r.Kind != routeRename || r.Handle != "@coder" || r.NewHandle != "@scout" {
		t.Errorf("/rename not routed: %+v", r)
	}
}

func TestClassify_NilStateIsNormal(t *testing.T) {
	r := classifyInput("hello", nil, nil, nil, false, false, "")
	if r.Kind != routeNormal {
		t.Errorf("nil state should be normal, got %+v", r)
	}
}
