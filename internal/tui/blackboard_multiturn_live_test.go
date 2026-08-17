package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"
	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/tools"
)

// TestLiveBlackboardAcrossTurns closes the gap the fix's original report named:
// the BLACKBOARD view across a turn boundary. Turn 1 writes plan/turn-one, turn 2
// writes plan/turn-two; the board must show BOTH entries with each attributed to
// its own turn's divider (never the previous, settled turn) — the same
// turn-boundary-skew fix the detail pane needed, exercised on the board surface.
func TestLiveBlackboardAcrossTurns(t *testing.T) {
	if testing.Short() {
		t.Skip("slow live e2e")
	}
	gw := newScriptedGateway(t, []scriptedGatewayTurn{
		{toolName: "blackboard_write", toolArgs: `{"key":"plan/turn-one","value":"{\"step\":1}"}`, reply: "turn-one-complete"},
		{toolName: "blackboard_write", toolArgs: `{"key":"plan/turn-two","value":"{\"step\":2}"}`, reply: "turn-two-complete"},
	}, 300*time.Millisecond)

	tree := agent.NewAgentTree("kimi", "test/scripted")
	root := tree.Node(tree.RootID())
	bb := agent.NewBlackboard(tree)
	reg := tools.NewRegistry()
	for _, tl := range tools.NewBlackboardTools(bb.ForNode(root)) {
		reg.Register(tl)
	}
	build := func(_ string, r agent.Renderer, _ permissions.ApprovalFunc) (*agent.Agent, error) {
		return agent.New(model.NewAdapter(gw.URL, "tkn", gw.Client()), reg, r, "test/scripted", "", 8, 256), nil
	}
	m := NewShellModel("kimi", false, "", testRegistry(), nil, build,
		permissions.NewSessionMode(permissions.ModeOff), true).
		WithTree(tree).WithBlackboard(bb)

	bridgeCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(120, 34))
	StartBridges(bridgeCtx, tm.GetProgram(), m.Channel(), nil, nil)
	ls := newLiveStream(tm)
	t.Cleanup(func() { ls.close(); tm.Quit() }) //nolint:errcheck

	send := func(s string) {
		for _, r := range s {
			tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		}
	}
	prompt := func(s, done string) {
		send(s)
		tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
		ls.waitFor(t, done, 30*time.Second)
	}

	prompt("record the first plan step", "turn-one-complete")
	prompt("record the second plan step", "turn-two-complete")

	if _, ok := bb.Get("plan/turn-one"); !ok {
		t.Fatal("turn 1 write never reached the store")
	}
	if _, ok := bb.Get("plan/turn-two"); !ok {
		t.Fatal("turn 2 write never reached the store")
	}
	if got := len(root.Snapshot().Turns); got != 2 {
		t.Fatalf("want 2 turn marks, got %d", got)
	}

	// Open the board (Tab into overlay, then 'b').
	tm.Send(tea.KeyMsg{Type: tea.KeyTab})
	ls.forceRepaint(tm)
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	ls.waitFor(t, "plan/turn-one", 10*time.Second)
	ls.forceRepaint(tm)
	frame := ls.text()

	// Both entries must render — neither swallowed by the previous turn.
	for _, want := range []string{"plan/turn-one", "plan/turn-two"} {
		if !strings.Contains(frame, want) {
			t.Errorf("board missing %q — the empty/mis-bucketed-board defect:\n%s", want, tailLines(frame, 40))
		}
	}
	// Turn-aware board must show dividers for BOTH turns and no negative offset.
	for _, want := range []string{"── turn 1 ──", "── turn 2 ──"} {
		if !strings.Contains(frame, want) {
			t.Errorf("board missing divider %q:\n%s", want, tailLines(frame, 40))
		}
	}
	if hits := negativeOffsetRE.FindAllString(frame, -1); len(hits) > 0 {
		t.Errorf("negative offsets on the board: %v", hits)
	}

	// Direct attribution check on the store: each write buckets to its own turn.
	rv := root.Snapshot()
	e1, _ := bb.Get("plan/turn-one")
	e2, _ := bb.Get("plan/turn-two")
	if got := turnIndexFor(rv, e1.WrittenAt); got != 0 {
		t.Errorf("plan/turn-one attributed to turn index %d, want 0 (turn 1)", got)
	}
	if got := turnIndexFor(rv, e2.WrittenAt); got != 1 {
		t.Errorf("plan/turn-two attributed to turn index %d, want 1 (turn 2)", got)
	}
}
