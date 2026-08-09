package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/permissions"
)

// shellWithHumanMessaging builds a sized ShellModel wired with a tree, a live
// child node + handle, and the human bus/registry — the fixture the routing
// integration tests drive.
func shellWithHumanMessaging(t *testing.T) (ShellModel, *agent.HumanBus, *agent.AgentTree) {
	t.Helper()
	tree := agent.NewAgentTree("root", "test")
	reg := agent.NewHandleRegistry()
	reg.Register(tree.RootID(), "root")
	child := agent.NewAgentNodeForTest("coder-1", tree.RootID(), "coder")
	tree.AddNodeForTest(child)
	reg.Register("coder-1", "coder")
	bus := agent.NewHumanBus(tree)
	bb := agent.NewBlackboard(tree)

	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder, permissions.NewSessionMode(permissions.ModeSmart), true))
	m = m.WithTree(tree).WithBlackboard(bb).WithHumanMessaging(bus, reg, nil)
	m.running = true // an agent is busy, so bare prose queues
	return m, bus, tree
}

// typeAndEnter feeds a line rune-by-rune then Enter through the real key handler.
func typeAndEnter(m ShellModel, line string) ShellModel {
	for _, r := range line {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(ShellModel)
	}
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	return next.(ShellModel)
}

func TestShellRoute_DirectEnqueues(t *testing.T) {
	m, bus, _ := shellWithHumanMessaging(t)
	m = typeAndEnter(m, "@coder use the streaming approach")

	p := bus.Pending("coder-1")
	if len(p) != 1 || p[0].Text != "use the streaming approach" || p[0].Mode != agent.ModeDirect {
		t.Fatalf("@coder did not enqueue a direct message: %+v", p)
	}
	if !strings.Contains(plainLines(m), "use the streaming approach") {
		t.Error("transcript should echo the routed message")
	}
}

func TestShellRoute_BroadcastEnqueuesAll(t *testing.T) {
	m, bus, tree := shellWithHumanMessaging(t)
	m = typeAndEnter(m, "@all pause and review")

	for _, id := range []string{tree.RootID(), "coder-1"} {
		if p := bus.Pending(id); len(p) != 1 || p[0].Mode != agent.ModeBroadcast {
			t.Errorf("node %s did not receive broadcast: %+v", id, p)
		}
	}
	_ = m
}

func TestShellRoute_QueuedDefaultsToRoot(t *testing.T) {
	m, bus, tree := shellWithHumanMessaging(t)
	m = typeAndEnter(m, "also add error handling")

	p := bus.Pending(tree.RootID())
	if len(p) != 1 || p[0].Mode != agent.ModeQueued {
		t.Fatalf("bare prose while busy should queue to root: %+v", p)
	}
	if !strings.Contains(plainLines(m), "queued") {
		t.Error("transcript should note the message was queued")
	}
}

func TestShellRoute_BtwIsReadOnly(t *testing.T) {
	m, bus, tree := shellWithHumanMessaging(t)
	m = typeAndEnter(m, "/btw how many running")

	// Aside must NOT enqueue anything anywhere.
	if len(bus.Pending(tree.RootID())) != 0 || len(bus.Pending("coder-1")) != 0 {
		t.Error("/btw must not deliver a message to any node")
	}
	out := plainLines(m)
	if !strings.Contains(out, "/btw") || !strings.Contains(out, "running") {
		t.Errorf("/btw answer missing from transcript: %q", out)
	}
}

func TestShellRoute_Rename(t *testing.T) {
	m, _, _ := shellWithHumanMessaging(t)
	m = typeAndEnter(m, "/rename @coder @scout")
	if !strings.Contains(plainLines(m), "renamed @coder → @scout") {
		t.Errorf("rename not reflected in transcript: %q", plainLines(m))
	}
	// A subsequent @scout message must resolve.
	m = typeAndEnter(m, "@scout keep going")
	if !strings.Contains(plainLines(m), "keep going") {
		t.Error("renamed handle should route")
	}
}
