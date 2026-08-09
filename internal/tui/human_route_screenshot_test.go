package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/permissions"
)

// screenshotShell builds a sized, human-messaging-enabled shell with a small
// live tree (root + two children) for the Phase-D screenshots.
func screenshotShell(t *testing.T) (ShellModel, *agent.HumanBus, *agent.AgentTree) {
	t.Helper()
	tree := agent.NewAgentTree("alpha", "test")
	reg := agent.NewHandleRegistry()
	reg.Register(tree.RootID(), "alpha")
	coder := agent.NewAgentNodeForTest("coder-1", tree.RootID(), "coder")
	coder.AddEvent(agent.AgentEvent{Kind: agent.KindToolCall, Name: "edit"})
	tree.AddNodeForTest(coder)
	reg.Register("coder-1", "coder")
	rev := agent.NewAgentNodeForTest("reviewer-1", tree.RootID(), "reviewer")
	tree.AddNodeForTest(rev)
	reg.Register("reviewer-1", "reviewer")
	bb := agent.NewBlackboard(tree)
	bb.Put("plan/api", "routes", "coder-1", "coder")

	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder, permissions.NewSessionMode(permissions.ModeSmart), true))
	m = m.WithTree(tree).WithBlackboard(bb).WithHumanMessaging(bus(tree), reg, nil)
	return m, m.humanBus, tree
}

func bus(tree *agent.AgentTree) *agent.HumanBus { return agent.NewHumanBus(tree) }

// TestScreenshot_HumanRouteTranscript captures a transcript showing @direct,
// @all, /btw, and a queued message all in one frame.
func TestScreenshot_HumanRouteTranscript(t *testing.T) {
	m, _, _ := screenshotShell(t)
	m.running = true // so bare prose queues

	for _, line := range []string{
		"@coder use the streaming approach",
		"@all keep changes minimal",
		"/btw what is @coder doing",
		"add integration tests too",
	} {
		m = typeAndEnter(m, line)
	}
	frame := captureModelFrame(t, m, "human-route-transcript")
	for _, want := range []string{"@coder", "@all", "/btw", "queued"} {
		if !strings.Contains(frame, want) {
			t.Errorf("transcript frame missing %q", want)
		}
	}
}

// TestScreenshot_QueueEditor captures the /queue editor with several pending
// messages and the cursor mid-list.
func TestScreenshot_QueueEditor(t *testing.T) {
	m, b, tree := screenshotShell(t)
	root := tree.RootID()
	b.Enqueue(root, agent.ModeQueued, "@alpha", "also add error handling")
	b.Enqueue("coder-1", agent.ModeDirect, "@coder", "use the streaming approach")
	b.Enqueue(root, agent.ModeBroadcast, "@all", "keep changes minimal")
	m = typeLine(m, "/queue")
	m, _ = enter(m)
	// Move the cursor down one for a mid-list highlight.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = next.(ShellModel)

	frame := captureModelFrame(t, m, "human-queue-editor")
	if !strings.Contains(frame, "Message queue") || !strings.Contains(frame, "reorder") {
		t.Errorf("queue editor frame incomplete:\n%s", frame)
	}
}

// TestScreenshot_BtwAside captures a /btw status answer rendered inline.
func TestScreenshot_BtwAside(t *testing.T) {
	m, _, _ := screenshotShell(t)
	m = typeAndEnter(m, "/btw show tree")
	m = typeAndEnter(m, "/btw what is @coder doing")
	frame := captureModelFrame(t, m, "human-btw-aside")
	if !strings.Contains(frame, "coder") {
		t.Errorf("btw frame missing tree/answer:\n%s", frame)
	}
}
