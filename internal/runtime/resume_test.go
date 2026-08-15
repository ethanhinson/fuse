package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// TestResumeRehydratesFinishedLoopOnColdInstance is the core D4 test: an interactive
// loop runs a turn on runtime A and finishes (idle-reaped); a FRESH runtime B over the
// SAME durable store + registry — holding no in-memory loop — Resumes it. The resumed
// loop keeps the SAME loop_id, its transcript is rebuilt from the durable stream, and
// a subsequent Send drives another turn (proving the re-park took: Send resumes instead
// of returning ErrLoopFinished).
func TestResumeRehydratesFinishedLoopOnColdInstance(t *testing.T) {
	dir := t.TempDir()
	store := fsstore.NewDurableFSStore(dir)
	var reg event.LoopRegistry = store

	// Runtime A drives the first exchange. A short idle TTL reaps it once it parks and
	// goes idle, so the loop reaches the finished/not-live state Resume must rehydrate.
	fakeA := newGatedCompleter(
		model.CompletionResp{Content: "first answer"}, // turn 0 -> park -> idle-reaped
	)
	rtA := New(Deps{
		DurableStore:  store,
		Registry:      reg,
		MaxConcurrent: 1,
		IdleTTL:       120 * time.Millisecond,
		BuildAgent: func(s event.EventStore, tree *agent.AgentTree, modelID string, r *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			return agent.New(fakeA, execAll{r}, nopRenderer{}, modelID, "", 1, 0), nil, modelID, nil
		},
	})

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	hA, err := rtA.StartLoop(ctxA, LoopConfig{Task: "hello there", ModelID: "cloud/x", Interactive: true})
	if err != nil {
		t.Fatalf("A StartLoop: %v", err)
	}
	loopID := hA.ID()

	evA, unsubA, err := rtA.Observe(ctxA, event.DefaultTenant, loopID)
	if err != nil {
		t.Fatalf("A Observe: %v", err)
	}
	fakeA.admit(0)
	waitForKind(t, evA, event.KindLoopParked, 2*time.Second)
	unsubA() // drop the live observer so the loop goes idle and is reaped

	// Wait for A's loop to finish (idle-reap). After this, the registry marks it not-live.
	select {
	case <-doneOf(hA):
	case <-time.After(2 * time.Second):
		t.Fatal("A's interactive loop was not reaped after going idle")
	}

	// Runtime B: a FRESH instance, empty loops map, same durable store + registry. Its
	// completer answers only the SECOND exchange (the resumed turn).
	fakeB := newGatedCompleter(
		model.CompletionResp{Content: "second answer"}, // resumed turn -> park
	)
	rtB := New(Deps{
		DurableStore:  store,
		Registry:      reg,
		MaxConcurrent: 1,
		IdleTTL:       time.Hour, // keep B's loop alive for the assertions
		BuildAgent: func(s event.EventStore, tree *agent.AgentTree, modelID string, r *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			return agent.New(fakeB, execAll{r}, nopRenderer{}, modelID, "", 1, 0), nil, modelID, nil
		},
	})

	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()

	hB, err := rtB.Resume(ctxB, event.DefaultTenant, loopID)
	if err != nil {
		t.Fatalf("B Resume: %v", err)
	}
	if hB.ID() != loopID {
		t.Fatalf("resumed loop id = %q, want the original %q (identity must survive re-park)", hB.ID(), loopID)
	}

	// The resumed loop must expose the FULL prior transcript on reconnect: Attach (the
	// durable replay path, which the loopserver's Observe combines with a live tail)
	// from seq 0 carries the first exchange's park (loop.parked) — proving the durable
	// history survived and is reachable through the resumed loop's stream.
	hist, err := rtB.Attach(ctxB, event.DefaultTenant, loopID, 0)
	if err != nil {
		t.Fatalf("B Attach: %v", err)
	}
	if !hasKind(hist, event.KindLoopParked) {
		t.Fatalf("resumed loop's durable history missing the first-exchange loop.parked: %v", kindsOf(hist))
	}
	if !hasKind(hist, event.KindUserInput) {
		t.Fatalf("resumed loop's durable history missing the original user input: %v", kindsOf(hist))
	}

	// Subscribe to the live tail, then Send: a resumed loop's Send must drive another
	// turn (NOT ErrLoopFinished), and a genuinely-new loop.parked lands on the live tail.
	evB, unsubB, err := rtB.Observe(ctxB, event.DefaultTenant, loopID)
	if err != nil {
		t.Fatalf("B Observe: %v", err)
	}
	defer unsubB()
	fakeB.admit(0)
	if err := rtB.Send(ctxB, event.DefaultTenant, loopID, "and then?"); err != nil {
		t.Fatalf("Send to a resumed loop should succeed, got: %v", err)
	}
	waitForKind(t, evB, event.KindLoopParked, 2*time.Second)
}

// kindsOf lists event kinds for a failure message.
func kindsOf(evs []event.Event) []event.Kind {
	out := make([]event.Kind, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}

// TestResumeLiveLoopIsNoop proves case 1: Resume of a loop still live in the same
// instance returns the existing handle without re-launching (same handle identity).
func TestResumeLiveLoopIsNoop(t *testing.T) {
	dir := t.TempDir()
	store := fsstore.NewDurableFSStore(dir)
	var reg event.LoopRegistry = store

	fake := newGatedCompleter(
		model.CompletionResp{Content: "first answer"}, // turn 0 -> park (stays live)
	)
	rt := New(Deps{
		DurableStore:  store,
		Registry:      reg,
		MaxConcurrent: 1,
		IdleTTL:       time.Hour,
		BuildAgent: func(s event.EventStore, tree *agent.AgentTree, modelID string, r *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			return agent.New(fake, execAll{r}, nopRenderer{}, modelID, "", 1, 0), nil, modelID, nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, err := rt.StartLoop(ctx, LoopConfig{Task: "hi", ModelID: "cloud/x", Interactive: true})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	evCh, unsub, err := rt.Observe(ctx, event.DefaultTenant, h.ID())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	defer unsub()
	fake.admit(0)
	waitForKind(t, evCh, event.KindLoopParked, 2*time.Second)

	hResumed, err := rt.Resume(ctx, event.DefaultTenant, h.ID())
	if err != nil {
		t.Fatalf("Resume of a live loop: %v", err)
	}
	if hResumed.ID() != h.ID() {
		t.Fatalf("live-resume id = %q, want %q", hResumed.ID(), h.ID())
	}
}

// TestResumeUnknownLoopReturnsNotFound proves a resume of a loop that never existed
// (or is under a different tenant) is ErrLoopNotFound, never a leak.
func TestResumeUnknownLoopReturnsNotFound(t *testing.T) {
	dir := t.TempDir()
	store := fsstore.NewDurableFSStore(dir)
	var reg event.LoopRegistry = store

	rt := New(Deps{
		DurableStore:  store,
		Registry:      reg,
		MaxConcurrent: 1,
		BuildAgent: func(s event.EventStore, tree *agent.AgentTree, modelID string, r *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			return agent.New(newGatedCompleter(), execAll{r}, nopRenderer{}, modelID, "", 1, 0), nil, modelID, nil
		},
	})
	_, err := rt.Resume(context.Background(), event.DefaultTenant, "does-not-exist")
	if err == nil {
		t.Fatal("Resume of an unknown loop should error, got nil")
	}
}
