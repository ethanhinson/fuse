package runtime

import (
	"context"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/observe"
	"github.com/ethanhinson/fuse/internal/tools"
)

type observedOperation struct {
	kind observe.OperationKind
	name string
}

type recordingObserver struct {
	mu  sync.Mutex
	ops []observedOperation
}

func (o *recordingObserver) Start(ctx context.Context, d observe.Descriptor) (context.Context, observe.Handle) {
	o.mu.Lock()
	o.ops = append(o.ops, observedOperation{kind: d.Kind, name: d.Name})
	o.mu.Unlock()
	return ctx, observe.NoopHandle{}
}

func (o *recordingObserver) saw(kind observe.OperationKind, name string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, op := range o.ops {
		if op.kind == kind && op.name == name {
			return true
		}
	}
	return false
}

// scriptedCompleter returns queued responses in order, then a default "done".
type scriptedCompleter struct {
	responses []model.CompletionResp
	i         int
}

func (s *scriptedCompleter) Complete(ctx context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	if s.i >= len(s.responses) {
		return model.CompletionResp{Content: "done"}, nil
	}
	r := s.responses[s.i]
	s.i++
	return r, nil
}

// execAll is a ToolExecutor over a tools.Registry (matches agent.ToolExecutor).
type execAll struct{ reg *tools.Registry }

func (e execAll) Schemas() []model.ToolSchema { return e.reg.Schemas() }
func (e execAll) Execute(ctx context.Context, name, args string) tools.Result {
	return e.reg.Execute(ctx, name, args)
}

// nopRenderer is a no-op agent.Renderer.
type nopRenderer struct{}

func (nopRenderer) Assistant(string)                {}
func (nopRenderer) ToolCall(string, string)         {}
func (nopRenderer) ToolResult(string, tools.Result) {}
func (nopRenderer) Errorf(string, ...any)           {}
func (nopRenderer) Tokens(int, int)                 {}

func hasKind(evs []event.Event, want event.Kind) bool {
	for _, e := range evs {
		if e.Kind == want {
			return true
		}
	}
	return false
}

// TestStartLoopRunsAndEmits proves a loop runs end to end through the seam and
// produces a real fsstore event stream verifiable via Replay.
func TestStartLoopRunsAndEmits(t *testing.T) {
	fake := &scriptedCompleter{responses: []model.CompletionResp{
		{Content: "final answer"},
	}}
	baseDir := t.TempDir()
	rt := New(Deps{
		BaseDir:       baseDir,
		MaxConcurrent: 1,
		BuildAgent: func(store event.EventStore, tree *agent.AgentTree, modelID string, reg *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			return agent.New(fake, execAll{reg}, nopRenderer{}, modelID, "", 1, 0), nil, modelID, nil
		},
	})

	h, err := rt.StartLoop(context.Background(), LoopConfig{Task: "hi", ModelID: "cloud/x"})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	if h.ID() == "" {
		t.Fatal("handle ID() is empty")
	}
	if _, err := h.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	// The per-loop fsstore should carry a turn.start then turn.end via Replay(0).
	// Open the store independently under baseDir/<rootID> (Attach is Task 3).
	store, err := fsstore.NewFSEventStore(baseDir, h.ID())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	evs, err := store.Replay(0)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if !hasKind(evs, event.KindTurnStart) {
		t.Errorf("no turn.start in %v", evs)
	}
	if !hasKind(evs, event.KindTurnEnd) {
		t.Errorf("no turn.end in %v", evs)
	}
}

func TestDurableSinkObservesStoreAndPubSubOperations(t *testing.T) {
	store := fsstore.NewDurableFSStore(t.TempDir())
	observer := &recordingObserver{}
	sink := &durableSink{
		store:    store,
		key:      event.StreamKey{Tenant: "acme", Loop: "loop-a"},
		observer: observer,
	}
	if err := sink.Append(event.Event{Kind: event.KindTurnStart}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	ch, cancel := sink.Subscribe()
	cancel()
	if ch == nil {
		t.Fatal("Subscribe returned nil channel")
	}
	if _, err := sink.Replay(0); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	for _, want := range []observedOperation{
		{kind: observe.OperationStore, name: "append"},
		{kind: observe.OperationPubSub, name: "subscribe"},
		{kind: observe.OperationStore, name: "replay"},
	} {
		if !observer.saw(want.kind, want.name) {
			t.Errorf("missing observation %s.%s", want.kind, want.name)
		}
	}
}
