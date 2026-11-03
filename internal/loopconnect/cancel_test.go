package loopconnect

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
	"github.com/ethanhinson/fuse/internal/observe"
	observeotel "github.com/ethanhinson/fuse/internal/observe/otel"
	"github.com/ethanhinson/fuse/internal/runtime"
)

// cancelRuntime is a fakeRuntime that also implements runtime.Canceller. It
// records every Cancel call and answers with a configurable result.
type cancelRuntime struct {
	*fakeRuntime

	mu        sync.Mutex
	calls     int
	sawTenant event.TenantID
	sawLoop   string
	sawReason string
	sawBy     string
	result    runtime.CancelResult
	err       error
}

var _ runtime.Canceller = (*cancelRuntime)(nil)

func (c *cancelRuntime) Cancel(_ context.Context, tenant event.TenantID, loopID, reason, requestedBy string) (runtime.CancelResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.sawTenant, c.sawLoop, c.sawReason, c.sawBy = tenant, loopID, reason, requestedBy
	return c.result, c.err
}

func (c *cancelRuntime) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func newCancelRuntime() *cancelRuntime {
	return &cancelRuntime{
		fakeRuntime: &fakeRuntime{},
		result: runtime.CancelResult{
			Outcome: runtime.CancelOutcomeCancelled, WasLive: true, TerminalSeq: 42, Reason: "operator stop",
		},
	}
}

// newCancelServer stands up the handler over rt with an optional registry and
// auth verifier (both nil = the pure-transport path) and an optional observer.
func newCancelServer(t *testing.T, rt runtime.Runtime, reg event.LoopRegistry, verifier loopauth.Verifier, obs observe.Observer) loopv1connect.LoopServiceClient {
	t.Helper()
	h := NewHandler(rt).WithKeepalive(time.Hour).WithObserver(obs)
	if reg != nil {
		h = h.WithRegistry(reg)
	}
	var opts []connect.HandlerOption
	if verifier != nil {
		opts = append(opts, connect.WithInterceptors(NewAuthInterceptor(verifier)))
	}
	_, handler := loopv1connect.NewLoopServiceHandler(h, opts...)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return loopv1connect.NewLoopServiceClient(srv.Client(), srv.URL)
}

func cancelReq(loop, reason, token string) *connect.Request[loopv1.CancelLoopRequest] {
	return bearer(connect.NewRequest(&loopv1.CancelLoopRequest{LoopId: loop, Reason: reason}), token)
}

func cancelTokens() map[string]loopauth.Principal {
	return map[string]loopauth.Principal{
		"alice-cancel": {Tenant: "acme", Subject: "alice", Scopes: loopauth.NewScopes(loopauth.ScopeLoopCancel)},
		"alice-other": {Tenant: "acme", Subject: "alice",
			Scopes: loopauth.NewScopes(loopauth.ScopeLoopSend, loopauth.ScopeLoopObserve, loopauth.ScopeLoopStart)},
		"bob-admin":       {Tenant: "acme", Subject: "bob", Scopes: loopauth.NewScopes(loopauth.ScopeLoopAdmin)},
		"bob-cancel":      {Tenant: "acme", Subject: "bob", Scopes: loopauth.NewScopes(loopauth.ScopeLoopCancel)},
		"alice-unscoped":  {Tenant: "acme", Subject: "alice"},
		"carol-other-ten": {Tenant: "other", Subject: "carol"},
	}
}

func newCancelFixture(t *testing.T) (*cancelRuntime, loopv1connect.LoopServiceClient) {
	t.Helper()
	rt := newCancelRuntime()
	reg := newFakeRegistry()
	reg.seed("acme", "loop-1", "alice", "node-1")
	return rt, newCancelServer(t, rt, reg, tokenVerifier{tokens: cancelTokens()}, nil)
}

// TestCancelLoopScopes: loop:cancel cancels the caller's own loop; the other loop
// scopes do not imply it; loop:admin satisfies it and lifts the owner check; an
// unrestricted token cancels; loop:cancel alone does not lift the owner check.
func TestCancelLoopScopes(t *testing.T) {
	t.Run("loop:cancel cancels own loop", func(t *testing.T) {
		rt, client := newCancelFixture(t)
		resp, err := client.CancelLoop(context.Background(), cancelReq("loop-1", "operator stop", "alice-cancel"))
		if err != nil {
			t.Fatalf("CancelLoop: %v", err)
		}
		m := resp.Msg
		if m.Outcome != loopv1.CancelOutcome_CANCEL_OUTCOME_CANCELLED || !m.WasLive || m.TerminalSeq != 42 || m.Reason != "operator stop" {
			t.Fatalf("response = %+v, want CANCELLED was_live seq 42 reason %q", m, "operator stop")
		}
		rt.mu.Lock()
		defer rt.mu.Unlock()
		if rt.calls != 1 || rt.sawTenant != "acme" || rt.sawLoop != "loop-1" || rt.sawReason != "operator stop" || rt.sawBy != "alice" {
			t.Fatalf("runtime saw calls=%d tenant=%q loop=%q reason=%q by=%q", rt.calls, rt.sawTenant, rt.sawLoop, rt.sawReason, rt.sawBy)
		}
	})
	t.Run("other loop scopes do not imply loop:cancel", func(t *testing.T) {
		rt, client := newCancelFixture(t)
		_, err := client.CancelLoop(context.Background(), cancelReq("loop-1", "", "alice-other"))
		if connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "loop:cancel") {
			t.Fatalf("err = %v, want PermissionDenied naming loop:cancel", err)
		}
		if rt.callCount() != 0 {
			t.Fatal("runtime Cancel was called on a refused request")
		}
	})
	t.Run("loop:admin cancels another subject's loop", func(t *testing.T) {
		rt, client := newCancelFixture(t)
		if _, err := client.CancelLoop(context.Background(), cancelReq("loop-1", "", "bob-admin")); err != nil {
			t.Fatalf("admin CancelLoop: %v", err)
		}
		rt.mu.Lock()
		defer rt.mu.Unlock()
		if rt.calls != 1 || rt.sawBy != "bob" {
			t.Fatalf("runtime calls=%d by=%q, want 1 call by bob", rt.calls, rt.sawBy)
		}
	})
	t.Run("loop:cancel does not lift the owner check", func(t *testing.T) {
		rt, client := newCancelFixture(t)
		_, err := client.CancelLoop(context.Background(), cancelReq("loop-1", "", "bob-cancel"))
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("err = %v, want PermissionDenied (owner check)", err)
		}
		if rt.callCount() != 0 {
			t.Fatal("runtime Cancel was called on a refused request")
		}
	})
	t.Run("unrestricted token cancels", func(t *testing.T) {
		rt, client := newCancelFixture(t)
		if _, err := client.CancelLoop(context.Background(), cancelReq("loop-1", "", "alice-unscoped")); err != nil {
			t.Fatalf("unrestricted CancelLoop: %v", err)
		}
		if rt.callCount() != 1 {
			t.Fatalf("runtime calls = %d, want 1", rt.callCount())
		}
	})
}

// TestCancelLoopTenantIsolation: another tenant's loop is NotFound, and a wire
// tenant that does not match the principal's is a spoof (PermissionDenied).
func TestCancelLoopTenantIsolation(t *testing.T) {
	rt, client := newCancelFixture(t)
	_, err := client.CancelLoop(context.Background(), cancelReq("loop-1", "", "carol-other-ten"))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("cross-tenant err = %v, want NotFound", err)
	}
	spoof := bearer(connect.NewRequest(&loopv1.CancelLoopRequest{LoopId: "loop-1", Tenant: "globex"}), "alice-cancel")
	_, err = client.CancelLoop(context.Background(), spoof)
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("spoofed tenant err = %v, want PermissionDenied", err)
	}
	if rt.callCount() != 0 {
		t.Fatalf("runtime Cancel called %d times on refused requests", rt.callCount())
	}
}

// TestCancelLoopValidation: an empty loop_id and a reason that is too long, has
// a control character or is not UTF-8 are InvalidArgument; 256 bytes pass.
func TestCancelLoopValidation(t *testing.T) {
	refused := []struct {
		name, loop, reason string
	}{
		{"empty loop_id", "", "x"},
		{"257-byte reason", "loop-1", strings.Repeat("a", 257)},
		{"newline", "loop-1", "stop\nnow"},
		{"bell", "loop-1", "stop\x07"},
		{"tab", "loop-1", "stop\tnow"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			rt, client := newCancelFixture(t)
			_, err := client.CancelLoop(context.Background(), cancelReq(tc.loop, tc.reason, "alice-cancel"))
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Fatalf("err = %v, want InvalidArgument", err)
			}
			if rt.callCount() != 0 {
				t.Fatal("runtime Cancel was called on a refused request")
			}
		})
	}
	// proto3 refuses to marshal invalid UTF-8, so a client cannot send it over
	// the wire; call the handler directly to prove the edge refuses it anyway.
	t.Run("invalid utf-8", func(t *testing.T) {
		rt := newCancelRuntime()
		_, err := NewHandler(rt).CancelLoop(context.Background(),
			connect.NewRequest(&loopv1.CancelLoopRequest{LoopId: "loop-1", Reason: "stop\xff"}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("err = %v, want InvalidArgument", err)
		}
		if rt.callCount() != 0 {
			t.Fatal("runtime Cancel was called on a refused request")
		}
	})
	t.Run("256-byte reason passes", func(t *testing.T) {
		rt, client := newCancelFixture(t)
		reason := strings.Repeat("é", 128) // 256 bytes, multi-byte runes
		if _, err := client.CancelLoop(context.Background(), cancelReq("loop-1", reason, "alice-cancel")); err != nil {
			t.Fatalf("CancelLoop: %v", err)
		}
		if rt.callCount() != 1 {
			t.Fatalf("runtime calls = %d, want 1", rt.callCount())
		}
	})
	t.Run("empty reason passes", func(t *testing.T) {
		_, client := newCancelFixture(t)
		if _, err := client.CancelLoop(context.Background(), cancelReq("loop-1", "", "alice-cancel")); err != nil {
			t.Fatalf("CancelLoop: %v", err)
		}
	})
}

// TestCancelLoopUnimplemented: a runtime without the Canceller capability
// answers Unimplemented.
func TestCancelLoopUnimplemented(t *testing.T) {
	client := newTestServer(t, &fakeRuntime{})
	_, err := client.CancelLoop(context.Background(), connect.NewRequest(&loopv1.CancelLoopRequest{LoopId: "l"}))
	if got := connect.CodeOf(err); got != connect.CodeUnimplemented {
		t.Fatalf("CancelLoop code = %v, want Unimplemented (err %v)", got, err)
	}
}

// TestCancelLoopErrorMapping: runtime errors and outcomes map onto the wire.
func TestCancelLoopErrorMapping(t *testing.T) {
	errCases := []struct {
		name string
		err  error
		want connect.Code
	}{
		{"not found", fmt.Errorf("%w: %q", runtime.ErrLoopNotFound, "l"), connect.CodeNotFound},
		{"cancelled", fmt.Errorf("%w: %q", runtime.ErrLoopCancelled, "l"), connect.CodeFailedPrecondition},
		{"other", errors.New("store down"), connect.CodeInternal},
	}
	for _, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			rt := newCancelRuntime()
			rt.err = tc.err
			client := newTestServer(t, rt)
			_, err := client.CancelLoop(context.Background(), connect.NewRequest(&loopv1.CancelLoopRequest{LoopId: "l"}))
			if got := connect.CodeOf(err); got != tc.want {
				t.Fatalf("code = %v, want %v (err %v)", got, tc.want, err)
			}
		})
	}
	outcomes := []struct {
		in   runtime.CancelOutcome
		want loopv1.CancelOutcome
	}{
		{runtime.CancelOutcomeCancelled, loopv1.CancelOutcome_CANCEL_OUTCOME_CANCELLED},
		{runtime.CancelOutcomeAlreadyCancelled, loopv1.CancelOutcome_CANCEL_OUTCOME_ALREADY_CANCELLED},
		{runtime.CancelOutcomeCancelling, loopv1.CancelOutcome_CANCEL_OUTCOME_CANCELLING},
	}
	for _, tc := range outcomes {
		t.Run(tc.want.String(), func(t *testing.T) {
			rt := newCancelRuntime()
			rt.result = runtime.CancelResult{Outcome: tc.in, Reason: "first", TerminalSeq: 7}
			client := newTestServer(t, rt)
			resp, err := client.CancelLoop(context.Background(), connect.NewRequest(&loopv1.CancelLoopRequest{LoopId: "l", Reason: "second"}))
			if err != nil {
				t.Fatalf("CancelLoop: %v", err)
			}
			if resp.Msg.Outcome != tc.want || resp.Msg.Reason != "first" || resp.Msg.TerminalSeq != 7 || resp.Msg.WasLive {
				t.Fatalf("response = %+v, want outcome %v reason first seq 7 not live", resp.Msg, tc.want)
			}
		})
	}
}

// TestCancelLoopTraceHeader: CancelLoop opens a fuse.api.request span named
// cancel_loop and returns its trace id in Fuse-Trace-Id; a refused request ends
// that span with an error outcome.
func TestCancelLoopTraceHeader(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	client := newCancelServer(t, newCancelRuntime(), nil, nil, observeotel.New(provider))

	resp, err := client.CancelLoop(context.Background(), connect.NewRequest(&loopv1.CancelLoopRequest{LoopId: "l"}))
	if err != nil {
		t.Fatalf("CancelLoop: %v", err)
	}
	traceID := resp.Header().Get("Fuse-Trace-Id")
	if traceID == "" {
		t.Fatal("CancelLoop response lacks Fuse-Trace-Id")
	}
	spans := exporter.GetSpans()
	if len(spans) != 1 || spans[0].Name != "fuse.api.request.cancel_loop" || spans[0].SpanContext.TraceID().String() != traceID {
		t.Fatalf("spans = %+v, want one fuse.api.request.cancel_loop in trace %s", spans, traceID)
	}

	exporter.Reset()
	if _, err := client.CancelLoop(context.Background(), connect.NewRequest(&loopv1.CancelLoopRequest{})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("empty loop_id err = %v, want InvalidArgument", err)
	}
	spans = exporter.GetSpans()
	if len(spans) != 1 || spans[0].Status.Description != string(observe.OutcomeError) {
		t.Fatalf("refused spans = %+v, want one span with error outcome", spans)
	}
}

// TestSendCancelledLoopIsFailedPrecondition: Send to a cancelled loop is
// FailedPrecondition "loop cancelled" and never tries to resume it.
func TestSendCancelledLoopIsFailedPrecondition(t *testing.T) {
	rt := &fakeRuntime{sendErr: fmt.Errorf("%w: %q", runtime.ErrLoopCancelled, "l")}
	client := newTestServer(t, rt)
	_, err := client.Send(context.Background(), connect.NewRequest(&loopv1.SendRequest{LoopId: "l", Input: "x"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (err %v)", connect.CodeOf(err), err)
	}
	if msg := err.Error(); !strings.Contains(msg, "cancelled") || strings.Contains(msg, "finished") {
		t.Fatalf("message = %q, want it to say cancelled, not finished", msg)
	}
	if rt.resumed != 0 || rt.sawResumeLoop != "" {
		t.Fatalf("a cancelled loop was resumed (resumed=%d loop=%q)", rt.resumed, rt.sawResumeLoop)
	}
}

// TestSendFinishedThenResumeCancelledSurfacesCancelled: a loop this replica does
// not cache reads as finished; the resume retry learns it is cancelled, and Send
// surfaces that rather than "loop finished".
func TestSendFinishedThenResumeCancelledSurfacesCancelled(t *testing.T) {
	rt := &fakeRuntime{
		sendErr:   fmt.Errorf("%w: %q", runtime.ErrLoopFinished, "l"),
		resumeErr: fmt.Errorf("%w: %q", runtime.ErrLoopCancelled, "l"),
	}
	client := newTestServer(t, rt)
	_, err := client.Send(context.Background(), connect.NewRequest(&loopv1.SendRequest{LoopId: "l", Input: "x"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (err %v)", connect.CodeOf(err), err)
	}
	if msg := err.Error(); !strings.Contains(msg, "cancelled") || strings.Contains(msg, "finished") {
		t.Fatalf("message = %q, want it to say cancelled, not finished", msg)
	}
}

// TestStartLoopCancelledOrchestrateTargetIsFailedPrecondition: orchestrating a
// cancelled loop is a client error, not a server fault.
func TestStartLoopCancelledOrchestrateTargetIsFailedPrecondition(t *testing.T) {
	rt := &fakeRuntime{startErr: fmt.Errorf("runtime: orchestrate target: %w", runtime.ErrLoopCancelled)}
	client := newTestServer(t, rt)
	_, err := client.StartLoop(context.Background(), connect.NewRequest(&loopv1.StartLoopRequest{Task: "t", OrchestrateLoopId: "l"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (err %v)", connect.CodeOf(err), err)
	}
}
