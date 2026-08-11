package loopconnect

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/ethanhinson/fuse/internal/event"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/runtime"
)

// --- fakeRuntime: a scripted runtime.Runtime double (no live model) --------------

type fakeRuntime struct {
	startID  string
	startErr error

	sendErr error

	// recorded call args for tenant/threading assertions.
	sawStartCfg runtime.LoopConfig
	sawSendTn   event.TenantID
	sawSendLoop string
	sawSendIn   string

	// Observe/Attach hooks (used by later tasks).
	live      chan event.Event
	unsub     func()
	observeFn func(ctx context.Context, tn event.TenantID, loopID string) (<-chan event.Event, func(), error)
	attachFn  func(ctx context.Context, tn event.TenantID, loopID string, from event.Seq) ([]event.Event, error)
}

func (f *fakeRuntime) StartLoop(ctx context.Context, cfg runtime.LoopConfig) (runtime.LoopHandle, error) {
	f.sawStartCfg = cfg
	if f.startErr != nil {
		return nil, f.startErr
	}
	return fakeHandle{id: f.startID}, nil
}
func (f *fakeRuntime) Send(ctx context.Context, tenant event.TenantID, loopID, input string) error {
	f.sawSendTn, f.sawSendLoop, f.sawSendIn = tenant, loopID, input
	return f.sendErr
}
func (f *fakeRuntime) Spawn(ctx context.Context, loopID string, opts runtime.SpawnOpts) (runtime.SpawnHandle, error) {
	return nil, nil
}
func (f *fakeRuntime) Observe(ctx context.Context, tenant event.TenantID, loopID string) (<-chan event.Event, func(), error) {
	if f.observeFn != nil {
		return f.observeFn(ctx, tenant, loopID)
	}
	unsub := f.unsub
	if unsub == nil {
		unsub = func() {}
	}
	return f.live, unsub, nil
}
func (f *fakeRuntime) Attach(ctx context.Context, tenant event.TenantID, loopID string, from event.Seq) ([]event.Event, error) {
	if f.attachFn != nil {
		return f.attachFn(ctx, tenant, loopID, from)
	}
	return nil, nil
}

type fakeHandle struct{ id string }

func (h fakeHandle) ID() string                     { return h.id }
func (h fakeHandle) Wait() ([]model.Message, error) { return nil, nil }

// --- test harness: an in-process connect server over the handler ------------------

func newTestServer(t *testing.T, rt runtime.Runtime) loopv1connect.LoopServiceClient {
	t.Helper()
	path, h := loopv1connect.NewLoopServiceHandler(NewHandler(rt))
	mux := httptest.NewServer(h)
	t.Cleanup(mux.Close)
	_ = path
	return loopv1connect.NewLoopServiceClient(mux.Client(), mux.URL)
}

func TestStartLoopReturnsLoopID(t *testing.T) {
	rt := &fakeRuntime{startID: "loop-42"}
	client := newTestServer(t, rt)

	resp, err := client.StartLoop(context.Background(), connect.NewRequest(&loopv1.StartLoopRequest{
		Task: "do it", Model: "cloud/x", Tenant: "acme", Interactive: true,
	}))
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	if resp.Msg.LoopId != "loop-42" {
		t.Fatalf("loop_id = %q, want loop-42", resp.Msg.LoopId)
	}
	// The seam received the task/model/interactive from the request.
	if rt.sawStartCfg.Task != "do it" || rt.sawStartCfg.ModelID != "cloud/x" || !rt.sawStartCfg.Interactive {
		t.Fatalf("StartLoop cfg not threaded: %+v", rt.sawStartCfg)
	}
	// Tenant is threaded through as a typed field (pass-through key).
	if rt.sawStartCfg.Tenant != event.TenantID("acme") {
		t.Fatalf("tenant = %q, want acme", rt.sawStartCfg.Tenant)
	}
}

func TestSendSuccessThreadsTenant(t *testing.T) {
	rt := &fakeRuntime{}
	client := newTestServer(t, rt)

	_, err := client.Send(context.Background(), connect.NewRequest(&loopv1.SendRequest{
		LoopId: "loop-1", Input: "more", Tenant: "acme",
	}))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if rt.sawSendTn != event.TenantID("acme") || rt.sawSendLoop != "loop-1" || rt.sawSendIn != "more" {
		t.Fatalf("Send args not threaded: tn=%q loop=%q in=%q", rt.sawSendTn, rt.sawSendLoop, rt.sawSendIn)
	}
}

func TestSendErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want connect.Code
	}{
		{"not found", runtime.ErrLoopNotFound, connect.CodeNotFound},
		{"finished", runtime.ErrLoopFinished, connect.CodeFailedPrecondition},
		{"other", errors.New("boom"), connect.CodeInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := &fakeRuntime{sendErr: tc.err}
			client := newTestServer(t, rt)
			_, err := client.Send(context.Background(), connect.NewRequest(&loopv1.SendRequest{LoopId: "x", Input: "y"}))
			if got := connect.CodeOf(err); got != tc.want {
				t.Fatalf("Send err code = %v, want %v (err=%v)", got, tc.want, err)
			}
		})
	}
}

func TestStartLoopErrorMapping(t *testing.T) {
	rt := &fakeRuntime{startErr: errors.New("nope")}
	client := newTestServer(t, rt)
	_, err := client.StartLoop(context.Background(), connect.NewRequest(&loopv1.StartLoopRequest{Task: "t"}))
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Fatalf("StartLoop err code = %v, want Internal", got)
	}
}

// TestEventMapperRoundTrip proves the edge mappers keep internal/event transport-free
// and preserve every field including raw JSON payload and the timestamp.
func TestEventMapperRoundTrip(t *testing.T) {
	ts := time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC)
	in := event.Event{
		Seq: 7, TS: ts, NodeID: "n1", ParentID: "p1", Depth: 2, Turn: 3,
		Kind: event.KindToolResult, Payload: json.RawMessage(`{"ok":true}`),
	}
	pe := toProtoEvent(in)
	if pe.Seq != 7 || pe.NodeId != "n1" || pe.ParentId != "p1" || pe.Depth != 2 || pe.Turn != 3 || pe.Kind != string(event.KindToolResult) {
		t.Fatalf("toProtoEvent lost a scalar field: %+v", pe)
	}
	if string(pe.Payload) != `{"ok":true}` {
		t.Fatalf("payload = %q, want raw JSON", pe.Payload)
	}
	if !pe.Ts.AsTime().Equal(ts) {
		t.Fatalf("ts = %v, want %v", pe.Ts.AsTime(), ts)
	}
	back := fromProtoEvent(pe)
	if back.Seq != in.Seq || back.NodeID != in.NodeID || back.Kind != in.Kind || string(back.Payload) != string(in.Payload) || !back.TS.Equal(in.TS) {
		t.Fatalf("round-trip lost a field: %+v vs %+v", back, in)
	}
}
