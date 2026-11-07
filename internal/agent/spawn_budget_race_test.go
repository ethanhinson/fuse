package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// TestSpawnBudgetReservedAtomically is the regression guard for the
// check-then-act budget defect: parallel Spawn calls in one batch used to all
// read SpawnBudget ("room left") before any child node reached the tree, so a
// budget of N could start N+k children. With reserveSpawn the budget unit is
// consumed atomically at admission, so exactly max_spawns spawns are admitted
// and the rest are refused with ErrSpawnBudgetExhausted. Run under -race to
// exercise the reservation counter concurrently.
func TestSpawnBudgetReservedAtomically(t *testing.T) {
	const budget = 2
	const batch = 12

	tree := NewAgentTree("root", "m")
	tree.SetMaxSpawns(budget)

	s := NewSpawner(WithTree(tree), WithChildBuilder(
		func(ctx context.Context, opts SpawnOpts, node *AgentNode, _ *AgentTree) (string, error) {
			return "ok", nil
		}))

	var wg sync.WaitGroup
	var mu sync.Mutex
	var admitted, refused int
	for i := 0; i < batch; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := s.Spawn(context.Background(), SpawnOpts{Label: "c"})
			mu.Lock()
			if err != nil {
				if !errors.Is(err, ErrSpawnBudgetExhausted) {
					t.Errorf("spawn err = %v, want ErrSpawnBudgetExhausted", err)
				}
				refused++
			} else {
				admitted++
				h.Wait()
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	if admitted != budget {
		t.Fatalf("admitted = %d, want exactly %d (budget)", admitted, budget)
	}
	if refused != batch-budget {
		t.Fatalf("refused = %d, want %d", refused, batch-budget)
	}
	if used, max := tree.SpawnBudget(); used != budget || max != budget {
		t.Fatalf("SpawnBudget = %d/%d, want %d/%d", used, max, budget, budget)
	}
}

// TestSpawnBudgetReservationNotReleasedOnFailure pins the accounting semantics:
// a child whose run fails has still consumed its budget unit — the reservation
// is fulfilled into the tree's node count and never refunded, matching the
// pre-fix behavior where a failed child's node stayed in the tree and counted.
func TestSpawnBudgetReservationNotReleasedOnFailure(t *testing.T) {
	tree := NewAgentTree("root", "m")
	tree.SetMaxSpawns(1)

	s := NewSpawner(WithTree(tree), WithChildBuilder(
		func(ctx context.Context, opts SpawnOpts, node *AgentNode, _ *AgentTree) (string, error) {
			return "", errors.New("boom")
		}))

	h, err := s.Spawn(context.Background(), SpawnOpts{Label: "fail"})
	if err != nil {
		t.Fatalf("first spawn: %v", err)
	}
	if d := h.Wait(); d.Err == nil {
		t.Fatal("expected the child run to fail")
	}

	// The budget was consumed by the failed spawn: the next one must be refused.
	if _, err := s.Spawn(context.Background(), SpawnOpts{Label: "next"}); !errors.Is(err, ErrSpawnBudgetExhausted) {
		t.Fatalf("second spawn err = %v, want ErrSpawnBudgetExhausted (failed spawn still consumed budget)", err)
	}
}

// A batch that exactly fits the budget must always be admitted in full. A moment
// where one spawn counted as both a node and a reservation would refuse one of them.
func TestSpawnBudgetExactBatchIsNeverRefused(t *testing.T) {
	for round := 0; round < 200; round++ {
		const budget = 8
		tree := NewAgentTree("root", "m")
		tree.SetMaxSpawns(budget)
		s := NewSpawner(WithTree(tree), WithChildBuilder(
			func(ctx context.Context, opts SpawnOpts, node *AgentNode, _ *AgentTree) (string, error) {
				return "ok", nil
			}))
		var wg sync.WaitGroup
		errs := make(chan error, budget)
		for i := 0; i < budget; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				h, err := s.Spawn(context.Background(), SpawnOpts{Label: "c"})
				if err != nil {
					errs <- err
					return
				}
				h.Wait()
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("round %d: a batch within budget was refused: %v", round, err)
		}
	}
}

// A spawn refused before it had a node must give its budget back.
func TestSpawnBudgetReleasedWhenRefusedBeforeANodeExists(t *testing.T) {
	tree := NewAgentTree("root", "m")
	tree.SetMaxSpawns(1)
	sched := tree.Scheduler()
	if err := sched.reserveSpawn(); err != nil {
		t.Fatal(err)
	}
	if err := sched.reserveSpawn(); !errors.Is(err, ErrSpawnBudgetExhausted) {
		t.Fatalf("second reservation = %v, want exhausted", err)
	}
	sched.releaseSpawn()
	if err := sched.reserveSpawn(); err != nil {
		t.Fatalf("budget was not given back: %v", err)
	}
}

// The same through Spawn: a token-quota refusal happens after the budget reservation,
// and must not spend the budget of a child that never existed.
func TestSpawnRefusedByTokenQuotaKeepsItsBudget(t *testing.T) {
	tree := NewAgentTree("root", "m")
	tree.SetMaxSpawns(1)
	tree.Scheduler().SetSessionTokens(100)
	tree.Node(tree.RootID()).UpdateTokens(100, 0)
	s := NewSpawner(WithTree(tree), WithChildBuilder(
		func(ctx context.Context, opts SpawnOpts, node *AgentNode, _ *AgentTree) (string, error) {
			return "ok", nil
		}))
	if _, err := s.Spawn(context.Background(), SpawnOpts{Label: "refused"}); err == nil {
		t.Fatal("spawn over the token quota was admitted")
	}
	tree.Scheduler().SetSessionTokens(0)
	h, err := s.Spawn(context.Background(), SpawnOpts{Label: "after"})
	if err != nil {
		t.Fatalf("the refused spawn used up the budget: %v", err)
	}
	h.Wait()
}
