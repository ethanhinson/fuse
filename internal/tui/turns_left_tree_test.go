package tui

import (
	"context"
	"strings"
	"testing"

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

	// Default: root + both turn headers + turn 2's child (current turn expanded);
	// turn 1 is a settled turn, collapsed by default, so its children are hidden.
	for _, want := range []string{"kimi", "▸ turn 1", "▾ turn 2", "Kimi Advocate"} {
		if !strings.Contains(got, want) {
			t.Errorf("left tree missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Kimi Strategist") {
		t.Errorf("turn 1 is collapsed; its children must be hidden:\n%s", got)
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
// header toggles its group, never drilling into the detail pane.
func TestLeftTree_EnterOnHeaderToggles(t *testing.T) {
	_, m := twoTurnSpawnedTree(t)
	_ = m.renderTreeRows(60) // build the row model

	// Row 0 is the root; row 1 is turn 1's header (collapsed by default).
	m.selected = 1
	if !m.treeRows[m.selected].header || m.treeRows[m.selected].turn != 1 {
		t.Fatalf("row 1 is not turn 1's header: %+v", m.treeRows[m.selected])
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

// TestLeftTree_SelectedNodeSkipsHeaders confirms selectedNode returns a node for
// a node row and nothing for a header row (so the detail pane shows the guard).
func TestLeftTree_SelectedNodeSkipsHeaders(t *testing.T) {
	_, m := twoTurnSpawnedTree(t)
	_ = m.renderTreeRows(60)

	// Root row -> a node.
	m.selected = 0
	if n, ok := m.selectedNode(); !ok || n.Depth != 0 {
		t.Errorf("row 0 should resolve to the root node; ok=%v", ok)
	}
	// Turn header row -> no node.
	m.selected = 1
	if _, ok := m.selectedNode(); ok {
		t.Error("a turn header row must not resolve to a node")
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
