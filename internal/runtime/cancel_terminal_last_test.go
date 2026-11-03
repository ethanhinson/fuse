package runtime

import (
	"context"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/tools"
)

// lateEmitter stands in for what a binding attaches to a loop's store in
// BuildAgent and that can still fire after the loop's run returned: the per-loop
// sandbox pool's Reaped hook (its Close in LoopTeardown, or its idle reaper), and
// the process-scoped sandbox gate/health hooks, which keep the last loop's store
// until another loop rebinds them.
type lateEmitter struct {
	mu    sync.Mutex
	store event.EventStore
	errs  []error
}

func (l *lateEmitter) bind(store event.EventStore) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.store = store
}

func (l *lateEmitter) emit(nodeID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.store == nil {
		return
	}
	l.errs = append(l.errs, l.store.Append(event.Event{NodeID: nodeID, Kind: event.KindSandboxReap}))
}

func (l *lateEmitter) refusedAll() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, err := range l.errs {
		if err == nil {
			return false
		}
	}
	return len(l.errs) > 0
}

// assertCancelledLast fails unless loop.cancelled is the stream's last event and
// nothing of kind sandbox.reap follows it.
func assertCancelledLast(t *testing.T, evs []event.Event) {
	t.Helper()
	i := indexOfKind(evs, event.KindLoopCancelled)
	if i < 0 {
		t.Fatal("the stream holds no loop.cancelled")
	}
	for _, e := range evs[i+1:] {
		t.Errorf("event %d (%s) follows loop.cancelled (seq %d)", e.Seq, e.Kind, evs[i].Seq)
	}
}

// Spec §3.2 step 3: loop.cancelled is the highest seq a cancelled loop's stream
// ever holds. LoopTeardown runs after the terminal append, and what it releases
// (the sandbox pool, whose Reaped hook writes to the loop's store) must not land
// after it; neither may an emitter that fires once Cancel has returned.
func TestCancelLiveLoopNothingFollowsTerminal(t *testing.T) {
	bc := newCancelBlockingCompleter()
	late := &lateEmitter{}
	var loopID string
	g := newCancelRig(t, cancelRigOpts{
		completer: bc,
		build: func(store event.EventStore, tree *agent.AgentTree, _ *tools.Registry) {
			loopID = tree.RootID()
			late.bind(store)
		},
		teardown: func(*tools.Registry) { late.emit(loopID) },
	})
	ctx := context.Background()
	h, err := g.rt.StartLoop(ctx, LoopConfig{Task: "work", ModelID: "m", Tenant: acme})
	if err != nil {
		t.Fatal(err)
	}
	<-bc.started
	res, err := g.c.Cancel(ctx, acme, h.ID(), "operator stop", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != CancelOutcomeCancelled || !res.WasLive {
		t.Fatalf("result = %+v, want Cancelled, was_live", res)
	}
	if g.teardownCount() != 1 {
		t.Fatalf("LoopTeardown ran %d times, want 1", g.teardownCount())
	}
	late.emit(h.ID()) // an idle reaper or process-scoped hook, after Cancel returned

	evs := streamOf(t, g.store, acme, h.ID())
	assertCancelledLast(t, evs)
	if last := evs[len(evs)-1]; last.Seq != res.TerminalSeq {
		t.Fatalf("last seq %d, Cancel reported %d", last.Seq, res.TerminalSeq)
	}
	if !late.refusedAll() {
		t.Fatal("an append to a sealed loop's store must report an error")
	}
}

// The same holds for a loop that finished on its own and is sealed afterwards: an
// emitter still bound to its store must not write past the terminal event.
func TestCancelFinishedLoopNothingFollowsTerminal(t *testing.T) {
	late := &lateEmitter{}
	g := newCancelRig(t, cancelRigOpts{
		completer: script(modelText("done")),
		build: func(store event.EventStore, _ *agent.AgentTree, _ *tools.Registry) {
			late.bind(store)
		},
	})
	ctx := context.Background()
	h, err := g.rt.StartLoop(ctx, LoopConfig{Task: "work", ModelID: "m", Tenant: acme})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatal(err)
	}
	res, err := g.c.Cancel(ctx, acme, h.ID(), "cleanup", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != CancelOutcomeCancelled || res.WasLive {
		t.Fatalf("result = %+v, want Cancelled, was_live=false", res)
	}
	late.emit(h.ID())

	assertCancelledLast(t, streamOf(t, g.store, acme, h.ID()))
	if !late.refusedAll() {
		t.Fatal("an append to a sealed loop's store must report an error")
	}
}
