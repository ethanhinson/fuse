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
	kind   observe.OperationKind
	name   string
	fields []observe.Field
}

// recordedEnd captures a single Handle.End call against a started operation.
type recordedEnd struct {
	name    string
	outcome observe.Outcome
	fields  []observe.Field
}

// recordingHandle is a recording Handle returned to callers of
// recordingObserver.Start. Ends are recorded back onto the
// owning recordingObserver under its mutex, since the handle is shared with
// the run goroutine (learnings: mutex-test-double-concurrent-provider — lock
// both sides).
type recordingHandle struct {
	owner *recordingObserver
	name  string
}

func (h recordingHandle) End(outcome observe.Outcome, fields ...observe.Field) {
	h.owner.mu.Lock()
	h.owner.ends = append(h.owner.ends, recordedEnd{name: h.name, outcome: outcome, fields: fields})
	h.owner.mu.Unlock()
}

type recordingObserver struct {
	mu   sync.Mutex
	ops  []observedOperation
	ends []recordedEnd
}

func (o *recordingObserver) Start(ctx context.Context, d observe.Descriptor) (context.Context, observe.Handle) {
	o.mu.Lock()
	o.ops = append(o.ops, observedOperation{kind: d.Kind, name: d.Name, fields: d.Fields})
	o.mu.Unlock()
	return ctx, recordingHandle{owner: o, name: d.Name}
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

// opsNamed returns every recorded operation with the given name, in recorded order.
func (o *recordingObserver) opsNamed(name string) []observedOperation {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []observedOperation
	for _, op := range o.ops {
		if op.name == name {
			out = append(out, op)
		}
	}
	return out
}

// fieldValue returns the value of key on the given operation and whether it
// was present.
func fieldValue(op observedOperation, key string) (string, bool) {
	for _, f := range op.fields {
		if f.Key == key {
			return f.Value, true
		}
	}
	return "", false
}

// endsFor returns every recorded End call for operations started with the
// given name, in recorded order.
func (o *recordingObserver) endsFor(name string) []recordedEnd {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []recordedEnd
	for _, e := range o.ends {
		if e.name == name {
			out = append(out, e)
		}
	}
	return out
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
		BuildAgent: func(store event.EventStore, tree *agent.AgentTree, modelID string, reg *tools.Registry) (*agent.Agent, error) {
			return agent.New(fake, execAll{reg}, nopRenderer{}, modelID, "", 1, 0), nil
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
	// Open the store independently under baseDir/<rootID>.
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
