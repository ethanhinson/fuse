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

	"github.com/ethanhinson/fuse/harness"
	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
	"github.com/ethanhinson/fuse/internal/runtime"
	"github.com/ethanhinson/fuse/internal/tenantlimit"
)

// limitClock is a mutable clock shared by the limiter and the lease stamps.
type limitClock struct {
	mu sync.Mutex
	t  time.Time
}

func newLimitClock() *limitClock {
	return &limitClock{t: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *limitClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *limitClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// registeringRuntime models the runtime seam's Register-at-start: each
// successful StartLoop mints loop-<n> and records it live, with a lease, in the
// registry the limiter counts. startErr makes it fail without registering.
type registeringRuntime struct {
	*fakeRuntime
	reg   *fakeRegistry
	lease func() time.Time

	mu       sync.Mutex
	n        int
	starts   int
	startErr error
}

func newRegisteringRuntime(reg *fakeRegistry, clk *limitClock) *registeringRuntime {
	return &registeringRuntime{
		fakeRuntime: &fakeRuntime{},
		reg:         reg,
		lease:       func() time.Time { return clk.Now().Add(30 * time.Second) },
	}
}

func (r *registeringRuntime) StartLoop(ctx context.Context, cfg runtime.LoopConfig) (runtime.LoopHandle, error) {
	r.mu.Lock()
	r.starts++
	if r.startErr != nil {
		err := r.startErr
		r.mu.Unlock()
		return nil, err
	}
	r.n++
	id := fmt.Sprintf("loop-%d", r.n)
	r.mu.Unlock()
	if err := r.reg.Register(ctx, event.LoopRecord{
		Key:         event.StreamKey{Tenant: event.NormalizeTenant(cfg.Tenant), Loop: event.LoopID(id)},
		OwnerNodeID: "node",
		Live:        true,
		LeaseExpiry: r.lease(),
	}); err != nil {
		return nil, err
	}
	return fakeHandle{id: id}, nil
}

func (r *registeringRuntime) startCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.starts
}

func (r *registeringRuntime) setStartErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.startErr = err
}

// listErrRegistry is a registry whose List fails, so the live count cannot be read.
type listErrRegistry struct{ *fakeRegistry }

func (listErrRegistry) List(context.Context, event.TenantID) ([]event.LoopRecord, error) {
	return nil, errors.New("registry down")
}

func limitVerifier() loopauth.Verifier {
	return tokenVerifier{tokens: map[string]loopauth.Principal{
		"tok-a":   {Tenant: "a", Subject: "alice"},
		"tok-b":   {Tenant: "b", Subject: "bob"},
		"tok-big": {Tenant: "big", Subject: "bigco"},
	}}
}

// newLimitServer is newAuthzServer plus WithLimits.
func newLimitServer(t *testing.T, rt runtime.Runtime, reg event.LoopRegistry, lim *tenantlimit.Limiter, v loopauth.Verifier) loopv1connect.LoopServiceClient {
	t.Helper()
	h := NewHandler(rt).WithRegistry(reg).WithKeepalive(time.Hour).WithLimits(lim)
	_, handler := loopv1connect.NewLoopServiceHandler(h, connect.WithInterceptors(NewAuthInterceptor(v)))
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return loopv1connect.NewLoopServiceClient(srv.Client(), srv.URL)
}

func newTestLimiter(t *testing.T, cfg tenantlimit.Config, reg event.LoopRegistry, clk *limitClock) *tenantlimit.Limiter {
	t.Helper()
	lim, err := tenantlimit.New(cfg, tenantlimit.RegistryCounter(reg, clk.Now), clk.Now)
	if err != nil {
		t.Fatalf("tenantlimit.New: %v", err)
	}
	return lim
}

func startAs(client loopv1connect.LoopServiceClient, token string) (*connect.Response[loopv1.StartLoopResponse], error) {
	return client.StartLoop(context.Background(), bearer(connect.NewRequest(&loopv1.StartLoopRequest{Task: "t"}), token))
}

func mustStart(t *testing.T, client loopv1connect.LoopServiceClient, token string) string {
	t.Helper()
	resp, err := startAs(client, token)
	if err != nil {
		t.Fatalf("StartLoop as %s: %v", token, err)
	}
	return resp.Msg.LoopId
}

// refusal asserts err is ResourceExhausted naming the axis, and returns its
// Retry-After header value.
func refusal(t *testing.T, err error, axis string) string {
	t.Helper()
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("err = %v, want a connect error", err)
	}
	if cerr.Code() != connect.CodeResourceExhausted {
		t.Fatalf("code = %v, want ResourceExhausted (err=%v)", cerr.Code(), err)
	}
	if !strings.Contains(cerr.Message(), axis) {
		t.Fatalf("message %q does not name %s", cerr.Message(), axis)
	}
	return cerr.Meta().Get("Retry-After")
}

func TestStartLoopLiveCapRefusesThirdAndRecovers(t *testing.T) {
	clk := newLimitClock()
	reg := newFakeRegistry()
	rt := newRegisteringRuntime(reg, clk)
	lim := newTestLimiter(t, tenantlimit.Config{Default: tenantlimit.Limits{MaxLiveLoops: 2}}, reg, clk)
	client := newLimitServer(t, rt, reg, lim, limitVerifier())

	first := mustStart(t, client, "tok-a")
	mustStart(t, client, "tok-a")
	_, err := startAs(client, "tok-a")
	if ra := refusal(t, err, "max_live_loops"); ra != "" {
		t.Fatalf("cap refusal carries Retry-After %q, want none", ra)
	}
	mustStart(t, client, "tok-b")

	if err := reg.SetLive(context.Background(), event.StreamKey{Tenant: "a", Loop: event.LoopID(first)}, false, "node"); err != nil {
		t.Fatalf("SetLive: %v", err)
	}
	mustStart(t, client, "tok-a")
}

func TestStartLoopLiveCapRecoversOnLeaseExpiry(t *testing.T) {
	clk := newLimitClock()
	reg := newFakeRegistry()
	rt := newRegisteringRuntime(reg, clk)
	lim := newTestLimiter(t, tenantlimit.Config{Default: tenantlimit.Limits{MaxLiveLoops: 2}}, reg, clk)
	client := newLimitServer(t, rt, reg, lim, limitVerifier())

	mustStart(t, client, "tok-a")
	mustStart(t, client, "tok-a")
	_, err := startAs(client, "tok-a")
	refusal(t, err, "max_live_loops")

	clk.Advance(31 * time.Second) // past the 30s lease, no heartbeat
	mustStart(t, client, "tok-a")
}

func TestStartLoopRateRefusesWithRetryAfter(t *testing.T) {
	clk := newLimitClock()
	reg := newFakeRegistry()
	rt := newRegisteringRuntime(reg, clk)
	lim := newTestLimiter(t, tenantlimit.Config{Default: tenantlimit.Limits{StartsPerMinute: 3}}, reg, clk)
	client := newLimitServer(t, rt, reg, lim, limitVerifier())

	for i := 0; i < 3; i++ {
		mustStart(t, client, "tok-a")
	}
	_, err := startAs(client, "tok-a")
	if ra := refusal(t, err, "starts_per_minute"); ra != "20" {
		t.Fatalf("Retry-After = %q, want 20", ra)
	}
	clk.Advance(20 * time.Second)
	mustStart(t, client, "tok-a")
}

func TestStartLoopTenantOverride(t *testing.T) {
	clk := newLimitClock()
	reg := newFakeRegistry()
	rt := newRegisteringRuntime(reg, clk)
	lim := newTestLimiter(t, tenantlimit.Config{
		Default: tenantlimit.Limits{MaxLiveLoops: 2},
		Tenants: map[string]tenantlimit.Limits{"big": {MaxLiveLoops: 10}},
	}, reg, clk)
	client := newLimitServer(t, rt, reg, lim, limitVerifier())

	for i := 0; i < 10; i++ {
		mustStart(t, client, "tok-big")
	}
	_, err := startAs(client, "tok-big")
	refusal(t, err, "max_live_loops")

	mustStart(t, client, "tok-a")
	mustStart(t, client, "tok-a")
	_, err = startAs(client, "tok-a")
	refusal(t, err, "max_live_loops")
}

func TestStartLoopRefusalNeverReachesRuntime(t *testing.T) {
	clk := newLimitClock()
	reg := newFakeRegistry()
	rt := newRegisteringRuntime(reg, clk)
	lim := newTestLimiter(t, tenantlimit.Config{
		Default: tenantlimit.Limits{MaxLiveLoops: 1},
		Tenants: map[string]tenantlimit.Limits{"b": {StartsPerMinute: 1, MaxLiveLoops: 5}},
	}, reg, clk)
	client := newLimitServer(t, rt, reg, lim, limitVerifier())

	mustStart(t, client, "tok-a")
	before := rt.startCount()
	_, err := startAs(client, "tok-a")
	refusal(t, err, "max_live_loops")
	if got := rt.startCount(); got != before {
		t.Fatalf("cap refusal reached the runtime: starts %d -> %d", before, got)
	}

	mustStart(t, client, "tok-b")
	before = rt.startCount()
	_, err = startAs(client, "tok-b")
	refusal(t, err, "starts_per_minute")
	if got := rt.startCount(); got != before {
		t.Fatalf("rate refusal reached the runtime: starts %d -> %d", before, got)
	}
}

func TestStartLoopFailedStartReleasesReservation(t *testing.T) {
	clk := newLimitClock()
	reg := newFakeRegistry()
	rt := newRegisteringRuntime(reg, clk)
	rt.setStartErr(errors.New("construct failed"))
	lim := newTestLimiter(t, tenantlimit.Config{Default: tenantlimit.Limits{MaxLiveLoops: 1}}, reg, clk)
	client := newLimitServer(t, rt, reg, lim, limitVerifier())

	_, err := startAs(client, "tok-a")
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Fatalf("failed start code = %v, want Internal (err=%v)", got, err)
	}
	rt.setStartErr(nil)
	mustStart(t, client, "tok-a")
}

func TestStartLoopSpoofCheckedBeforeLimits(t *testing.T) {
	clk := newLimitClock()
	reg := newFakeRegistry()
	rt := newRegisteringRuntime(reg, clk)
	lim := newTestLimiter(t, tenantlimit.Config{Default: tenantlimit.Limits{StartsPerMinute: 1}}, reg, clk)
	client := newLimitServer(t, rt, reg, lim, limitVerifier())

	_, err := client.StartLoop(context.Background(), bearer(connect.NewRequest(&loopv1.StartLoopRequest{Task: "t", Tenant: "b"}), "tok-a"))
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("spoof code = %v, want PermissionDenied (err=%v)", got, err)
	}
	mustStart(t, client, "tok-a")
}

func TestStartLoopCountErrorIsUnavailable(t *testing.T) {
	clk := newLimitClock()
	base := newFakeRegistry()
	reg := listErrRegistry{base}
	rt := newRegisteringRuntime(base, clk)
	lim := newTestLimiter(t, tenantlimit.Config{Default: tenantlimit.Limits{MaxLiveLoops: 1}}, reg, clk)
	client := newLimitServer(t, rt, reg, lim, limitVerifier())

	_, err := startAs(client, "tok-a")
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Fatalf("count error code = %v, want Unavailable (err=%v)", got, err)
	}
	if got := rt.startCount(); got != 0 {
		t.Fatalf("runtime called %d times despite an unreadable count", got)
	}
}

func TestStartLoopNoLimiterUnchanged(t *testing.T) {
	clk := newLimitClock()
	reg := newFakeRegistry()
	rt := newRegisteringRuntime(reg, clk)
	client := newAuthzServerRT(t, rt, reg, limitVerifier())
	for i := 0; i < 20; i++ {
		mustStart(t, client, "tok-a")
	}
}

// newAuthzServerRT is newAuthzServer over any runtime and with no limiter.
func newAuthzServerRT(t *testing.T, rt runtime.Runtime, reg event.LoopRegistry, v loopauth.Verifier) loopv1connect.LoopServiceClient {
	t.Helper()
	h := NewHandler(rt).WithRegistry(reg).WithKeepalive(time.Hour)
	_, handler := loopv1connect.NewLoopServiceHandler(h, connect.WithInterceptors(NewAuthInterceptor(v)))
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return loopv1connect.NewLoopServiceClient(srv.Client(), srv.URL)
}

// limitError maps every Admit failure shape: refusals to ResourceExhausted
// (Retry-After only on the rate axis, rounded up to whole seconds), a caller
// that gave up to its own context code, and anything else to Internal.
func TestLimitErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		code       connect.Code
		retryAfter string
	}{
		{"live", &tenantlimit.RefusedError{Tenant: "a", Axis: tenantlimit.AxisLiveLoops, Limit: 2}, connect.CodeResourceExhausted, ""},
		{"rate", &tenantlimit.RefusedError{Tenant: "a", Axis: tenantlimit.AxisStartRate, Limit: 3, RetryAfter: 20 * time.Second}, connect.CodeResourceExhausted, "20"},
		{"rate fractional", &tenantlimit.RefusedError{Tenant: "a", Axis: tenantlimit.AxisStartRate, Limit: 7, RetryAfter: 1500 * time.Millisecond}, connect.CodeResourceExhausted, "2"},
		{"canceled", context.Canceled, connect.CodeCanceled, ""},
		{"deadline", fmt.Errorf("wrapped: %w", context.DeadlineExceeded), connect.CodeDeadlineExceeded, ""},
		{"count", fmt.Errorf("tenantlimit: count live loops for %q: %w", "a", errors.New("boom")), connect.CodeUnavailable, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cerr := limitError(tc.err)
			if cerr.Code() != tc.code {
				t.Fatalf("code = %v, want %v", cerr.Code(), tc.code)
			}
			if got := cerr.Meta().Get("Retry-After"); got != tc.retryAfter {
				t.Fatalf("Retry-After = %q, want %q", got, tc.retryAfter)
			}
			if !errors.Is(cerr, tc.err) {
				t.Fatalf("mapped error does not wrap %v", tc.err)
			}
		})
	}
}

// resumableRuntime is registeringRuntime plus the seam's resume behavior: Send
// to a loop the registry holds not-live fails ErrLoopFinished, and Resume marks
// it live again (as the real runtime's Resume re-registers the revived loop).
type resumableRuntime struct {
	*registeringRuntime

	mu      sync.Mutex
	resumes int
}

func (r *resumableRuntime) Send(ctx context.Context, tenant event.TenantID, loopID, _ string) error {
	rec, err := r.reg.Resolve(ctx, event.StreamKey{Tenant: tenant, Loop: event.LoopID(loopID)})
	if err != nil {
		return runtime.ErrLoopNotFound
	}
	if !rec.Live {
		return runtime.ErrLoopFinished
	}
	return nil
}

func (r *resumableRuntime) Resume(ctx context.Context, tenant event.TenantID, loopID string) (runtime.LoopHandle, error) {
	r.mu.Lock()
	r.resumes++
	r.mu.Unlock()
	if err := r.reg.SetLive(ctx, event.StreamKey{Tenant: tenant, Loop: event.LoopID(loopID)}, true, "node"); err != nil {
		return nil, err
	}
	return fakeHandle{id: loopID}, nil
}

func (r *resumableRuntime) resumeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.resumes
}

func sendAs(client loopv1connect.LoopServiceClient, token, loopID string) error {
	_, err := client.Send(context.Background(), bearer(connect.NewRequest(&loopv1.SendRequest{LoopId: loopID, Input: "hi"}), token))
	return err
}

// A Send that would resume a finished loop is a start for the live-loop cap: a
// tenant at its cap cannot exceed it by resuming, while a Send to a loop that is
// still live is never refused and takes no slot.
func TestSendResumeAdmittedByLiveCap(t *testing.T) {
	clk := newLimitClock()
	reg := newFakeRegistry()
	rt := &resumableRuntime{registeringRuntime: newRegisteringRuntime(reg, clk)}
	lim := newTestLimiter(t, tenantlimit.Config{Default: tenantlimit.Limits{MaxLiveLoops: 2}}, reg, clk)
	client := newLimitServer(t, rt, reg, lim, limitVerifier())

	reaped := mustStart(t, client, "tok-a")
	live := mustStart(t, client, "tok-a")
	if err := reg.SetLive(context.Background(), event.StreamKey{Tenant: "a", Loop: event.LoopID(reaped)}, false, "node"); err != nil {
		t.Fatalf("SetLive: %v", err)
	}
	other := mustStart(t, client, "tok-a") // back at the cap of 2 live

	refusal(t, sendAs(client, "tok-a", reaped), "max_live_loops")
	if got := rt.resumeCount(); got != 0 {
		t.Fatalf("refused resume reached the runtime: %d resumes", got)
	}

	// Sends to live loops are untouched by the cap, repeatedly.
	for i := 0; i < 3; i++ {
		if err := sendAs(client, "tok-a", live); err != nil {
			t.Fatalf("Send to live loop at cap: %v", err)
		}
	}

	// A freed slot lets the resume through.
	if err := reg.SetLive(context.Background(), event.StreamKey{Tenant: "a", Loop: event.LoopID(other)}, false, "node"); err != nil {
		t.Fatalf("SetLive: %v", err)
	}
	if err := sendAs(client, "tok-a", reaped); err != nil {
		t.Fatalf("Send resuming under the cap: %v", err)
	}
	if got := rt.resumeCount(); got != 1 {
		t.Fatalf("resumes = %d, want 1", got)
	}
}

// A request refused by scope or by the harness digest pin must not spend the
// tenant's start-rate token: with starts_per_minute: 1, the refusals leave the
// one token for a valid start, and only a second valid start is rate-limited.
func TestStartLoopScopeAndDigestRefusalsSpendNoStartToken(t *testing.T) {
	one, two := uint32(1), uint32(2)
	spec := &harnessv1.HarnessSpec{MaxTurns: &one}
	variant := &harnessv1.HarnessSpec{MaxTurns: &two}
	clk := newLimitClock()
	reg := newFakeRegistry()
	rt := newRegisteringRuntime(reg, clk)
	lim := newTestLimiter(t, tenantlimit.Config{Default: tenantlimit.Limits{StartsPerMinute: 1}}, reg, clk)
	v := tokenVerifier{tokens: map[string]loopauth.Principal{
		"pinned":   {Tenant: "a", Subject: "alice", HarnessDigest: harness.Digest(spec)},
		"observer": {Tenant: "a", Subject: "alice", Scopes: loopauth.NewScopes(loopauth.ScopeLoopObserve)},
		"unscoped": {Tenant: "a", Subject: "alice"},
	}}
	client := newLimitServer(t, rt, reg, lim, v)
	start := func(tok string, h *harnessv1.HarnessSpec, tenant string) error {
		_, err := client.StartLoop(context.Background(),
			bearer(connect.NewRequest(&loopv1.StartLoopRequest{Task: "t", Harness: h, Tenant: tenant}), tok))
		return err
	}

	if got := connect.CodeOf(start("observer", nil, "")); got != connect.CodePermissionDenied {
		t.Fatalf("scope refusal code = %v", got)
	}
	if got := connect.CodeOf(start("pinned", variant, "")); got != connect.CodePermissionDenied {
		t.Fatalf("digest refusal code = %v", got)
	}
	if got := connect.CodeOf(start("unscoped", nil, "b")); got != connect.CodePermissionDenied {
		t.Fatalf("tenant spoof code = %v", got)
	}
	if err := start("pinned", spec, ""); err != nil {
		t.Fatalf("valid start after refusals was not admitted: %v", err)
	}
	refusal(t, start("unscoped", nil, ""), "starts_per_minute")
}
