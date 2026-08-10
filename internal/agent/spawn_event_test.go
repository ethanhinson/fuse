package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
)

// TestSpawnerEmitsSpawnStartDone is the change-0044 gate: the Spawner is the
// single choke point that emits the spawn.start (at admission) / spawn.done (at
// completion) pair onto the 0043 EventStore, so an observer holding no handle
// still sees the child lifecycle. Emission is relocated here from the three
// cmd-site child builders (0043) — one authoritative site every spawn passes.
func TestSpawnerEmitsSpawnStartDone(t *testing.T) {
	tree := NewAgentTree("root", "m")
	rec := &recordingStore{}
	root := tree.Node(tree.RootID())

	s := NewSpawner(
		WithTree(tree),
		WithNode(root),
		WithSpawnDepth(0),
		WithEventStore(rec),
		WithChildBuilder(func(ctx context.Context, opts SpawnOpts, node *AgentNode, _ *AgentTree) (string, error) {
			return "child result text", nil
		}),
	)

	h, err := s.Spawn(context.Background(), SpawnOpts{Label: "worker", Task: "do the thing"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	done := h.Wait()
	if done.Err != nil {
		t.Fatalf("child err: %v", done.Err)
	}

	// Filter to the spawn lifecycle kinds (the tree may emit other events).
	var start, doneEv *event.Event
	rec.mu.Lock()
	evs := append([]event.Event(nil), rec.events...)
	rec.mu.Unlock()
	for i := range evs {
		switch evs[i].Kind {
		case event.KindSpawnStart:
			if start != nil {
				t.Fatalf("more than one spawn.start emitted")
			}
			start = &evs[i]
		case event.KindSpawnDone:
			if doneEv != nil {
				t.Fatalf("more than one spawn.done emitted")
			}
			doneEv = &evs[i]
		}
	}
	if start == nil {
		t.Fatal("no spawn.start event emitted")
	}
	if doneEv == nil {
		t.Fatal("no spawn.done event emitted")
	}
	// Ordering: start is admitted before done completes.
	if !(start.Seq < doneEv.Seq) {
		t.Fatalf("spawn.start Seq (%d) must precede spawn.done Seq (%d)", start.Seq, doneEv.Seq)
	}

	var sp event.SpawnStartPayload
	if err := json.Unmarshal(start.Payload, &sp); err != nil {
		t.Fatalf("spawn.start payload: %v", err)
	}
	if sp.ChildNodeID != h.NodeID || sp.Label != "worker" || sp.Task != "do the thing" {
		t.Fatalf("spawn.start payload = %+v (nodeID want %q)", sp, h.NodeID)
	}
	if start.NodeID != h.NodeID {
		t.Fatalf("spawn.start envelope NodeID = %q, want %q", start.NodeID, h.NodeID)
	}

	var dp event.SpawnDonePayload
	if err := json.Unmarshal(doneEv.Payload, &dp); err != nil {
		t.Fatalf("spawn.done payload: %v", err)
	}
	if dp.ChildNodeID != h.NodeID {
		t.Fatalf("spawn.done ChildNodeID = %q, want %q", dp.ChildNodeID, h.NodeID)
	}
	if dp.Label != "worker" {
		t.Fatalf("spawn.done Label = %q, want worker", dp.Label)
	}
	if dp.Result != "child result text" {
		t.Fatalf("spawn.done Result = %q, want the child's returned result", dp.Result)
	}
	if dp.Err != "" {
		t.Fatalf("spawn.done Err = %q, want empty", dp.Err)
	}
}

// TestSpawnerEmitsSpawnDoneError: a child that errors surfaces the error on the
// spawn.done event (Err set), mirroring the direct-log "error" kind selection.
func TestSpawnerEmitsSpawnDoneError(t *testing.T) {
	tree := NewAgentTree("root", "m")
	rec := &recordingStore{}
	root := tree.Node(tree.RootID())

	s := NewSpawner(
		WithTree(tree), WithNode(root), WithSpawnDepth(0), WithEventStore(rec),
		WithChildBuilder(func(ctx context.Context, opts SpawnOpts, node *AgentNode, _ *AgentTree) (string, error) {
			return "", context.DeadlineExceeded
		}),
	)
	h, err := s.Spawn(context.Background(), SpawnOpts{Label: "w", Task: "t"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	h.Wait()

	var dp *event.SpawnDonePayload
	rec.mu.Lock()
	evs := append([]event.Event(nil), rec.events...)
	rec.mu.Unlock()
	for _, e := range evs {
		if e.Kind == event.KindSpawnDone {
			var p event.SpawnDonePayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			dp = &p
		}
	}
	if dp == nil {
		t.Fatal("no spawn.done emitted")
	}
	if dp.Err == "" {
		t.Fatal("spawn.done Err empty, want the child's error string")
	}
}

// TestSpawnerNoEventStoreIsInert: a Spawner with no EventStore (the default,
// as in one-shot/probe/mcp paths pre-wiring) spawns normally and never panics —
// the default NoopStore swallows emission.
func TestSpawnerNoEventStoreIsInert(t *testing.T) {
	tree := NewAgentTree("root", "m")
	s := NewSpawner(
		WithTree(tree), WithNode(tree.Node(tree.RootID())), WithSpawnDepth(0),
		WithChildBuilder(func(ctx context.Context, opts SpawnOpts, node *AgentNode, _ *AgentTree) (string, error) {
			return "ok", nil
		}),
	)
	h, err := s.Spawn(context.Background(), SpawnOpts{Label: "w", Task: "t"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if got := h.Wait(); got.Result != "ok" || got.Err != nil {
		t.Fatalf("wait = %+v, want result=ok err=nil", got)
	}
	_ = time.Now
}
