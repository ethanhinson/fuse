package agent

import (
	"context"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
)

// gatedStore holds every spawn.done Append until release is closed.
type gatedStore struct {
	recordingStore
	reached chan struct{}
	release chan struct{}
}

func (g *gatedStore) Append(e event.Event) error {
	if e.Kind == event.KindSpawnDone {
		close(g.reached)
		<-g.release
	}
	return g.recordingStore.Append(e)
}

// SpawnsInFlight stays non-zero after the child's node has finished and until its
// goroutine has emitted spawn.done and released its slot: a node's status flips
// before spawn.done is emitted, so status alone cannot order work after it.
func TestSpawnsInFlightCoversSpawnDone(t *testing.T) {
	tree := NewAgentTree("root", "m")
	st := &gatedStore{reached: make(chan struct{}), release: make(chan struct{})}
	s := NewSpawner(
		WithTree(tree),
		WithNode(tree.Node(tree.RootID())),
		WithSpawnDepth(0),
		WithEventStore(st),
		WithChildBuilder(func(context.Context, SpawnOpts, *AgentNode, *AgentTree) (string, error) {
			return "ok", nil
		}),
	)
	h, err := s.Spawn(context.Background(), SpawnOpts{Label: "kid", Task: "t"})
	if err != nil {
		t.Fatal(err)
	}
	<-st.reached
	if running, pending := tree.ActiveCounts(); running != 0 || pending != 0 {
		t.Fatalf("ActiveCounts = (%d, %d) at spawn.done, want (0, 0): the node finishes first", running, pending)
	}
	if n := tree.SpawnsInFlight(); n != 1 {
		t.Fatalf("SpawnsInFlight = %d before spawn.done is recorded, want 1", n)
	}
	close(st.release)
	h.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for tree.SpawnsInFlight() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("SpawnsInFlight = %d after the child finished, want 0", tree.SpawnsInFlight())
		}
		time.Sleep(time.Millisecond)
	}
	if n := tree.Scheduler().Snapshot().SlotsInUse; n != 0 {
		t.Fatalf("SlotsInUse = %d, want 0", n)
	}
}
