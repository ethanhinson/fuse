package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
	observeotel "github.com/ethanhinson/fuse/internal/observe/otel"
	"go.opentelemetry.io/otel/sdk/trace"
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

// TestEmitPersistsActiveTraceCarrier proves the durable event stream retains the
// safe, repository-owned context required to causally reconnect later work.
func TestEmitPersistsActiveTraceCarrier(t *testing.T) {
	tp := trace.NewTracerProvider()
	defer tp.Shutdown(context.Background())
	ctx, span := tp.Tracer("test").Start(context.Background(), "loop")
	defer span.End()

	rec := &recordingStore{}
	a := New(&scriptedCompleter{responses: []model.CompletionResp{{Content: "done"}}}, &fakeExec{}, nopRenderer{}, "m", "", 10, 100)
	a.SetObserver(observeotel.New(tp))
	a.SetEventSink(rec)
	if _, err := a.Run(ctx, []model.Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.events) == 0 {
		t.Fatal("no emitted events")
	}
	for _, e := range rec.events {
		if e.Trace == nil || e.Trace.Validate() != nil {
			t.Fatalf("event %q trace = %#v, want validated carrier", e.Kind, e.Trace)
		}
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

// TestEmitSummarize: a Tier-2 summarization pass emits a context.summarize event
// carrying the turn span and token accounting (coverage for the summarize kind).
func TestEmitSummarize(t *testing.T) {
	comp := &scriptedCompleter{responses: []model.CompletionResp{{Content: "done"}}}
	rec := &recordingStore{}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 10, 100)
	a.ContextWindow = 4000
	a.SetEventSink(rec)
	sc := &countingSummarizerCompleter{summary: "Objective: read files\nNext: continue"}
	a.SetSummarizer(newSummarizer(sc, "m", 2000), nil)

	if _, err := a.Run(context.Background(), overBudgetHistory()); err != nil {
		t.Fatalf("run err = %v", err)
	}
	e, ok := rec.first(event.KindSummarize)
	if !ok {
		t.Fatalf("no context.summarize event; kinds = %v", rec.kinds())
	}
	var pl event.SummarizePayload
	if err := json.Unmarshal(e.Payload, &pl); err != nil {
		t.Fatalf("summarize payload: %v", err)
	}
	if pl.TokensBefore == 0 {
		t.Errorf("summarize payload missing token accounting: %+v", pl)
	}
}

// TestEmitLoopTrip: the doom-loop detector firing emits a loop.detector.trip
// event (coverage for the loop-trip kind). Three byte-identical tool-call
// responses trip the loopLimit=3 detector; with no LoopApproval hook the loop
// aborts, so we also see the error event.
func TestEmitLoopTrip(t *testing.T) {
	call := model.CompletionResp{ToolCalls: []model.ToolCall{{ID: "c", Name: "bash", Arguments: `{"cmd":"ls"}`}}}
	comp := &scriptedCompleter{responses: []model.CompletionResp{call, call, call, call}}
	rec := &recordingStore{}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 20, 100)
	a.SetEventSink(rec)
	_, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "go"}})
	if !errors.Is(err, ErrLoopDetected) {
		t.Fatalf("err = %v, want ErrLoopDetected", err)
	}
	if !hasKind(rec.kinds(), event.KindLoopTrip) {
		t.Errorf("no loop.detector.trip event; kinds = %v", rec.kinds())
	}
}

// TestEmitReturnResultTurn: the structured-delegation (return_result) terminal
// path is symmetric with every other turn — it emits turn.end, and the synthetic
// return_result call emits tool.call + tool.result (review finding, change 0043).
func TestEmitReturnResultTurn(t *testing.T) {
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"name": map[string]any{"type": "string"}},
		"required":   []any{"name"},
	}
	comp := &scriptedCompleter{responses: []model.CompletionResp{
		{ToolCalls: []model.ToolCall{{ID: "1", Name: "return_result", Arguments: `{"name":"Ada"}`}}},
	}}
	rec := &recordingStore{}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 10, 100)
	var sink ExpectsSink
	a.SetExpects(schema, &sink)
	a.SetEventSink(rec)

	if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "go"}}); err != nil {
		t.Fatalf("run: %v", err)
	}
	ks := rec.kinds()
	// The terminal return_result turn must still close with turn.end...
	if !hasKind(ks, event.KindTurnEnd) {
		t.Errorf("return_result terminal path did not emit turn.end; kinds = %v", ks)
	}
	// ...and the synthetic return_result call is emitted as tool.call + tool.result.
	if !hasKind(ks, event.KindToolCall) || !hasKind(ks, event.KindToolResult) {
		t.Errorf("synthetic return_result call not emitted as tool.call/tool.result; kinds = %v", ks)
	}
	// The tool.result for return_result carries its output.
	tr, ok := rec.first(event.KindToolResult)
	if !ok {
		t.Fatal("no tool.result event")
	}
	var trp event.ToolResultPayload
	if err := json.Unmarshal(tr.Payload, &trp); err != nil {
		t.Fatalf("tool.result payload: %v", err)
	}
	if trp.Name != "return_result" {
		t.Errorf("tool.result name = %q, want return_result", trp.Name)
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
