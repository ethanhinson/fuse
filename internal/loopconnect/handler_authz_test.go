package loopconnect

import (
	"context"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"

	"net/http/httptest"
)

// --- fakeRegistry: a tiny in-memory event.LoopRegistry for edge-authz unit tests ---
//
// It records loop existence/ownership keyed by StreamKey and honors the same
// tenant-scoping and ErrLoopUnknown semantics the real backends do (a resolve under
// the wrong tenant misses because the key differs). It normalizes the tenant so an
// empty caller tenant and DefaultTenant address the same record, matching fsstore.
type fakeRegistry struct {
	mu   sync.Mutex
	recs map[event.StreamKey]event.LoopRecord
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{recs: map[event.StreamKey]event.LoopRecord{}}
}

func (r *fakeRegistry) norm(k event.StreamKey) event.StreamKey {
	return event.StreamKey{Tenant: event.NormalizeTenant(k.Tenant), Loop: k.Loop}
}

func (r *fakeRegistry) Register(_ context.Context, rec event.LoopRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := r.norm(rec.Key)
	now := time.Now().UTC()
	rec.Key = key
	if prev, ok := r.recs[key]; ok {
		rec.CreatedAt = prev.CreatedAt
	} else {
		rec.CreatedAt = now
	}
	rec.UpdatedAt = now
	r.recs[key] = rec
	return nil
}

func (r *fakeRegistry) SetLive(_ context.Context, key event.StreamKey, live bool, ownerNodeID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := r.norm(key)
	rec, ok := r.recs[k]
	if !ok {
		return event.ErrLoopUnknown
	}
	rec.Live = live
	rec.OwnerNodeID = ownerNodeID
	rec.UpdatedAt = time.Now().UTC()
	r.recs[k] = rec
	return nil
}

func (r *fakeRegistry) Heartbeat(_ context.Context, key event.StreamKey, ownerNodeID string, expiry time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := r.norm(key)
	rec, ok := r.recs[k]
	if !ok {
		return event.ErrLoopUnknown
	}
	rec.OwnerNodeID = ownerNodeID
	rec.LeaseExpiry = expiry
	rec.UpdatedAt = time.Now().UTC()
	r.recs[k] = rec
	return nil
}

func (r *fakeRegistry) Resolve(_ context.Context, key event.StreamKey) (event.LoopRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.recs[r.norm(key)]
	if !ok {
		return event.LoopRecord{}, event.ErrLoopUnknown
	}
	return rec, nil
}

func (r *fakeRegistry) List(_ context.Context, tenant event.TenantID) ([]event.LoopRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	nt := event.NormalizeTenant(tenant)
	var out []event.LoopRecord
	for k, rec := range r.recs {
		if k.Tenant == nt {
			out = append(out, rec)
		}
	}
	return out, nil
}

// seed pre-registers a loop record (as the runtime's StartLoop would, under
// StreamKey{tenant, loop}) so Send/Observe authz has something to resolve.
func (r *fakeRegistry) seed(tenant event.TenantID, loop, owner, ownerNode string) {
	_ = r.Register(context.Background(), event.LoopRecord{
		Key:         event.StreamKey{Tenant: tenant, Loop: event.LoopID(loop)},
		Owner:       owner,
		OwnerNodeID: ownerNode,
		Live:        true,
	})
}

// --- authz test harness: real auth interceptor + handler with a registry ----------

// tokenVerifier maps a fixed bearer token to a principal for the interceptor path.
type tokenVerifier struct{ tokens map[string]loopauth.Principal }

func (v tokenVerifier) Verify(_ context.Context, token string) (loopauth.Principal, error) {
	p, ok := v.tokens[token]
	if !ok {
		return loopauth.Principal{}, loopauth.ErrInvalidToken
	}
	return p, nil
}

// newAuthzServer stands up an in-process Connect server whose handler carries a
// registry AND is wrapped by the real auth interceptor, so a principal is stashed on
// the request ctx exactly as production does. Returns a client; callers set the
// bearer token per request.
func newAuthzServer(t *testing.T, rt *fakeRuntime, reg event.LoopRegistry, verifier loopauth.Verifier) loopv1connect.LoopServiceClient {
	t.Helper()
	h := NewHandler(rt).WithRegistry(reg).WithKeepalive(time.Hour)
	_, handler := loopv1connect.NewLoopServiceHandler(h, connect.WithInterceptors(NewAuthInterceptor(verifier)))
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return loopv1connect.NewLoopServiceClient(srv.Client(), srv.URL)
}

func bearer[T any](req *connect.Request[T], token string) *connect.Request[T] {
	req.Header().Set("Authorization", "Bearer "+token)
	return req
}

// TestStartLoopRecordsOwnerFromPrincipal proves the edge writes the authenticated
// subject as the loop's Owner after the seam creates it.
func TestStartLoopRecordsOwnerFromPrincipal(t *testing.T) {
	reg := newFakeRegistry()
	// The runtime's StartLoop would have Registered the loop under the principal's
	// tenant with OwnerNodeID + Live (no Owner yet); pre-seed that state.
	reg.seed("acme", "loop-9", "", "node-1")
	rt := &fakeRuntime{startID: "loop-9"}
	verifier := tokenVerifier{tokens: map[string]loopauth.Principal{
		"tok-alice": {Tenant: "acme", Subject: "alice"},
	}}
	client := newAuthzServer(t, rt, reg, verifier)

	_, err := client.StartLoop(context.Background(), bearer(connect.NewRequest(&loopv1.StartLoopRequest{
		Task: "t", Tenant: "acme",
	}), "tok-alice"))
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	// The seam was called with the PRINCIPAL's tenant.
	if rt.sawStartCfg.Tenant != event.TenantID("acme") {
		t.Fatalf("seam tenant = %q, want acme", rt.sawStartCfg.Tenant)
	}
	rec, rerr := reg.Resolve(context.Background(), event.StreamKey{Tenant: "acme", Loop: "loop-9"})
	if rerr != nil {
		t.Fatalf("Resolve: %v", rerr)
	}
	if rec.Owner != "alice" {
		t.Fatalf("Owner = %q, want alice", rec.Owner)
	}
	// Owner-set must NOT clobber OwnerNodeID/Live the seam recorded.
	if rec.OwnerNodeID != "node-1" || !rec.Live {
		t.Fatalf("Owner-set clobbered seam fields: OwnerNodeID=%q Live=%v", rec.OwnerNodeID, rec.Live)
	}
}

// TestStartLoopTenantSpoofDenied: a wire tenant that disagrees with the principal is a
// spoof → PermissionDenied, and the seam is never called.
func TestStartLoopTenantSpoofDenied(t *testing.T) {
	reg := newFakeRegistry()
	rt := &fakeRuntime{startID: "loop-x"}
	verifier := tokenVerifier{tokens: map[string]loopauth.Principal{
		"tok-alice": {Tenant: "acme", Subject: "alice"},
	}}
	client := newAuthzServer(t, rt, reg, verifier)

	_, err := client.StartLoop(context.Background(), bearer(connect.NewRequest(&loopv1.StartLoopRequest{
		Task: "t", Tenant: "evilcorp",
	}), "tok-alice"))
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("code = %v, want PermissionDenied (err=%v)", got, err)
	}
	if rt.sawStartCfg.Task != "" {
		t.Fatal("seam StartLoop was called despite tenant spoof")
	}
}

// TestSendSameOwnerSucceeds: the loop's owner Sends and the seam is reached with the
// principal's tenant.
func TestSendSameOwnerSucceeds(t *testing.T) {
	reg := newFakeRegistry()
	reg.seed("acme", "loop-1", "alice", "node-1")
	rt := &fakeRuntime{}
	verifier := tokenVerifier{tokens: map[string]loopauth.Principal{
		"tok-alice": {Tenant: "acme", Subject: "alice"},
	}}
	client := newAuthzServer(t, rt, reg, verifier)

	_, err := client.Send(context.Background(), bearer(connect.NewRequest(&loopv1.SendRequest{
		LoopId: "loop-1", Input: "hi", Tenant: "acme",
	}), "tok-alice"))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if rt.sawSendTn != event.TenantID("acme") || rt.sawSendLoop != "loop-1" || rt.sawSendIn != "hi" {
		t.Fatalf("seam args not threaded: tn=%q loop=%q in=%q", rt.sawSendTn, rt.sawSendLoop, rt.sawSendIn)
	}
}

// TestSendCrossOwnerSameTenantDenied: a different subject in the SAME tenant is denied.
func TestSendCrossOwnerSameTenantDenied(t *testing.T) {
	reg := newFakeRegistry()
	reg.seed("acme", "loop-1", "alice", "node-1")
	rt := &fakeRuntime{}
	verifier := tokenVerifier{tokens: map[string]loopauth.Principal{
		"tok-bob": {Tenant: "acme", Subject: "bob"},
	}}
	client := newAuthzServer(t, rt, reg, verifier)

	_, err := client.Send(context.Background(), bearer(connect.NewRequest(&loopv1.SendRequest{
		LoopId: "loop-1", Input: "hi", Tenant: "acme",
	}), "tok-bob"))
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("code = %v, want PermissionDenied (err=%v)", got, err)
	}
	if rt.sawSendLoop != "" {
		t.Fatal("seam Send was called despite cross-owner denial")
	}
}

// TestSendCrossTenantNotFound: the loop lives under acme but the principal is in a
// different tenant, so the tenant-scoped resolve misses → NotFound.
func TestSendCrossTenantNotFound(t *testing.T) {
	reg := newFakeRegistry()
	reg.seed("acme", "loop-1", "alice", "node-1")
	rt := &fakeRuntime{}
	verifier := tokenVerifier{tokens: map[string]loopauth.Principal{
		"tok-carol": {Tenant: "other", Subject: "carol"},
	}}
	client := newAuthzServer(t, rt, reg, verifier)

	// Wire tenant matches the principal (no spoof) but the loop is in a different tenant.
	_, err := client.Send(context.Background(), bearer(connect.NewRequest(&loopv1.SendRequest{
		LoopId: "loop-1", Input: "hi", Tenant: "other",
	}), "tok-carol"))
	if got := connect.CodeOf(err); got != connect.CodeNotFound {
		t.Fatalf("code = %v, want NotFound (err=%v)", got, err)
	}
	if rt.sawSendLoop != "" {
		t.Fatal("seam Send was called despite cross-tenant miss")
	}
}

// TestSendTenantSpoofDenied: wire tenant != principal tenant → PermissionDenied.
func TestSendTenantSpoofDenied(t *testing.T) {
	reg := newFakeRegistry()
	reg.seed("acme", "loop-1", "alice", "node-1")
	rt := &fakeRuntime{}
	verifier := tokenVerifier{tokens: map[string]loopauth.Principal{
		"tok-alice": {Tenant: "acme", Subject: "alice"},
	}}
	client := newAuthzServer(t, rt, reg, verifier)

	_, err := client.Send(context.Background(), bearer(connect.NewRequest(&loopv1.SendRequest{
		LoopId: "loop-1", Input: "hi", Tenant: "evilcorp",
	}), "tok-alice"))
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("code = %v, want PermissionDenied (err=%v)", got, err)
	}
}

// TestObserveSameOwnerSucceeds: the owner Observes and reaches the seam (a replayed
// frame proves the body ran with the principal's tenant).
func TestObserveSameOwnerSucceeds(t *testing.T) {
	reg := newFakeRegistry()
	reg.seed("acme", "loop-1", "alice", "node-1")
	f := newObserveFake()
	f.hist = func() []event.Event { return []event.Event{{Seq: 1}} }
	verifier := tokenVerifier{tokens: map[string]loopauth.Principal{
		"tok-alice": {Tenant: "acme", Subject: "alice"},
	}}
	h := NewHandler(&f.fakeRuntime).WithRegistry(reg).WithKeepalive(time.Hour)
	_, handler := loopv1connect.NewLoopServiceHandler(h, connect.WithInterceptors(NewAuthInterceptor(verifier)))
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := loopv1connect.NewLoopServiceClient(srv.Client(), srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.Observe(ctx, bearer(connect.NewRequest(&loopv1.ObserveRequest{
		LoopId: "loop-1", Tenant: "acme",
	}), "tok-alice"))
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	seen := collectSeqs(t, stream, 1)
	if len(seen) != 1 || seen[0] != 1 {
		t.Fatalf("got seqs %v, want [1]", seen)
	}
	cancel()
}

// TestObserveCrossOwnerDenied: a different subject in the same tenant is denied at
// stream open (PermissionDenied before any frame).
func TestObserveCrossOwnerDenied(t *testing.T) {
	reg := newFakeRegistry()
	reg.seed("acme", "loop-1", "alice", "node-1")
	f := newObserveFake()
	f.hist = func() []event.Event { return []event.Event{{Seq: 1}} }
	verifier := tokenVerifier{tokens: map[string]loopauth.Principal{
		"tok-bob": {Tenant: "acme", Subject: "bob"},
	}}
	h := NewHandler(&f.fakeRuntime).WithRegistry(reg).WithKeepalive(time.Hour)
	_, handler := loopv1connect.NewLoopServiceHandler(h, connect.WithInterceptors(NewAuthInterceptor(verifier)))
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := loopv1connect.NewLoopServiceClient(srv.Client(), srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, _ := client.Observe(ctx, bearer(connect.NewRequest(&loopv1.ObserveRequest{
		LoopId: "loop-1", Tenant: "acme",
	}), "tok-bob"))
	// The denial surfaces either on Observe() or the first Receive() depending on
	// streaming framing; drive Receive to force the error.
	stream.Receive()
	if got := connect.CodeOf(stream.Err()); got != connect.CodePermissionDenied {
		t.Fatalf("code = %v, want PermissionDenied (err=%v)", got, stream.Err())
	}
}

// TestObserveCrossTenantNotFound: loop under acme, principal in another tenant →
// NotFound.
func TestObserveCrossTenantNotFound(t *testing.T) {
	reg := newFakeRegistry()
	reg.seed("acme", "loop-1", "alice", "node-1")
	f := newObserveFake()
	f.hist = func() []event.Event { return []event.Event{{Seq: 1}} }
	verifier := tokenVerifier{tokens: map[string]loopauth.Principal{
		"tok-carol": {Tenant: "other", Subject: "carol"},
	}}
	h := NewHandler(&f.fakeRuntime).WithRegistry(reg).WithKeepalive(time.Hour)
	_, handler := loopv1connect.NewLoopServiceHandler(h, connect.WithInterceptors(NewAuthInterceptor(verifier)))
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := loopv1connect.NewLoopServiceClient(srv.Client(), srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, _ := client.Observe(ctx, bearer(connect.NewRequest(&loopv1.ObserveRequest{
		LoopId: "loop-1", Tenant: "other",
	}), "tok-carol"))
	stream.Receive()
	if got := connect.CodeOf(stream.Err()); got != connect.CodeNotFound {
		t.Fatalf("code = %v, want NotFound (err=%v)", got, stream.Err())
	}
}
