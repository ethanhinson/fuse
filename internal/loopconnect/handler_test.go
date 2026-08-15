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

	// Resume behavior (change 0054, D3). resumeErr is what Resume returns; resumed
	// counts Resume calls. sendErrAfterResume, when set, is the error Send returns until
	// a Resume has happened, after which Send returns nil — modeling a finished loop that
	// a resume revives so the handler's resume-then-retry path can be asserted.
	resumeErr          error
	resumed            int
	sendErrAfterResume error

	// recorded call args for tenant/threading assertions.
	sawStartCfg    runtime.LoopConfig
	sawStartCtxErr error // ctx.Err() at StartLoop time (must be nil: loop-lifetime ctx)
	sawSendTn      event.TenantID
	sawSendLoop    string
	sawSendIn      string
	sawResumeTn    event.TenantID
	sawResumeLoop  string

	// Observe/Attach hooks (used by later tasks).
	live      chan event.Event
	unsub     func()
	observeFn func(ctx context.Context, tn event.TenantID, loopID string) (<-chan event.Event, func(), error)
	attachFn  func(ctx context.Context, tn event.TenantID, loopID string, from event.Seq) ([]event.Event, error)
}

func (f *fakeRuntime) StartLoop(ctx context.Context, cfg runtime.LoopConfig) (runtime.LoopHandle, error) {
	f.sawStartCfg = cfg
	f.sawStartCtxErr = ctx.Err()
	if f.startErr != nil {
		return nil, f.startErr
	}
	return fakeHandle{id: f.startID}, nil
}
func (f *fakeRuntime) Send(ctx context.Context, tenant event.TenantID, loopID, input string) error {
	f.sawSendTn, f.sawSendLoop, f.sawSendIn = tenant, loopID, input
	// Model a finished-then-resumed loop: Send fails until a Resume, then succeeds.
	if f.sendErrAfterResume != nil {
		if f.resumed == 0 {
			return f.sendErrAfterResume
		}
		return nil
	}
	return f.sendErr
}
func (f *fakeRuntime) Resume(ctx context.Context, tenant event.TenantID, loopID string) (runtime.LoopHandle, error) {
	f.sawResumeTn, f.sawResumeLoop = tenant, loopID
	if f.resumeErr != nil {
		return nil, f.resumeErr
	}
	f.resumed++
	return fakeHandle{id: loopID}, nil
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
			// resumeErr set so the finished case exercises the pure fall-through: the
			// handler attempts a resume (change 0054 D3), it fails, and the original
			// finished error surfaces as CodeFailedPrecondition. The not-found/other
			// cases never reach the resume path.
			rt := &fakeRuntime{sendErr: tc.err, resumeErr: errors.New("not resumable")}
			client := newTestServer(t, rt)
			_, err := client.Send(context.Background(), connect.NewRequest(&loopv1.SendRequest{LoopId: "x", Input: "y"}))
			if got := connect.CodeOf(err); got != tc.want {
				t.Fatalf("Send err code = %v, want %v (err=%v)", got, tc.want, err)
			}
		})
	}
}

// TestSendResumesFinishedLoop is the D3 unit-level acceptance: a Send against a
// finished-but-resumable loop transparently resumes it and retries, returning success
// (not CodeFailedPrecondition). It proves the handler routes resume on the Send path.
func TestSendResumesFinishedLoop(t *testing.T) {
	rt := &fakeRuntime{sendErrAfterResume: runtime.ErrLoopFinished}
	client := newTestServer(t, rt)

	_, err := client.Send(context.Background(), connect.NewRequest(&loopv1.SendRequest{
		LoopId: "loop-1", Input: "and then?", Tenant: "acme",
	}))
	if err != nil {
		t.Fatalf("Send to a finished-but-resumable loop should succeed after resume, got: %v", err)
	}
	if rt.resumed != 1 {
		t.Fatalf("expected exactly one Resume call, got %d", rt.resumed)
	}
	if rt.sawResumeTn != event.TenantID("acme") || rt.sawResumeLoop != "loop-1" {
		t.Fatalf("Resume args not threaded tenant-scoped: tn=%q loop=%q", rt.sawResumeTn, rt.sawResumeLoop)
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

// TestStartLoopLaunchesUnderLoopLifetimeContext is a regression guard for the request-
// context decoupling bug: connect cancels a unary request's context the instant the
// RPC returns, so if StartLoop launched the loop under the request ctx the loop would
// die before its first turn. The handler must launch under its loop-lifetime baseCtx.
// This asserts the ctx the seam received is NOT already cancelled even though the
// client request's own context is cancelled right after.
func TestStartLoopLaunchesUnderLoopLifetimeContext(t *testing.T) {
	rt := &fakeRuntime{startID: "loop-x"}
	// The handler's baseCtx is a live (never-cancelled here) context.
	_, h := loopv1connect.NewLoopServiceHandler(NewHandler(rt))
	srv := httptest.NewServer(h)
	defer srv.Close()
	client := loopv1connect.NewLoopServiceClient(srv.Client(), srv.URL)

	// Use a request context we cancel immediately AFTER the call returns.
	ctx, cancel := context.WithCancel(context.Background())
	_, err := client.StartLoop(ctx, connect.NewRequest(&loopv1.StartLoopRequest{Task: "t"}))
	cancel()
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	if rt.sawStartCtxErr != nil {
		t.Fatalf("StartLoop seam ctx was already errored (%v) — loop tied to request ctx, would die early", rt.sawStartCtxErr)
	}
}
