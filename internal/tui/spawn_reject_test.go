package tui

import (
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/permissions"
)

// spawnShell builds a sized shell with a tree attached — the configuration
// under which labelled spawn_agent calls render as live inline blocks.
func spawnShell() ShellModel {
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder, permissions.NewSessionMode(permissions.ModeSmart), true))
	return m.WithTree(agent.NewAgentTree("alpha", "alpha"))
}

func transcriptText(m ShellModel) string {
	var b strings.Builder
	for _, l := range m.lines {
		b.WriteString(l.text)
		b.WriteByte('\n')
	}
	return b.String()
}

// A spawn rejected by the budget/depth backstop never creates a tree node, so
// no node event will ever settle its inline block. The error ToolResultMsg is
// the only signal — it must flip the block from "Running" to the error state
// instead of being skipped.
func TestRejectedSpawnSettlesInlineBlock(t *testing.T) {
	m := spawnShell()
	next, _ := m.Update(ToolCallMsg{Name: "spawn_agent", Args: `{"label":"child-c","task":"x"}`})
	m = next.(ShellModel)
	if !strings.Contains(transcriptText(m), "● Running") {
		t.Fatal("inline running block not created for labelled spawn")
	}

	next, _ = m.Update(ToolResultMsg{
		Name:    "spawn_agent",
		IsError: true,
		Output:  "spawn_agent: agent: spawn budget exhausted: 2/2 spawns used",
	})
	m = next.(ShellModel)

	got := transcriptText(m)
	if strings.Contains(got, "● Running") {
		t.Errorf("rejected spawn still shows Running block:\n%s", got)
	}
	if !strings.Contains(got, "✕ spawn_agent") {
		t.Errorf("rejected spawn block not settled as error:\n%s", got)
	}
	if !strings.Contains(got, "budget exhausted") {
		t.Errorf("error text missing from settled block:\n%s", got)
	}
}

// With several labelled spawns in flight, error results must settle the oldest
// still-unadopted block first (results arrive in call order).
func TestRejectedSpawnSettlesOldestUnadopted(t *testing.T) {
	m := spawnShell()
	for _, args := range []string{`{"label":"c1"}`, `{"label":"c2"}`} {
		next, _ := m.Update(ToolCallMsg{Name: "spawn_agent", Args: args})
		m = next.(ShellModel)
	}
	next, _ := m.Update(ToolResultMsg{Name: "spawn_agent", IsError: true, Output: "spawn_agent: rejected-first"})
	m = next.(ShellModel)

	if m.inlineByLabel["c1"] != nil {
		t.Error("first (oldest) block not settled by first error result")
	}
	if m.inlineByLabel["c2"] == nil {
		t.Error("second block settled out of order")
	}
}

// A block adopted by a real tree node is owned by the node's lifecycle; the
// error-result path must leave it alone (the node events render its state).
func TestAdoptedSpawnBlockNotSettledByResult(t *testing.T) {
	m := spawnShell()
	next, _ := m.Update(ToolCallMsg{Name: "spawn_agent", Args: `{"label":"child-a","task":"x"}`})
	m = next.(ShellModel)
	block := m.inlineByLabel["child-a"]
	if block == nil {
		t.Fatal("inline block not tracked by label")
	}
	block.nodeID = "node-1" // simulate adoption by a tree event
	m.inlineByNode["node-1"] = block

	next, _ = m.Update(ToolResultMsg{Name: "spawn_agent", IsError: true, Output: "spawn_agent: child failed"})
	m = next.(ShellModel)

	if !strings.Contains(transcriptText(m), "● Running") {
		t.Error("adopted block was settled by the result path; node lifecycle owns it")
	}
}

// Safety net: a turn that ends with an unadopted block still in "Running"
// (result lost or never emitted) must not leave the spinner frozen forever.
func TestAgentDoneSettlesUnadoptedBlocks(t *testing.T) {
	m := spawnShell()
	next, _ := m.Update(ToolCallMsg{Name: "spawn_agent", Args: `{"label":"child-x","task":"x"}`})
	m = next.(ShellModel)

	next, _ = m.Update(AgentDoneMsg{})
	m = next.(ShellModel)

	got := transcriptText(m)
	if strings.Contains(got, "● Running") {
		t.Errorf("turn ended with an inline block frozen at Running:\n%s", got)
	}
	if !strings.Contains(got, "✕ spawn_agent") {
		t.Errorf("unadopted block not settled as error at turn end:\n%s", got)
	}
}
