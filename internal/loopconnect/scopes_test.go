package loopconnect

import (
	"context"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
)

// scopeTokens are the per-test bearer tokens, all in tenant acme except where a
// test adds its own.
func scopeTokens() map[string]loopauth.Principal {
	return map[string]loopauth.Principal{
		"obs":       {Tenant: "acme", Subject: "alice", Scopes: loopauth.NewScopes(loopauth.ScopeLoopObserve)},
		"start":     {Tenant: "acme", Subject: "alice", Scopes: loopauth.NewScopes(loopauth.ScopeLoopStart)},
		"send":      {Tenant: "acme", Subject: "bob", Scopes: loopauth.NewScopes(loopauth.ScopeLoopSend)},
		"admin":     {Tenant: "acme", Subject: "bob", Scopes: loopauth.NewScopes(loopauth.ScopeLoopStart, loopauth.ScopeLoopAdmin)},
		"bob-start": {Tenant: "acme", Subject: "bob", Scopes: loopauth.NewScopes(loopauth.ScopeLoopStart)},
		"caps":      {Tenant: "acme", Subject: "alice", Scopes: loopauth.NewScopes(loopauth.ScopeCapabilitiesRead)},
		"none":      {Tenant: "acme", Subject: "alice"},
		"junk":      {Tenant: "acme", Subject: "alice", Scopes: loopauth.NewScopes("foo:bar")},
		"start-inline": {Tenant: "acme", Subject: "alice",
			Scopes: loopauth.NewScopes(loopauth.ScopeLoopStart, loopauth.ScopeSecretsInline)},
		"inline-only": {Tenant: "acme", Subject: "alice", Scopes: loopauth.NewScopes(loopauth.ScopeSecretsInline)},
	}
}

// scopeFixture is one authenticated server over a fake runtime, a registry and a
// capability source, counting the seam calls a refused request must never make.
type scopeFixture struct {
	rt           *fakeRuntime
	reg          *fakeRegistry
	caps         *fakeCaps
	client       loopv1connect.LoopServiceClient
	observeCalls int
}

func newScopeFixture(t *testing.T) *scopeFixture {
	t.Helper()
	f := &scopeFixture{reg: newFakeRegistry(), caps: &fakeCaps{}}
	f.rt = &fakeRuntime{startID: "loop-new"}
	f.rt.observeFn = func(context.Context, event.TenantID, string) (<-chan event.Event, func(), error) {
		f.observeCalls++
		return nil, func() {}, nil // a nil channel: the live tail idles until ctx ends
	}
	f.rt.attachFn = func(context.Context, event.TenantID, string, event.Seq) ([]event.Event, error) {
		return []event.Event{{Seq: 1, Kind: "x"}}, nil
	}
	// The seam's StartLoop registers the new loop; pre-seed it so recordOwner resolves.
	f.reg.seed("acme", "loop-new", "", "node-1")
	h := NewHandler(f.rt).WithRegistry(f.reg).WithCapabilities(f.caps).WithKeepalive(time.Hour)
	_, handler := loopv1connect.NewLoopServiceHandler(h,
		connect.WithInterceptors(NewAuthInterceptor(tokenVerifier{tokens: scopeTokens()})))
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	f.client = loopv1connect.NewLoopServiceClient(srv.Client(), srv.URL)
	return f
}

func (f *scopeFixture) start(token, orchestrate string) error {
	_, err := f.client.StartLoop(context.Background(), bearer(connect.NewRequest(&loopv1.StartLoopRequest{
		Task: "t", Tenant: "acme", OrchestrateLoopId: orchestrate,
	}), token))
	return err
}

// startWithSecrets is start carrying loop-scoped secret values.
func (f *scopeFixture) startWithSecrets(token string, ss []*loopv1.LoopSecret) error {
	_, err := f.client.StartLoop(context.Background(), bearer(connect.NewRequest(&loopv1.StartLoopRequest{
		Task: "t", Tenant: "acme", Secrets: ss,
	}), token))
	return err
}

func (f *scopeFixture) send(token, loop string) error {
	_, err := f.client.Send(context.Background(), bearer(connect.NewRequest(&loopv1.SendRequest{
		LoopId: loop, Input: "hi",
	}), token))
	return err
}

// observe opens the stream and reads the first frame. It returns nil when that
// frame arrived (the stream opened), else the stream error.
func (f *scopeFixture) observe(t *testing.T, token, loop string) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := f.client.Observe(ctx, bearer(connect.NewRequest(&loopv1.ObserveRequest{LoopId: loop}), token))
	if err != nil {
		return err
	}
	defer stream.Close()
	if stream.Receive() {
		return nil
	}
	if stream.Err() == nil {
		t.Fatal("Observe stream ended with no frame and no error")
	}
	return stream.Err()
}

func (f *scopeFixture) capabilities(token string) error {
	_, err := f.client.GetCapabilities(context.Background(), bearer(connect.NewRequest(&loopv1.GetCapabilitiesRequest{}), token))
	return err
}

func wantDenied(t *testing.T, what string, err error, scope string) {
	t.Helper()
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("%s: code = %v, want PermissionDenied (err=%v)", what, got, err)
	}
	if scope != "" && !strings.Contains(err.Error(), scope) {
		t.Fatalf("%s: error %q does not name scope %q", what, err, scope)
	}
}

func wantOK(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func TestScopeObserveOnlyToken(t *testing.T) {
	f := newScopeFixture(t)
	f.reg.seed("acme", "loop-a", "alice", "node-1")

	wantOK(t, "Observe(loop-a)", f.observe(t, "obs", "loop-a"))

	wantDenied(t, "StartLoop", f.start("obs", ""), loopauth.ScopeLoopStart)
	if f.rt.startCalls != 0 {
		t.Fatalf("seam StartLoop called %d times for a refused request", f.rt.startCalls)
	}
	wantDenied(t, "Send(loop-a)", f.send("obs", "loop-a"), loopauth.ScopeLoopSend)
	if f.rt.sawSendLoop != "" {
		t.Fatalf("seam Send reached for a refused request (loop %q)", f.rt.sawSendLoop)
	}
}

func TestScopeUnrestrictedTokenUnchanged(t *testing.T) {
	f := newScopeFixture(t)
	f.reg.seed("acme", "loop-a", "alice", "node-1")
	f.reg.seed("acme", "loop-b", "bob", "node-1")

	wantOK(t, "StartLoop", f.start("none", ""))
	wantOK(t, "Send(own)", f.send("none", "loop-a"))
	wantOK(t, "Observe(own)", f.observe(t, "none", "loop-a"))
	wantOK(t, "GetCapabilities", f.capabilities("none"))
	// An unrestricted token never gains loop:admin: the owner rule still applies.
	wantDenied(t, "Send(bob's loop)", f.send("none", "loop-b"), "")
}

func TestScopeAdminActsOnOtherSubjectsLoop(t *testing.T) {
	f := newScopeFixture(t)
	f.reg.seed("acme", "loop-a", "alice", "node-1")

	wantOK(t, "admin Send(alice's loop)", f.send("admin", "loop-a"))
	wantOK(t, "admin Observe(alice's loop)", f.observe(t, "admin", "loop-a"))
	wantDenied(t, "loop:send-only Send(alice's loop)", f.send("send", "loop-a"), "")
}

func TestScopeAdminCrossTenantStillNotFound(t *testing.T) {
	f := newScopeFixture(t)
	f.reg.seed("globex", "loop-g", "carol", "node-1")

	if got := connect.CodeOf(f.send("admin", "loop-g")); got != connect.CodeNotFound {
		t.Fatalf("admin Send(cross-tenant) code = %v, want NotFound", got)
	}
	if got := connect.CodeOf(f.observe(t, "admin", "loop-g")); got != connect.CodeNotFound {
		t.Fatalf("admin Observe(cross-tenant) code = %v, want NotFound", got)
	}
}

func TestScopeAdminOrchestrate(t *testing.T) {
	f := newScopeFixture(t)
	f.reg.seed("acme", "loop-a", "alice", "node-1")

	wantOK(t, "admin StartLoop{orchestrate alice's loop}", f.start("admin", "loop-a"))
	if f.rt.startCalls != 1 {
		t.Fatalf("seam StartLoop calls = %d, want 1", f.rt.startCalls)
	}
	wantDenied(t, "loop:start-only StartLoop{orchestrate alice's loop}", f.start("bob-start", "loop-a"), "")
	if f.rt.startCalls != 1 {
		t.Fatalf("seam StartLoop reached for a refused orchestrate (calls = %d)", f.rt.startCalls)
	}
}

func TestScopeCapabilities(t *testing.T) {
	f := newScopeFixture(t)

	wantOK(t, "caps GetCapabilities", f.capabilities("caps"))
	wantDenied(t, "obs GetCapabilities", f.capabilities("obs"), loopauth.ScopeCapabilitiesRead)
	if f.caps.calls != 1 {
		t.Fatalf("capability source calls = %d, want 1 (the refused request must not reach it)", f.caps.calls)
	}
}

func TestScopeUnknownOnlyRestrictsEverything(t *testing.T) {
	f := newScopeFixture(t)
	f.reg.seed("acme", "loop-a", "alice", "node-1")

	wantDenied(t, "StartLoop", f.start("junk", ""), loopauth.ScopeLoopStart)
	wantDenied(t, "Send", f.send("junk", "loop-a"), loopauth.ScopeLoopSend)
	wantDenied(t, "Observe", f.observe(t, "junk", "loop-a"), loopauth.ScopeLoopObserve)
	wantDenied(t, "GetCapabilities", f.capabilities("junk"), loopauth.ScopeCapabilitiesRead)
	if f.rt.startCalls != 0 || f.observeCalls != 0 || f.caps.calls != 0 || f.rt.sawSendLoop != "" {
		t.Fatalf("a refused request reached the seam: start=%d observe=%d caps=%d send=%q",
			f.rt.startCalls, f.observeCalls, f.caps.calls, f.rt.sawSendLoop)
	}
}

// TestScopeRefusedBeforeTenantCheck pins the check order: the scope check runs
// before the tenant/spoof check, so a token lacking the scope is refused for the
// scope even when its wire tenant would also be a spoof.
func TestScopeRefusedBeforeTenantCheck(t *testing.T) {
	f := newScopeFixture(t)
	_, err := f.client.StartLoop(context.Background(), bearer(connect.NewRequest(&loopv1.StartLoopRequest{
		Task: "t", Tenant: "evilcorp",
	}), "obs"))
	wantDenied(t, "StartLoop(spoofed tenant)", err, loopauth.ScopeLoopStart)
}

func TestScopeNoPrincipalPathUnchanged(t *testing.T) {
	rt := &fakeRuntime{startID: "loop-1"}
	resp, err := newTestServer(t, rt).StartLoop(context.Background(), connect.NewRequest(&loopv1.StartLoopRequest{Task: "t"}))
	if err != nil {
		t.Fatalf("StartLoop without an interceptor: %v", err)
	}
	if resp.Msg.LoopId != "loop-1" || rt.startCalls != 1 {
		t.Fatalf("loop id = %q, seam calls = %d; want loop-1 and 1", resp.Msg.LoopId, rt.startCalls)
	}
}

func TestStartLoopThreadsPrincipalRecord(t *testing.T) {
	cases := []struct {
		name   string
		p      loopauth.Principal
		scopes []string
	}{
		{"scoped", loopauth.Principal{Tenant: "acme", Subject: "alice", ClientID: "client-1", Issuer: "cms", TokenID: "x",
			Scopes: loopauth.NewScopes("loop:start", "loop:observe")}, []string{"loop:observe", "loop:start"}},
		{"unrestricted", loopauth.Principal{Tenant: "acme", Subject: "alice", ClientID: "client-1", Issuer: "cms", TokenID: "x"}, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := &fakeRuntime{startID: "loop-1"}
			h := NewHandler(rt)
			_, handler := loopv1connect.NewLoopServiceHandler(h,
				connect.WithInterceptors(NewAuthInterceptor(tokenVerifier{tokens: map[string]loopauth.Principal{"tok": tc.p}})))
			srv := httptest.NewServer(handler)
			t.Cleanup(srv.Close)
			client := loopv1connect.NewLoopServiceClient(srv.Client(), srv.URL)
			if _, err := client.StartLoop(context.Background(), bearer(connect.NewRequest(&loopv1.StartLoopRequest{Task: "t", Tenant: "acme"}), "tok")); err != nil {
				t.Fatalf("StartLoop: %v", err)
			}
			want := &event.LoopPrincipalPayload{Subject: "alice", ClientID: "client-1", Issuer: "cms", Scopes: tc.scopes, TokenID: "x"}
			got := rt.sawStartCfg.Principal
			if !reflect.DeepEqual(got, want) || got.Scopes == nil {
				t.Fatalf("Principal = %+v, want %+v", got, want)
			}
		})
	}
}

func TestStartLoopNoPrincipalNoRecord(t *testing.T) {
	rt := &fakeRuntime{startID: "loop-1"}
	if _, err := newTestServer(t, rt).StartLoop(context.Background(), connect.NewRequest(&loopv1.StartLoopRequest{Task: "t"})); err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	if rt.sawStartCfg.Principal != nil {
		t.Fatalf("Principal = %+v, want nil without an interceptor", rt.sawStartCfg.Principal)
	}
}
