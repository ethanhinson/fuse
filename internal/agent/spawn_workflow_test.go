package agent

import (
	"context"
	"errors"
	"testing"
)

func rootSpawner(tree *AgentTree, fn ChildBuilder) *Spawner {
	return NewSpawner(WithTree(tree), WithNode(tree.Node(tree.RootID())), WithChildBuilder(fn))
}

func TestSpawnWorkflowMarksChildAsWorkflowRoot(t *testing.T) {
	tree := NewAgentTree("root", "m")
	sp := rootSpawner(tree, fixedBuilder("ok", nil, nil))
	h, err := sp.Spawn(context.Background(), SpawnOpts{Label: "lead", Task: "t", Workflow: "wf"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	h.Wait()
	if got := tree.Node(h.NodeID).Snapshot().WorkflowRoot; got != "wf" {
		t.Fatalf("WorkflowRoot = %q, want %q", got, "wf")
	}
	if got := tree.WorkflowRootOf(h.NodeID); got != h.NodeID {
		t.Fatalf("WorkflowRootOf = %q, want the child itself %q", got, h.NodeID)
	}
}

func TestSpawnWithoutWorkflowLeavesMarkerEmpty(t *testing.T) {
	tree := NewAgentTree("root", "m")
	sp := rootSpawner(tree, fixedBuilder("ok", nil, nil))
	h, err := sp.Spawn(context.Background(), SpawnOpts{Label: "plain", Task: "t"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	h.Wait()
	if got := tree.Node(h.NodeID).Snapshot().WorkflowRoot; got != "" {
		t.Fatalf("WorkflowRoot = %q, want empty", got)
	}
}

func TestSpawnRefusesNestedWorkflow(t *testing.T) {
	tree := NewAgentTree("root", "m")
	sp := rootSpawner(tree, fixedBuilder("ok", nil, nil))
	lead, err := sp.Spawn(context.Background(), SpawnOpts{Label: "lead", Task: "t", Workflow: "wf"})
	if err != nil {
		t.Fatalf("spawn lead: %v", err)
	}
	lead.Wait()

	inner := NewSpawner(WithTree(tree), WithNode(tree.Node(lead.NodeID)), WithSpawnDepth(1),
		WithChildBuilder(fixedBuilder("ok", nil, nil)))
	before := len(tree.SnapshotAll())
	_, err = inner.Spawn(context.Background(), SpawnOpts{Label: "n", Task: "t", Workflow: "wf2"})
	if !errors.Is(err, ErrNestedWorkflow) {
		t.Fatalf("expected ErrNestedWorkflow, got %v", err)
	}
	if after := len(tree.SnapshotAll()); after != before {
		t.Fatalf("nested refusal added nodes: %d -> %d", before, after)
	}

	gc, err := inner.Spawn(context.Background(), SpawnOpts{Label: "gc", Task: "t"})
	if err != nil {
		t.Fatalf("plain grandchild spawn inside the subtree should succeed: %v", err)
	}
	gc.Wait()
	if got := tree.WorkflowRootOf(gc.NodeID); got != lead.NodeID {
		t.Fatalf("grandchild governed by %q, want lead %q", got, lead.NodeID)
	}
}

func TestWithBuilderReplacesOnlyTheChildBuilder(t *testing.T) {
	tree := NewAgentTree("root", "m")
	sp := rootSpawner(tree, fixedBuilder("A", nil, nil))
	alt := sp.WithBuilder(fixedBuilder("B", nil, nil))

	hb, err := alt.Spawn(context.Background(), SpawnOpts{Label: "b", Task: "t"})
	if err != nil {
		t.Fatalf("spawn via WithBuilder: %v", err)
	}
	if got := hb.Wait().Result; got != "B" {
		t.Fatalf("WithBuilder spawn ran %q, want B", got)
	}
	ha, err := sp.Spawn(context.Background(), SpawnOpts{Label: "a", Task: "t"})
	if err != nil {
		t.Fatalf("spawn via original: %v", err)
	}
	if got := ha.Wait().Result; got != "A" {
		t.Fatalf("original spawner ran %q, want A (must not be mutated)", got)
	}
	if hb.NodeID == "" || tree.Node(hb.NodeID) == nil || tree.Node(hb.NodeID).ParentID != tree.RootID() {
		t.Fatalf("WithBuilder copy must share tree and node")
	}
}
