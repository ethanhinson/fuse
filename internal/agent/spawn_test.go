package agent

import (
	"context"
	"errors"
	"testing"
)

// makeInstantSpawner returns a spawner func that immediately closes its Done
// channel with a fixed result string.
func makeInstantSpawner(result string) func(ctx context.Context, opts SpawnOpts) (AgentHandle, error) {
	return func(ctx context.Context, opts SpawnOpts) (AgentHandle, error) {
		ch := make(chan SpawnDone, 1)
		ch <- SpawnDone{Result: result}
		return AgentHandle{NodeID: "test", Done: ch, cancel: func() {}}, nil
	}
}

func TestSpawnerDepthLimit(t *testing.T) {
	t.Run("at_max_depth_returns_error", func(t *testing.T) {
		s := NewSpawner(WithSpawnDepth(MaxDepth))
		_, err := s.Spawn(context.Background(), SpawnOpts{Label: "child"})
		if !errors.Is(err, ErrMaxDepthExceeded) {
			t.Fatalf("expected ErrMaxDepthExceeded, got %v", err)
		}
	})

	t.Run("below_max_depth_creates_node", func(t *testing.T) {
		tree := NewAgentTree("root", "m")
		s := NewSpawner(WithSpawnDepth(MaxDepth-1), WithTree(tree))
		h, err := s.Spawn(context.Background(), SpawnOpts{Label: "child"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if h.NodeID == "" {
			t.Fatal("NodeID must be non-empty for a successful spawn")
		}
		// drain done channel to avoid goroutine leak
		<-h.Done
	})
}

func TestSpawnGroupJoin(t *testing.T) {
	group := NewSpawnGroup(makeInstantSpawner("pong"))

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := group.Spawn(ctx, SpawnOpts{Label: "child"}); err != nil {
			t.Fatalf("Spawn %d failed: %v", i, err)
		}
	}

	results, err := group.Join(ctx)
	if err != nil {
		t.Fatalf("Join returned error: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	for i, r := range results {
		if r.Err != nil {
			t.Errorf("result[%d].Err = %v, want nil", i, r.Err)
		}
		if r.Result != "pong" {
			t.Errorf("result[%d].Result = %q, want %q", i, r.Result, "pong")
		}
	}
}

func TestSpawnGroupJoinCancel(t *testing.T) {
	// A spawner that blocks until the context is cancelled.
	blocker := func(ctx context.Context, opts SpawnOpts) (AgentHandle, error) {
		ch := make(chan SpawnDone, 1)
		// never sends on ch; Join will observe ctx cancel
		return AgentHandle{NodeID: "block", Done: ch, cancel: func() {}}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	group := NewSpawnGroup(blocker)

	for i := 0; i < 3; i++ {
		if _, err := group.Spawn(ctx, SpawnOpts{Label: "child"}); err != nil {
			t.Fatalf("Spawn %d failed: %v", i, err)
		}
	}

	// Cancel before Join returns.
	cancel()
	results, err := group.Join(ctx)
	if err != nil {
		t.Fatalf("Join returned error: %v", err)
	}
	for i, r := range results {
		if !errors.Is(r.Err, context.Canceled) {
			t.Errorf("result[%d].Err = %v, want context.Canceled", i, r.Err)
		}
	}
}
