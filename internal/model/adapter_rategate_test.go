package model

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
)

// fakeGate records how Adapter.Complete drives a RateGate. It is shared with the
// adapter (which may call it from the request goroutine in concurrent tests), so
// every field access — getter and setter — holds mu (learning #10).
type fakeGate struct {
	mu sync.Mutex

	waitCalls   int
	waitCancel  context.Context // last ctx handed to Wait
	waitTokens  int
	lastWaitPvr string
	waitErr     error // if set, Wait returns it (and dispatch must not happen)

	reportCalls   int
	reportIn      int
	reportOut     int
	lastReportPvr string
}

func (g *fakeGate) Wait(ctx context.Context, provider string, estTokens int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.waitCalls++
	g.waitCancel = ctx
	g.waitTokens = estTokens
	g.lastWaitPvr = provider
	return g.waitErr
}

func (g *fakeGate) Report(provider string, inTokens, outTokens int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reportCalls++
	g.reportIn = inTokens
	g.reportOut = outTokens
	g.lastReportPvr = provider
}

func (g *fakeGate) snapshot() (waits, reports, in, out int, waitPvr, reportPvr string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.waitCalls, g.reportCalls, g.reportIn, g.reportOut, g.lastWaitPvr, g.lastReportPvr
}

func usageServer(t *testing.T, in, out int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],
			"usage":{"prompt_tokens":`+strconv.Itoa(in)+`,"completion_tokens":`+strconv.Itoa(out)+`}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRateGateConsultedBeforeDispatchAndReportedAfter is Acceptance 4's adapter
// half: the gate is asked to Wait once before dispatch and Report the reported
// usage once after a successful response, keyed by the request model.
func TestRateGateConsultedBeforeDispatchAndReportedAfter(t *testing.T) {
	srv := usageServer(t, 111, 222)
	g := &fakeGate{}
	a := NewAdapter(srv.URL, "k", srv.Client()).WithRateGate(g)

	_, err := a.Complete(context.Background(), CompletionReq{
		Model:    "cloud/kimi-k3",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waits, reports, in, out, waitPvr, reportPvr := g.snapshot()
	if waits != 1 {
		t.Errorf("Wait calls = %d, want 1", waits)
	}
	// The adapter cannot cheaply/accurately estimate tokens before dispatch, so it
	// passes estTokens=0 and lets Report reconcile against the gateway's actuals.
	g.mu.Lock()
	estTokens := g.waitTokens
	g.mu.Unlock()
	if estTokens != 0 {
		t.Errorf("Wait estTokens = %d, want 0 (adapter under-charges, Report reconciles)", estTokens)
	}
	if reports != 1 {
		t.Errorf("Report calls = %d, want 1", reports)
	}
	if in != 111 || out != 222 {
		t.Errorf("reported usage = (%d,%d), want (111,222)", in, out)
	}
	if waitPvr != "cloud/kimi-k3" || reportPvr != "cloud/kimi-k3" {
		t.Errorf("provider keys = (%q,%q), want cloud/kimi-k3", waitPvr, reportPvr)
	}
}

// TestRateGateWaitErrorBlocksDispatch: if the gate refuses (e.g. ctx cancelled
// while blocked on the bucket), Complete returns that error and never dispatches.
func TestRateGateWaitErrorBlocksDispatch(t *testing.T) {
	var dispatched bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dispatched = true
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"x"}}]}`)
	}))
	defer srv.Close()

	sentinel := errors.New("gate refused")
	g := &fakeGate{waitErr: sentinel}
	a := NewAdapter(srv.URL, "k", srv.Client()).WithRateGate(g)

	_, err := a.Complete(context.Background(), CompletionReq{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
	if dispatched {
		t.Error("dispatched despite gate refusal")
	}
	waits, reports, _, _, _, _ := g.snapshot()
	if waits != 1 {
		t.Errorf("Wait calls = %d, want 1", waits)
	}
	if reports != 0 {
		t.Errorf("Report calls = %d, want 0 (no success)", reports)
	}
}

// TestRateGateNotReportedOnFailure: a failed dispatch reports no usage (nothing
// to reconcile), even though Wait already charged the request.
func TestRateGateNotReportedOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, "nope")
	}))
	defer srv.Close()

	g := &fakeGate{}
	a := NewAdapter(srv.URL, "k", srv.Client()).WithRateGate(g)
	a.MaxAttempts = 1

	_, err := a.Complete(context.Background(), CompletionReq{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("expected error")
	}
	waits, reports, _, _, _, _ := g.snapshot()
	if waits != 1 {
		t.Errorf("Wait calls = %d, want 1", waits)
	}
	if reports != 0 {
		t.Errorf("Report calls = %d, want 0 on failure", reports)
	}
}

// TestRateGateChargedOncePerCompleteNotPerAttempt: retries of one logical request
// must not re-charge the request bucket. The gateway fails twice (retryable) then
// succeeds; Wait is still called exactly once.
func TestRateGateChargedOncePerCompleteNotPerAttempt(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, "retry me")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],
			"usage":{"prompt_tokens":5,"completion_tokens":7}}`)
	}))
	defer srv.Close()

	g := &fakeGate{}
	a := NewAdapter(srv.URL, "k", srv.Client()).WithRateGate(g)
	a.MaxAttempts = 3
	a.RetryBackoff = 0

	_, err := a.Complete(context.Background(), CompletionReq{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	waits, reports, in, out, _, _ := g.snapshot()
	if waits != 1 {
		t.Errorf("Wait calls = %d, want 1 (once per Complete, not per attempt)", waits)
	}
	if reports != 1 || in != 5 || out != 7 {
		t.Errorf("Report = (%d calls, in=%d out=%d), want (1, 5, 7)", reports, in, out)
	}
}

// TestNilRateGateNoInteraction: the default (nil) gate is the fast path — Complete
// works with no gate present at all. (Absence of a gate is verified structurally:
// there is nothing to call.)
func TestNilRateGateNoInteraction(t *testing.T) {
	srv := usageServer(t, 1, 1)
	a := NewAdapter(srv.URL, "k", srv.Client()) // no WithRateGate
	if a.gate != nil {
		t.Fatal("default adapter must have a nil gate (fast path)")
	}
	if _, err := a.Complete(context.Background(), CompletionReq{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatal(err)
	}
}

// TestWithRateGateDoesNotMutateReceiver keeps the copy-on-configure idiom honest:
// decorating a base adapter leaves the base's gate nil.
func TestWithRateGateDoesNotMutateReceiver(t *testing.T) {
	base := NewAdapter("http://x", "k", http.DefaultClient)
	_ = base.WithRateGate(&fakeGate{})
	if base.gate != nil {
		t.Error("WithRateGate mutated the receiver")
	}
}
