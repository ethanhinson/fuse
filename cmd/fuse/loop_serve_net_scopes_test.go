package main

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/ethanhinson/fuse/harness"
	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
)

// TestBuildLoopVerifierStaticScopes proves a static loop_server.auth entry's
// scopes reach the Principal through the real composition root, canonicalized,
// and that an entry without scopes stays unrestricted.
func TestBuildLoopVerifierStaticScopes(t *testing.T) {
	v, _, err := buildLoopVerifier(config.Config{LoopServer: config.LoopServerConfig{Auth: []config.AuthTokenConfig{
		{Token: "a", Tenant: "acme", Subject: "x", Scopes: []string{"loop:observe"}},
		{Token: "b", Tenant: "acme", Subject: "y"},
	}}})
	if err != nil {
		t.Fatalf("buildLoopVerifier: %v", err)
	}
	pa, err := v.Verify(context.Background(), "a")
	if err != nil {
		t.Fatalf("Verify(a): %v", err)
	}
	if want := loopauth.NewScopes("loop:observe"); pa.Scopes != want {
		t.Errorf("a.Scopes = %q, want %q", pa.Scopes, want)
	}
	pb, err := v.Verify(context.Background(), "b")
	if err != nil {
		t.Fatalf("Verify(b): %v", err)
	}
	if pb.Scopes != "" {
		t.Errorf("b.Scopes = %q, want unrestricted", pb.Scopes)
	}
}

// The tests below drive the scope, loop:admin, digest-pin and loop.principal
// features through the production composition root: the verifier comes from
// buildLoopVerifier(cfg) and the server is the real Connect edge over the real
// runtime (authTestServer). A hand-built verifier would not prove the config
// and issuer claims actually reach enforcement.

// scopesTestConfig is issuerTestConfig with the issuer allowed to assert both
// acme and globex.
func scopesTestConfig() config.Config {
	cfg := issuerTestConfig()
	cfg.LoopServer.Issuers[0].Tenants = []string{"acme", "globex"}
	return cfg
}

// scopedServer builds the production verifier from cfg and stands up the real
// server over it.
func scopedServer(t *testing.T, cfg config.Config) (string, event.LoopRegistry) {
	t.Helper()
	v, _, err := buildLoopVerifier(cfg)
	if err != nil {
		t.Fatalf("buildLoopVerifier: %v", err)
	}
	return authTestServer(t, v)
}

// scopedToken mints an issuer token for sub in tenant with extra claims merged.
func scopedToken(t *testing.T, sub, tenant string, extra map[string]any) string {
	t.Helper()
	c := issuerClaims(sub, tenant)
	for k, v := range extra {
		c[k] = v
	}
	return mintIssuerToken(t, issuerTestKey, c)
}

// assertDenied fails unless err is PermissionDenied and, when want is set, its
// message names want.
func assertDenied(t *testing.T, what string, err error, want string) {
	t.Helper()
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("%s: code = %v, want PermissionDenied (err=%v)", what, got, err)
	}
	if want != "" && !strings.Contains(err.Error(), want) {
		t.Fatalf("%s: refusal %q does not name %q", what, err.Error(), want)
	}
}

// observeUntilTurnEnd collects the loop's events from seq 0 until turn.end.
func observeUntilTurnEnd(t *testing.T, client loopv1connect.LoopServiceClient, loopID string) []*loopv1.Event {
	t.Helper()
	return observeThroughTurnEnds(t, client, loopID, 1)
}

// observeThroughTurnEnds collects the loop's events from seq 0 through the n-th
// turn.end, retrying until the stream holds that many.
func observeThroughTurnEnds(t *testing.T, client loopv1connect.LoopServiceClient, loopID string, n int) []*loopv1.Event {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		stream, err := client.Observe(ctx, connect.NewRequest(&loopv1.ObserveRequest{LoopId: loopID, FromSeq: 0}))
		if err != nil {
			cancel()
			time.Sleep(20 * time.Millisecond)
			continue
		}
		var evs []*loopv1.Event
		ends := 0
		for ends < n && stream.Receive() {
			msg := stream.Msg()
			if msg.Keepalive || msg.Event == nil {
				continue
			}
			evs = append(evs, msg.Event)
			if event.Kind(msg.Event.Kind) == event.KindTurnEnd {
				ends++
			}
		}
		_ = stream.Close()
		cancel()
		if ends >= n {
			return evs
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %d turn.end events", n)
	return nil
}

func TestScopedIssuerTokenEndToEnd(t *testing.T) {
	base, _ := scopedServer(t, scopesTestConfig())
	ctx := context.Background()

	starter := bearerClient(base, scopedToken(t, "cms-user-alice", "acme", map[string]any{"scope": "loop:start loop:observe"}))
	sr, err := starter.StartLoop(ctx, connect.NewRequest(&loopv1.StartLoopRequest{Task: "hi"}))
	if err != nil {
		t.Fatalf("scoped StartLoop: %v", err)
	}
	loopID := sr.Msg.LoopId
	waitForTurnEndByObserve(t, starter, loopID)

	observer := bearerClient(base, scopedToken(t, "cms-user-alice", "acme", map[string]any{"scope": "loop:observe"}))
	waitForTurnEndByObserve(t, observer, loopID) // Observe is allowed

	_, err = observer.StartLoop(ctx, connect.NewRequest(&loopv1.StartLoopRequest{Task: "hi"}))
	assertDenied(t, "observe-only StartLoop", err, loopauth.ScopeLoopStart)
	_, err = observer.Send(ctx, connect.NewRequest(&loopv1.SendRequest{LoopId: loopID, Input: "x"}))
	assertDenied(t, "observe-only Send", err, loopauth.ScopeLoopSend)

	// No scope claim: unrestricted, exactly as before.
	legacy := bearerClient(base, scopedToken(t, "cms-user-alice", "acme", nil))
	sr, err = legacy.StartLoop(ctx, connect.NewRequest(&loopv1.StartLoopRequest{Task: "hi"}))
	if err != nil {
		t.Fatalf("unscoped StartLoop: %v", err)
	}
	waitForTurnEndByObserve(t, legacy, sr.Msg.LoopId)
}

func TestAdminScopeEndToEnd(t *testing.T) {
	base, _ := scopedServer(t, scopesTestConfig())
	ctx := context.Background()

	alice := bearerClient(base, scopedToken(t, "cms-user-alice", "acme", map[string]any{"scope": "loop:start loop:observe"}))
	// Interactive, so the loop parks after its first turn and can take a Send.
	sr, err := alice.StartLoop(ctx, connect.NewRequest(&loopv1.StartLoopRequest{Task: "hi", Interactive: true}))
	if err != nil {
		t.Fatalf("alice StartLoop: %v", err)
	}
	loopID := sr.Msg.LoopId
	waitForTurnEndByObserve(t, alice, loopID)

	plain := bearerClient(base, scopedToken(t, "cms-user-bob", "acme", map[string]any{"scope": "loop:start loop:send"}))
	_, err = plain.Send(ctx, connect.NewRequest(&loopv1.SendRequest{LoopId: loopID, Input: "x"}))
	assertDenied(t, "non-admin cross-owner Send", err, "")

	foreign := bearerClient(base, scopedToken(t, "cms-user-bob", "globex", map[string]any{"scope": "loop:start loop:admin"}))
	_, err = foreign.Send(ctx, connect.NewRequest(&loopv1.SendRequest{LoopId: loopID, Input: "x"}))
	if got := connect.CodeOf(err); got != connect.CodeNotFound {
		t.Fatalf("cross-tenant admin Send code = %v, want NotFound (err=%v)", got, err)
	}

	admin := bearerClient(base, scopedToken(t, "cms-user-bob", "acme", map[string]any{"scope": "loop:start loop:admin"}))
	if _, err := admin.Send(ctx, connect.NewRequest(&loopv1.SendRequest{LoopId: loopID, Input: "more"})); err != nil {
		t.Fatalf("admin cross-owner Send: %v", err)
	}
	// Quiesce the admin-driven turn before teardown (loop:admin satisfies
	// loop:observe, so the admin token can watch it finish).
	observeThroughTurnEnds(t, admin, loopID, 2)
}

func TestDigestPinEndToEnd(t *testing.T) {
	base, registry := scopedServer(t, scopesTestConfig())
	ctx := context.Background()

	one, two := uint32(1), uint32(2)
	spec := &harnessv1.HarnessSpec{MaxTurns: &one}
	other := &harnessv1.HarnessSpec{MaxTurns: &two}
	pin := harness.Digest(spec)
	if pin == harness.Digest(other) {
		t.Fatal("fixture specs share a digest")
	}

	alice := bearerClient(base, scopedToken(t, "cms-user-alice", "acme", map[string]any{"harness_digest": pin}))

	for name, req := range map[string]*loopv1.StartLoopRequest{
		"no harness":    {Task: "hi"},
		"other harness": {Task: "hi", Harness: other},
	} {
		_, err := alice.StartLoop(ctx, connect.NewRequest(req))
		assertDenied(t, name, err, "")
		recs, lerr := registry.List(ctx, "acme")
		if lerr != nil {
			t.Fatal(lerr)
		}
		if len(recs) != 0 {
			t.Fatalf("%s: refused StartLoop left %d registry records", name, len(recs))
		}
	}

	sr, err := alice.StartLoop(ctx, connect.NewRequest(&loopv1.StartLoopRequest{Task: "hi", Harness: spec}))
	if err != nil {
		t.Fatalf("pinned StartLoop with its own spec: %v", err)
	}
	if sr.Msg.HarnessDigest != pin {
		t.Fatalf("harness_digest = %q, want %q", sr.Msg.HarnessDigest, pin)
	}
	waitForTurnEndByObserve(t, alice, sr.Msg.LoopId)
}

func TestStaticScopesEndToEnd(t *testing.T) {
	cfg := scopesTestConfig()
	cfg.LoopServer.Auth = []config.AuthTokenConfig{
		{Token: "obs-tok", Tenant: "acme", Subject: "ops", Scopes: []string{"loop:observe"}},
		{Token: "full-tok", Tenant: "acme", Subject: "ops"},
	}
	base, _ := scopedServer(t, cfg)
	ctx := context.Background()

	_, err := bearerClient(base, "obs-tok").StartLoop(ctx, connect.NewRequest(&loopv1.StartLoopRequest{Task: "hi"}))
	assertDenied(t, "observe-only static StartLoop", err, loopauth.ScopeLoopStart)

	full := bearerClient(base, "full-tok")
	sr, err := full.StartLoop(ctx, connect.NewRequest(&loopv1.StartLoopRequest{Task: "hi"}))
	if err != nil {
		t.Fatalf("unrestricted static StartLoop: %v", err)
	}
	waitForTurnEndByObserve(t, full, sr.Msg.LoopId)
}

func TestLoopPrincipalEventEndToEnd(t *testing.T) {
	base, _ := scopedServer(t, scopesTestConfig())
	ctx := context.Background()

	tok := scopedToken(t, "cms-user-alice", "acme", map[string]any{
		"scope": "loop:start loop:observe", "azp": "client-1", "jti": "x",
	})
	alice := bearerClient(base, tok)
	sr, err := alice.StartLoop(ctx, connect.NewRequest(&loopv1.StartLoopRequest{Task: "hi"}))
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	evs := observeUntilTurnEnd(t, alice, sr.Msg.LoopId)

	var principals []event.LoopPrincipalPayload
	for _, ev := range evs {
		if strings.Contains(string(ev.Payload), tok) {
			t.Fatalf("event %s payload carries the raw bearer token", ev.Kind)
		}
		if event.Kind(ev.Kind) != event.KindLoopPrincipal {
			continue
		}
		var p event.LoopPrincipalPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatalf("decode loop.principal: %v", err)
		}
		principals = append(principals, p)
	}
	if len(principals) != 1 {
		t.Fatalf("loop.principal events = %d, want exactly 1", len(principals))
	}
	want := event.LoopPrincipalPayload{
		Subject: "cms-user-alice", ClientID: "client-1", Issuer: "cms",
		Scopes: []string{"loop:observe", "loop:start"}, TokenID: "x",
	}
	if !reflect.DeepEqual(principals[0], want) {
		t.Fatalf("loop.principal = %+v, want %+v", principals[0], want)
	}
}

// A static loop_server.auth token records issuer "static" so audit can tell it
// apart from a trusted issuer's principal.
func TestStaticTokenPrincipalIssuerIsStatic(t *testing.T) {
	cfg := config.Config{LoopServer: config.LoopServerConfig{Auth: []config.AuthTokenConfig{
		{Token: "tok-static", Tenant: "acme", Subject: "alice"},
	}}}
	v, _, err := buildLoopVerifier(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p, err := v.Verify(context.Background(), "tok-static")
	if err != nil {
		t.Fatal(err)
	}
	if p.Issuer != "static" {
		t.Fatalf("Issuer = %q, want static", p.Issuer)
	}
}
