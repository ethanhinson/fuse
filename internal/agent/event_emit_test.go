package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
)

// recordingStore is an in-memory EventStore that captures every appended event
// for assertions. It satisfies event.EventStore.
type recordingStore struct {
	mu     sync.Mutex
	events []event.Event
	seq    event.Seq
}

func (r *recordingStore) Append(e event.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	e.Seq = r.seq
	r.events = append(r.events, e)
	return nil
}
func (r *recordingStore) Subscribe() (<-chan event.Event, func()) {
	ch := make(chan event.Event)
	close(ch)
	return ch, func() {}
}
func (r *recordingStore) Replay(from event.Seq) ([]event.Event, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []event.Event
	for _, e := range r.events {
		if e.Seq > from {
			out = append(out, e)
		}
	}
	return out, nil
}
func (r *recordingStore) kinds() []event.Kind {
	r.mu.Lock()
	defer r.mu.Unlock()
	ks := make([]event.Kind, len(r.events))
	for i, e := range r.events {
		ks[i] = e.Kind
	}
	return ks
}
func (r *recordingStore) first(k event.Kind) (event.Event, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e.Kind == k {
			return e, true
		}
	}
	return event.Event{}, false
}

func hasKind(ks []event.Kind, want event.Kind) bool {
	for _, k := range ks {
		if k == want {
			return true
		}
	}
	return false
}

// TestEmitPlainTurn: a no-tool-call turn emits turn.start, model.call.start,
// model.call.end, turn.end — in order — with a full assistant payload.
func TestEmitPlainTurn(t *testing.T) {
	comp := &scriptedCompleter{responses: []model.CompletionResp{
		{Content: "final answer", InputTokens: 11, OutputTokens: 22},
	}}
	rec := &recordingStore{}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 10, 100)
	a.SetEventSink(rec)
	a.SetNodeIdentity("node-1", "root", 1)

	if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatal(err)
	}
	got := rec.kinds()
	want := []event.Kind{event.KindTurnStart, event.KindModelCallStart, event.KindModelCallEnd, event.KindTurnEnd}
	if len(got) != len(want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", got, want)
		}
	}
	// Full model.call.end payload carries the complete response + usage.
	e, _ := rec.first(event.KindModelCallEnd)
	var pl event.ModelCallEndPayload
	if err := json.Unmarshal(e.Payload, &pl); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if pl.Content != "final answer" || pl.InputTokens != 11 || pl.OutputTokens != 22 {
		t.Errorf("model.call.end payload = %+v", pl)
	}
	// Node identity threaded onto every envelope.
	if e.NodeID != "node-1" || e.ParentID != "root" || e.Depth != 1 {
		t.Errorf("envelope identity = %q/%q/%d", e.NodeID, e.ParentID, e.Depth)
	}
}

// TestEmitToolTurn: a tool-call turn emits tool.call + tool.result with full args
// and full results between the model boundary and turn.end.
func TestEmitToolTurn(t *testing.T) {
	comp := &scriptedCompleter{responses: []model.CompletionResp{
		{ToolCalls: []model.ToolCall{{ID: "c1", Name: "read_file", Arguments: `{"path":"x"}`}}},
		{Content: "done"},
	}}
	rec := &recordingStore{}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 10, 100)
	a.SetEventSink(rec)

	if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "go"}}); err != nil {
		t.Fatal(err)
	}
	ks := rec.kinds()
	for _, want := range []event.Kind{event.KindToolCall, event.KindToolResult} {
		if !hasKind(ks, want) {
			t.Errorf("missing kind %q in %v", want, ks)
		}
	}
	// tool.call carries the full raw args.
	tc, _ := rec.first(event.KindToolCall)
	var tcp event.ToolCallPayload
	if err := json.Unmarshal(tc.Payload, &tcp); err != nil {
		t.Fatalf("tool.call payload: %v", err)
	}
	if tcp.Name != "read_file" || string(tcp.Args) != `{"path":"x"}` {
		t.Errorf("tool.call payload = %+v", tcp)
	}
	// tool.result carries the full output.
	tr, _ := rec.first(event.KindToolResult)
	var trp event.ToolResultPayload
	if err := json.Unmarshal(tr.Payload, &trp); err != nil {
		t.Fatalf("tool.result payload: %v", err)
	}
	if trp.Result != "ok" || trp.Name != "read_file" {
		t.Errorf("tool.result payload = %+v", trp)
	}
}

// errorCompleter always returns an error to drive the error-emission path.
type errorCompleter struct{}

func (errorCompleter) Complete(ctx context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	return model.CompletionResp{}, errors.New("gateway boom")
}

// TestEmitModelError: a model error emits an error event carrying the message.
func TestEmitModelError(t *testing.T) {
	rec := &recordingStore{}
	a := New(errorCompleter{}, &fakeExec{}, nopRenderer{}, "m", "", 10, 100)
	a.SetEventSink(rec)
	_, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Fatal("expected an error")
	}
	e, ok := rec.first(event.KindError)
	if !ok {
		t.Fatalf("no error event emitted; kinds = %v", rec.kinds())
	}
	var pl event.ErrorPayload
	if jerr := json.Unmarshal(e.Payload, &pl); jerr != nil {
		t.Fatalf("error payload: %v", jerr)
	}
	if pl.Err == "" {
		t.Error("error payload has empty Err")
	}
}

// TestEmitMaxTurns: exhausting the turn budget emits an error event with the
// max-turns message. A 1-turn cap with a tool-calling completer forces it.
func TestEmitMaxTurns(t *testing.T) {
	comp := &scriptedCompleter{responses: []model.CompletionResp{
		{ToolCalls: []model.ToolCall{{ID: "c1", Name: "read_file", Arguments: `{}`}}},
	}}
	rec := &recordingStore{}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 1, 100) // maxTurns = 1
	a.SetEventSink(rec)
	_, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "go"}})
	if !errors.Is(err, ErrMaxTurns) {
		t.Fatalf("err = %v, want ErrMaxTurns", err)
	}
	if !hasKind(rec.kinds(), event.KindError) {
		t.Errorf("no error event on max-turns; kinds = %v", rec.kinds())
	}
}

// TestEmitDefaultNoopNeverPanics: an Agent with no event sink installed (the New
// default) runs without panicking — emission is inert.
func TestEmitDefaultNoopNeverPanics(t *testing.T) {
	comp := &scriptedCompleter{responses: []model.CompletionResp{{Content: "ok"}}}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 10, 100)
	// no SetEventSink — default is event.NoopStore{}
	if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatal(err)
	}
}
