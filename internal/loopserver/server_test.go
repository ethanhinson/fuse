package loopserver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/runtime"
)

// fakeRuntime is a test double satisfying runtime.Runtime. It records the last
// StartLoop/Send arguments and returns scripted Attach/Observe results.
type fakeRuntime struct {
	startCfg  runtime.LoopConfig
	startID   string
	startErr  error

	sendLoopID string
	sendInput  string
	sendErr    error

	attachHist []event.Event
	attachErr  error

	observeCh   chan event.Event
	observeErr  error
	observeStop bool
}

func (f *fakeRuntime) StartLoop(ctx context.Context, cfg runtime.LoopConfig) (runtime.LoopHandle, error) {
	f.startCfg = cfg
	if f.startErr != nil {
		return nil, f.startErr
	}
	return runtimeHandle{id: f.startID}, nil
}

func (f *fakeRuntime) Send(ctx context.Context, loopID, input string) error {
	f.sendLoopID = loopID
	f.sendInput = input
	return f.sendErr
}

func (f *fakeRuntime) Spawn(ctx context.Context, loopID string, opts runtime.SpawnOpts) (runtime.SpawnHandle, error) {
	return nil, nil
}

func (f *fakeRuntime) Observe(loopID string) (<-chan event.Event, func(), error) {
	if f.observeErr != nil {
		return nil, nil, f.observeErr
	}
	return f.observeCh, func() { f.observeStop = true }, nil
}

func (f *fakeRuntime) Attach(loopID string, from event.Seq) ([]event.Event, error) {
	return f.attachHist, f.attachErr
}

// runtimeHandle is the concrete handle returned by fakeRuntime; it must match
// runtime.LoopHandle exactly (Wait returns []model.Message).
type runtimeHandle struct{ id string }

func (h runtimeHandle) ID() string                     { return h.id }
func (h runtimeHandle) Wait() ([]model.Message, error) { return nil, nil }

func TestDispatchLoopStartReturnsLoopID(t *testing.T) {
	fr := &fakeRuntime{startID: "loop-abc"}
	s := NewServer(nil, nil, fr)

	r := req{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "loop.start", Params: json.RawMessage(`{"task":"hi","model":"cloud/x"}`)}
	got := s.dispatch(context.Background(), r)

	if got.Error != nil {
		t.Fatalf("unexpected error: %+v", got.Error)
	}
	var res startResult
	if err := json.Unmarshal(got.Result, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if res.LoopID != "loop-abc" {
		t.Fatalf("loop_id = %q, want loop-abc", res.LoopID)
	}
	if fr.startCfg.Task != "hi" || fr.startCfg.ModelID != "cloud/x" {
		t.Fatalf("StartLoop cfg = %+v, want Task=hi ModelID=cloud/x", fr.startCfg)
	}
}

func TestDispatchLoopSendCallsRuntime(t *testing.T) {
	fr := &fakeRuntime{}
	s := NewServer(nil, nil, fr)

	r := req{JSONRPC: "2.0", ID: json.RawMessage(`2`), Method: "loop.send", Params: json.RawMessage(`{"loop_id":"loop-abc","input":"more"}`)}
	got := s.dispatch(context.Background(), r)

	if got.Error != nil {
		t.Fatalf("unexpected error: %+v", got.Error)
	}
	if fr.sendLoopID != "loop-abc" || fr.sendInput != "more" {
		t.Fatalf("Send args = (%q,%q), want (loop-abc,more)", fr.sendLoopID, fr.sendInput)
	}
}

func TestUnknownMethod(t *testing.T) {
	s := NewServer(nil, nil, &fakeRuntime{})
	r := req{JSONRPC: "2.0", ID: json.RawMessage(`3`), Method: "loop.nope"}
	got := s.dispatch(context.Background(), r)
	if got.Error == nil || got.Error.Code != codeMethodNotFound {
		t.Fatalf("got %+v, want method-not-found", got.Error)
	}
}

func TestLoopStartBadParams(t *testing.T) {
	s := NewServer(nil, nil, &fakeRuntime{})
	r := req{JSONRPC: "2.0", ID: json.RawMessage(`4`), Method: "loop.start", Params: json.RawMessage(`{`)}
	got := s.dispatch(context.Background(), r)
	if got.Error == nil || got.Error.Code != codeInvalidParams {
		t.Fatalf("got %+v, want invalid-params", got.Error)
	}
}
