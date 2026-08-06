package agent

import (
	"context"
	"errors"
	"testing"
)

// The workflow backstop is the race/batch safety net for a workflow pool: even
// when the model emits a batch of spawn calls in ONE turn (so the per-turn strip
// saw the tool present at turn start), each individual Spawn is still checked, so
// the pool's total quota cannot be overshot within a turn.
func TestSpawnWorkflowBackstopFires(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 16)
	tr.SetMaxSpawns(64) // global budget is generous; the workflow total is the binding cap
	root := tr.Node(tr.RootID())

	total := 2
	spawned := 0
	backstop := func(newDepth int) error {
		if spawned >= total {
			return ErrWorkflowQuotaExhausted
		}
		return nil
	}
	s := NewSpawner(
		WithTree(tr),
		WithNode(root),
		WithSpawnDepth(0),
		WithChildBuilder(func(ctx context.Context, o SpawnOpts, n *AgentNode, tt *AgentTree) (string, error) { return "ok", nil }),
		WithSpawnBackstop(backstop),
	)

	// First two spawns succeed; the third is refused by the backstop.
	for i := 0; i < 2; i++ {
		if _, err := s.Spawn(context.Background(), SpawnOpts{Label: "c", Task: "t"}); err != nil {
			t.Fatalf("spawn %d unexpectedly failed: %v", i, err)
		}
		spawned++
	}
	if _, err := s.Spawn(context.Background(), SpawnOpts{Label: "c", Task: "t"}); !errors.Is(err, ErrWorkflowQuotaExhausted) {
		t.Fatalf("third spawn should be refused by workflow backstop, got %v", err)
	}
}

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
