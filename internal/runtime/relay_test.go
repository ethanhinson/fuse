package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/tools"
)

// twoReplicas builds two runtimes ("replicas") over ONE durable store and registry, the
// shape of a deployment behind a load balancer. Loops live in the replica that started
// them; the other replica has an empty loop map and sees them only through the store.
func twoReplicas(t *testing.T, f *steerFake, nowB func() time.Time) (a, b Runtime, store *fsstore.FSDurableStore) {
	t.Helper()
	store = fsstore.NewDurableFSStore(t.TempDir())
	mk := func(now func() time.Time) Runtime {
		return New(Deps{
			DurableStore:  store,
			Registry:      store,
			MaxConcurrent: 2,
			RelayPoll:     20 * time.Millisecond,
			Now:           now,
			BuildAgent: func(s event.EventStore, tree *agent.AgentTree, modelID string, reg *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
				return agent.New(f.completer(modelID), execAll{reg}, nopRenderer{}, modelID, "", 1, 0), nil, modelID, nil
			},
		})
	}
	return mk(nil), mk(nowB), store
}

// A steer sent to the replica that does NOT run the loop reaches the loop, interrupt
// included, and the delivery is recorded so it is not delivered twice.
func TestSteerRelaysToTheOwningReplica(t *testing.T) {
	f := &steerFake{targetBlock: make(chan struct{})}
	rtA, rtB, store := twoReplicas(t, f, nil)
	ctx := context.Background()

	h, err := rtA.StartLoop(ctx, LoopConfig{Task: "build it", ModelID: "target", Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	<-f.targetBlock // A's first model call is in flight

	if err := rtB.(Steerer).Steer(ctx, event.DefaultTenant, h.ID(), "switch to plan B",
		SteerOpts{Source: agent.SourceOrchestrator, Interrupt: true}); err != nil {
		t.Fatalf("Steer on the other replica: %v", err)
	}
	eventually(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.targetReqs) >= 2
	})
	f.mu.Lock()
	got := lastUser(f.targetReqs[1])
	f.mu.Unlock()
	if !strings.Contains(got, "[orchestrator guidance]") || !strings.Contains(got, "switch to plan B") {
		t.Fatalf("owner saw %q", got)
	}

	// The stream records the request and, on delivery, which request it satisfied.
	key := event.StreamKey{Tenant: event.DefaultTenant, Loop: event.LoopID(h.ID())}
	var reqSeq event.Seq
	eventually(t, func() bool {
		evs, _ := store.Replay(ctx, key, 0)
		reqSeq = 0
		delivered := false
		for _, e := range evs {
			if e.Kind == event.KindSteerRequest {
				reqSeq = e.Seq
			}
			if e.Kind == event.KindUserInput {
				var p event.UserInputPayload
				_ = json.Unmarshal(e.Payload, &p)
				for _, s := range p.ReqSeqs {
					if reqSeq != 0 && s == reqSeq {
						delivered = true
					}
				}
			}
		}
		return reqSeq != 0 && delivered
	})

	// Not delivered again: the owner stays parked, so no further model call happens.
	time.Sleep(300 * time.Millisecond)
	f.mu.Lock()
	n := len(f.targetReqs)
	f.mu.Unlock()
	if n != 2 {
		t.Fatalf("the request was delivered again (%d model calls)", n)
	}
}

// A plain Send is relayed the same way.
func TestSendRelaysToTheOwningReplica(t *testing.T) {
	f := &steerFake{}
	rtA, rtB, _ := twoReplicas(t, f, nil)
	ctx := context.Background()
	h, err := rtA.StartLoop(ctx, LoopConfig{Task: "chat", ModelID: "target", Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.targetReqs) >= 1 })

	if err := rtB.Send(ctx, event.DefaultTenant, h.ID(), "and one more thing"); err != nil {
		t.Fatalf("Send on the other replica: %v", err)
	}
	eventually(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.targetReqs) >= 2 && strings.Contains(lastUser(f.targetReqs[len(f.targetReqs)-1]), "and one more thing")
	})
}

// An orchestrator on one replica steers a loop on another: the whole point of running
// more than one replica.
func TestOrchestratorSteersAcrossReplicas(t *testing.T) {
	f := &steerFake{}
	rtA, rtB, _ := twoReplicas(t, f, nil)
	ctx := context.Background()
	target, err := rtA.StartLoop(ctx, LoopConfig{Task: "build it", ModelID: "target", Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.targetReqs) >= 1 })

	args, _ := json.Marshal(map[string]any{"message": "run the linter"})
	f.mu.Lock()
	f.orchSteps = nil
	f.mu.Unlock()
	f.mu.Lock()
	f.orchSteps = append(f.orchSteps,
		modelToolCall("c1", "steer_agent", string(args)),
		modelText("told it"))
	f.mu.Unlock()

	// The orchestrator runs on replica B; the target runs on replica A.
	if _, err := rtB.StartLoop(ctx, LoopConfig{Task: "keep it honest", ModelID: "orch", Orchestrate: target.ID()}); err != nil {
		t.Fatalf("StartLoop(Orchestrate) on the other replica: %v", err)
	}
	eventually(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.targetReqs) >= 2 && strings.Contains(lastUser(f.targetReqs[len(f.targetReqs)-1]), "run the linter")
	})
}

// A loop whose owner stopped renewing its lease is abandoned. A message to it is
// reported as finished (so the edge can resume it) and not queued into a stream nobody
// reads.
func TestSendToAnAbandonedLoopReportsFinished(t *testing.T) {
	f := &steerFake{}
	future := func() time.Time { return time.Now().Add(time.Hour) }
	rtA, rtB, store := twoReplicas(t, f, future)
	ctx := context.Background()
	h, err := rtA.StartLoop(ctx, LoopConfig{Task: "chat", ModelID: "target", Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.targetReqs) >= 1 })

	err = rtB.Send(ctx, event.DefaultTenant, h.ID(), "anyone there?")
	if !errors.Is(err, ErrLoopFinished) {
		t.Fatalf("Send to an abandoned loop = %v, want ErrLoopFinished", err)
	}
	key := event.StreamKey{Tenant: event.DefaultTenant, Loop: event.LoopID(h.ID())}
	evs, _ := store.Replay(ctx, key, 0)
	for _, e := range evs {
		if e.Kind == event.KindSteerRequest {
			t.Fatal("a request was queued for an abandoned loop")
		}
	}
}

// pendingRequests picks the requests no delivery has accounted for: what a restarted
// owner must redeliver, and nothing else.
func TestPendingRequests(t *testing.T) {
	mk := func(seq event.Seq, k event.Kind, payload any) event.Event {
		raw, _ := json.Marshal(payload)
		return event.Event{Seq: seq, Kind: k, Payload: raw}
	}
	evs := []event.Event{
		mk(1, event.KindUserInput, event.UserInputPayload{Content: "task"}),
		mk(2, event.KindSteerRequest, event.SteerRequestPayload{Content: "a"}),
		mk(3, event.KindSteerRequest, event.SteerRequestPayload{Content: "b"}),
		mk(4, event.KindUserInput, event.UserInputPayload{Content: "[human message]\na", ReqSeqs: []event.Seq{2}}),
		mk(5, event.KindSteerRequest, event.SteerRequestPayload{Content: "c"}),
	}
	got := pendingRequests(evs)
	var seqs []event.Seq
	for _, e := range got {
		seqs = append(seqs, e.Seq)
	}
	if len(seqs) != 2 || seqs[0] != 3 || seqs[1] != 5 {
		t.Fatalf("pending = %v, want [3 5]", seqs)
	}
}

// Clients observe before they send. Observing an abandoned loop must not make it look
// alive again: the live failure was a Send accepted into a stream nobody reads.
func TestObserveDoesNotRevivePhantomLease(t *testing.T) {
	f := &steerFake{}
	future := func() time.Time { return time.Now().Add(time.Hour) }
	rtA, rtB, _ := twoReplicas(t, f, future)
	ctx := context.Background()
	h, err := rtA.StartLoop(ctx, LoopConfig{Task: "chat", ModelID: "target", Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.targetReqs) >= 1 })

	_, unsub, err := rtB.Observe(ctx, event.DefaultTenant, h.ID())
	if err != nil {
		t.Fatalf("Observe of an abandoned loop must still work: %v", err)
	}
	defer unsub()
	if err := rtB.Send(ctx, event.DefaultTenant, h.ID(), "hello?"); !errors.Is(err, ErrLoopFinished) {
		t.Fatalf("Send after Observe = %v, want ErrLoopFinished so the edge resumes the loop", err)
	}
}

// A clean scale-down (SIGTERM) must hand its loops off at once: after Shutdown the loops
// are recorded not live, so the next message - to any replica - sees ErrLoopFinished and
// the edge resumes the loop, instead of relaying to a replica that no longer exists.
func TestShutdownHandsLoopsOff(t *testing.T) {
	f := &steerFake{}
	rtA, rtB, store := twoReplicas(t, f, nil)
	ctx := context.Background()
	h, err := rtA.StartLoop(ctx, LoopConfig{Task: "chat", ModelID: "target", Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.targetReqs) >= 1 })

	sd, ok := rtA.(Shutdowner)
	if !ok {
		t.Fatal("the in-process runtime must implement Shutdowner")
	}
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := sd.Shutdown(sctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	key := event.StreamKey{Tenant: event.DefaultTenant, Loop: event.LoopID(h.ID())}
	rec, err := store.Resolve(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Live {
		t.Fatal("a shut-down loop must be recorded not live")
	}
	if err := rtB.Send(ctx, event.DefaultTenant, h.ID(), "still there?"); !errors.Is(err, ErrLoopFinished) {
		t.Fatalf("Send after the owner shut down = %v, want ErrLoopFinished", err)
	}
}

// Shutdown with nothing running returns at once.
func TestShutdownWithNoLoops(t *testing.T) {
	f := &steerFake{}
	rtA, _, _ := twoReplicas(t, f, nil)
	if err := rtA.(Shutdowner).Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
