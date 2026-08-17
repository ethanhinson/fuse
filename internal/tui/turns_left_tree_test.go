package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/ethanhinson/fuse/internal/agent"
)

// twoTurnSpawnedTree builds a root with two turns: turn 1 spawns Strategist +
// Critic, turn 2 spawns Advocate. Returns the tree and an AgentsModel primed with
// a snapshot. Shared by the left-tree tests.
func twoTurnSpawnedTree(t *testing.T) (*agent.AgentTree, *AgentsModel) {
	t.Helper()
	tree := agent.NewAgentTreeWithConcurrency("kimi", "glm", 16)
	rootNode := tree.Node(tree.RootID())
	sp := agent.NewSpawner(agent.WithTree(tree), agent.WithNode(rootNode), agent.WithSpawnDepth(0))

	tree.BeginTurnWithPrompt("debate a roadmap for code-index")
	if _, err := sp.Spawn(context.Background(), agent.SpawnOpts{Label: "Kimi Strategist"}); err != nil {
		t.Fatalf("spawn 1a: %v", err)
	}
	if _, err := sp.Spawn(context.Background(), agent.SpawnOpts{Label: "Kimi Critic"}); err != nil {
		t.Fatalf("spawn 1b: %v", err)
	}
	tree.EndTurn(false)
	tree.BeginTurnWithPrompt("review this analysis and give your 2 cents")
	if _, err := sp.Spawn(context.Background(), agent.SpawnOpts{Label: "Kimi Advocate"}); err != nil {
		t.Fatalf("spawn 2a: %v", err)
	}

	m := NewAgentsModel(tree, nil)
	m.width, m.height = 120, 34
	m.refreshSnapshot()
	return tree, m
}

// TestTurnsInLeftTree_Visual builds a two-turn session that spawns agents in each
// turn and prints the left pane, asserting the turn-grouped tree shape.
func TestTurnsInLeftTree_Visual(t *testing.T) {
	tree := agent.NewAgentTreeWithConcurrency("kimi", "glm", 16)
	rootNode := tree.Node(tree.RootID())
	sp := agent.NewSpawner(agent.WithTree(tree), agent.WithNode(rootNode), agent.WithSpawnDepth(0))

	// Turn 1 spawns two agents.
	tree.BeginTurnWithPrompt("debate a roadmap for code-index")
	if _, err := sp.Spawn(context.Background(), agent.SpawnOpts{Label: "Kimi Strategist"}); err != nil {
		t.Fatalf("spawn 1a: %v", err)
	}
	if _, err := sp.Spawn(context.Background(), agent.SpawnOpts{Label: "Kimi Critic"}); err != nil {
		t.Fatalf("spawn 1b: %v", err)
	}
	tree.EndTurn(false)

	// Turn 2 spawns one agent.
	tree.BeginTurnWithPrompt("review this analysis and give your 2 cents")
	if _, err := sp.Spawn(context.Background(), agent.SpawnOpts{Label: "Kimi Advocate"}); err != nil {
		t.Fatalf("spawn 2a: %v", err)
	}

	m := NewAgentsModel(tree, nil)
	m.width, m.height = 120, 34
	m.refreshSnapshot()

	rows := m.renderTreeRows(60)
	got := stripANSITurns(strings.Join(rows, "\n"))
	t.Logf("LEFT TREE (turn 1 collapsed by default):\n%s", got)

	// Turns are the top level (no standalone root row): both turn headers +
	// turn 2's child (current turn expanded); turn 1 is settled, collapsed by
	// default, so its children are hidden.
	for _, want := range []string{"▸ turn 1", "▾ turn 2", "Kimi Advocate"} {
		if !strings.Contains(got, want) {
			t.Errorf("left tree missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Kimi Strategist") {
		t.Errorf("turn 1 is collapsed; its children must be hidden:\n%s", got)
	}
	// There is NO standalone root row — the root's activity lives inside its turns.
	if strings.HasPrefix(strings.TrimSpace(got), "kimi") {
		t.Errorf("unexpected standalone root row; turns should be the top level:\n%s", got)
	}

	// Toggle turn 1 open: its children appear.
	m.toggleTurn(1, false)
	got2 := stripANSITurns(strings.Join(m.renderTreeRows(60), "\n"))
	t.Logf("LEFT TREE (turn 1 expanded):\n%s", got2)
	for _, want := range []string{"Kimi Strategist", "Kimi Critic"} {
		if !strings.Contains(got2, want) {
			t.Errorf("after expanding turn 1, missing %q:\n%s", want, got2)
		}
	}

	// Provenance check: each child carries the turn it was spawned in.
	byLabel := map[string]int{}
	for _, n := range m.nodes {
		byLabel[n.Label] = n.SpawnedInTurn
	}
	if byLabel["Kimi Strategist"] != 1 || byLabel["Kimi Critic"] != 1 {
		t.Errorf("turn-1 children have wrong SpawnedInTurn: %+v", byLabel)
	}
	if byLabel["Kimi Advocate"] != 2 {
		t.Errorf("Kimi Advocate SpawnedInTurn=%d, want 2", byLabel["Kimi Advocate"])
	}
}

// TestLeftTree_EnterOnHeaderToggles drives the real key handler: enter on a turn
// header toggles its group, never drilling into the detail pane. Turns are the
// top level, so turn 1's header is ROW 0.
func TestLeftTree_EnterOnHeaderToggles(t *testing.T) {
	_, m := twoTurnSpawnedTree(t)
	_ = m.renderTreeRows(60) // build the row model

	m.selected = 0
	if !m.treeRows[m.selected].header || m.treeRows[m.selected].turn != 1 {
		t.Fatalf("row 0 is not turn 1's header: %+v", m.treeRows[m.selected])
	}
	// Enter on the header must toggle, NOT enter detail.
	m.handleTreeKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.inDetail {
		t.Fatal("enter on a turn header wrongly opened the detail pane")
	}
	rows := stripANSITurns(strings.Join(m.renderTreeRows(60), "\n"))
	if !strings.Contains(rows, "Kimi Strategist") {
		t.Errorf("turn 1 did not expand on enter:\n%s", rows)
	}
	// Enter again collapses.
	m.handleTreeKey(tea.KeyMsg{Type: tea.KeyEnter})
	rows = stripANSITurns(strings.Join(m.renderTreeRows(60), "\n"))
	if strings.Contains(rows, "Kimi Strategist") {
		t.Errorf("turn 1 did not collapse on second enter:\n%s", rows)
	}
}

// TestLeftTree_HeaderInspectsRootTurnSlice confirms a turn HEADER resolves to the
// ROOT node with that turn's filter, so its detail is the root's turn-scoped
// transcript (the fix for empty-turns-when-root-works-directly).
func TestLeftTree_HeaderInspectsRootTurnSlice(t *testing.T) {
	_, m := twoTurnSpawnedTree(t)
	_ = m.renderTreeRows(60)

	// Row 0 is turn 1's header -> resolves to the root node, turn filter 1.
	m.selected = 0
	n, ok := m.selectedNode()
	if !ok || n.Depth != 0 {
		t.Fatalf("turn-1 header should resolve to the root node; ok=%v depth=%d", ok, n.Depth)
	}
	if tf := m.selectedTurnFilter(); tf != 1 {
		t.Errorf("turn-1 header turn filter = %d, want 1", tf)
	}
	// A spawned-agent (node) row resolves to that node with no turn filter.
	m.toggleTurn(1, false) // expand turn 1 so its children are rows
	_ = m.renderTreeRows(60)
	var childRow int
	for i, tr := range m.treeRows {
		if !tr.header && tr.node.Label == "Kimi Strategist" {
			childRow = i
		}
	}
	m.selected = childRow
	if cn, ok := m.selectedNode(); !ok || cn.Label != "Kimi Strategist" {
		t.Errorf("child row should resolve to its own node; got ok=%v label=%q", ok, cn.Label)
	}
	if tf := m.selectedTurnFilter(); tf != 0 {
		t.Errorf("child row turn filter = %d, want 0 (no filter)", tf)
	}
}

// TestLeftTree_SingleTurnIsFlat pins the byte-identical legacy path: a root with
// at most one turn mark renders the flat node list, no headers.
func TestLeftTree_SingleTurnIsFlat(t *testing.T) {
	tree := agent.NewAgentTreeWithConcurrency("root", "glm", 16)
	rootNode := tree.Node(tree.RootID())
	sp := agent.NewSpawner(agent.WithTree(tree), agent.WithNode(rootNode), agent.WithSpawnDepth(0))
	tree.BeginTurnWithPrompt("do one thing")
	if _, err := sp.Spawn(context.Background(), agent.SpawnOpts{Label: "child"}); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	m := NewAgentsModel(tree, nil)
	m.width, m.height = 120, 34
	m.refreshSnapshot()

	got := stripANSITurns(strings.Join(m.renderTreeRows(60), "\n"))
	if strings.Contains(got, "turn 1") {
		t.Errorf("single-turn tree must NOT render a turn header:\n%s", got)
	}
	for _, want := range []string{"root", "child"} {
		if !strings.Contains(got, want) {
			t.Errorf("single-turn tree missing %q:\n%s", want, got)
		}
	}
	// The row model is 1:1 with nodes (no header rows).
	for i, tr := range m.treeRows {
		if tr.header {
			t.Errorf("row %d is a header in a single-turn tree", i)
		}
	}
}

// TestLeftTree_RefreshDoesNotSnapCursorOffTurnRows reproduces the reported bug:
// on a fresh turn the down arrow "snapped back to the top" until responses
// arrived. Cause: refreshSnapshot clamped m.selected against len(m.nodes), but
// m.selected indexes the row model (headers + nodes), which is larger. A cursor
// parked on a turn-header row (index >= len(nodes)) got yanked up on the next
// refresh (fired by every tree update / 250ms tick).
func TestLeftTree_RefreshDoesNotSnapCursorOffTurnRows(t *testing.T) {
	_, m := twoTurnSpawnedTree(t)
	// Both turns expanded: rows = [▾turn1, child, child, ▾turn2, child] = 5 rows
	// over 4 nodes (root + 3 children), so the header rows push indices past
	// len(nodes). (Turns are the top level; there is no standalone root row.)
	m.toggleTurn(1, false)
	_ = m.renderTreeRows(60)
	if len(m.treeRows) <= len(m.nodes) {
		t.Fatalf("test needs rows(%d) > nodes(%d)", len(m.treeRows), len(m.nodes))
	}

	// Park the cursor on the LAST row (turn 2's header), which is past len(nodes).
	m.selected = len(m.treeRows) - 1
	parked := m.selected

	// A refresh fires constantly (tree updates, 250ms tick). It must NOT move the
	// cursor when the row model is unchanged.
	m.refreshSnapshot()
	if m.selected != parked {
		t.Errorf("refreshSnapshot snapped the cursor from row %d to %d (the reported bug)", parked, m.selected)
	}

	// And down-navigation from a valid row must actually move (not be clamped by a
	// stale node-count bound).
	m.selected = 2 // a child row
	m.handleTreeKey(tea.KeyMsg{Type: tea.KeyDown})
	if m.selected != 3 {
		t.Errorf("down from row 2 = %d, want 3", m.selected)
	}
}

// TestLeftTree_RootWorksDirectlyTurnsShowSlices is the reported case: a root that
// does all the work ITSELF (no spawns) across turns. Turns must not be empty —
// selecting each turn header shows the root's events for THAT turn only.
func TestLeftTree_RootWorksDirectlyTurnsShowSlices(t *testing.T) {
	tree := agent.NewAgentTree("glm", "glm")
	root := tree.Node(tree.RootID())

	tree.BeginTurnWithPrompt("do analysis of this repo")
	root.AddEvent(agent.AgentEvent{Kind: agent.KindToolCall, Name: "bash", TS: time.Now()})
	root.AddEvent(agent.AgentEvent{Kind: agent.KindAssistant, Name: "t1-assistant", TS: time.Now()})
	tree.EndTurn(false)

	tree.BeginTurnWithPrompt("whats next")
	time.Sleep(2 * time.Millisecond)
	root.AddEvent(agent.AgentEvent{Kind: agent.KindAssistant, Name: "t2-assistant", TS: time.Now()})

	m := NewAgentsModel(tree, nil)
	m.width, m.height = 120, 34
	m.refreshSnapshot()
	_ = m.renderTreeRows(60)

	// No standalone root row; both turns are headers.
	got := stripANSITurns(strings.Join(m.renderTreeRows(60), "\n"))
	if strings.HasPrefix(strings.TrimSpace(got), "glm") {
		t.Errorf("unexpected standalone root row:\n%s", got)
	}

	// Select turn 1 header -> exactly turn 1's two root events.
	for i, tr := range m.treeRows {
		if tr.header && tr.turn == 1 {
			m.selected = i
		}
	}
	_ = m.buildDetailLines(100)
	if m.eventCount != 2 {
		t.Errorf("turn 1 detail eventCount = %d, want 2 (bash + t1-assistant)", m.eventCount)
	}

	// Select turn 2 header -> exactly turn 2's one root event, NOT empty.
	for i, tr := range m.treeRows {
		if tr.header && tr.turn == 2 {
			m.selected = i
		}
	}
	d2 := stripANSITurns(strings.Join(m.buildDetailLines(100), "\n"))
	if m.eventCount != 1 {
		t.Errorf("turn 2 detail eventCount = %d, want 1 (t2-assistant) — turns must not be empty", m.eventCount)
	}
	if !strings.Contains(d2, "t2-assistant") {
		t.Errorf("turn 2 detail missing its event:\n%s", d2)
	}
	if strings.Contains(d2, "t1-assistant") {
		t.Errorf("turn 2 detail leaked turn 1's events:\n%s", d2)
	}
}
