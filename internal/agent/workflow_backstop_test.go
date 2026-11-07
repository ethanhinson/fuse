package agent

import (
	"context"
	"testing"
)

func TestSpawnNoBackstopIsNoop(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 16)
	tr.SetMaxSpawns(64)
	root := tr.Node(tr.RootID())
	s := NewSpawner(
		WithTree(tr),
		WithNode(root),
		WithSpawnDepth(0),
		WithChildBuilder(func(ctx context.Context, o SpawnOpts, n *AgentNode, tt *AgentTree) (string, error) { return "ok", nil }),
	)
	if _, err := s.Spawn(context.Background(), SpawnOpts{Label: "c", Task: "t"}); err != nil {
		t.Fatalf("no backstop configured: spawn should succeed, got %v", err)
	}
}
