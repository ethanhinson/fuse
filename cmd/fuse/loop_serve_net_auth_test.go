package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/loopauth"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/runtime"
)

// authTestServer stands up a REAL runtime (production buildLoopServerRuntimeDeps
// wiring) over a shared fsstore durable store + registry, mounted through the
// production serveNet with the auth interceptor + registry wired exactly as
// runLoopServeNet does. It returns the server base URL and the shared registry
// (so a test can inspect/seed records). The scripted LLM_GATEWAY_URL double drives
// deterministic model-free turns — NEVER a real provider (project policy).
//
// This is the change-0049 Task-8 acceptance harness: it proves auth + per-loop
// authz + reconnect over the REAL Connect wire against the REAL runtime, not a
// fake handler (smoke-over-fake-backend-proves-wire-not-system).
func authTestServer(t *testing.T, verifier loopauth.Verifier) (base string, registry event.LoopRegistry) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	// Scripted gateway double: a fixed reply so each loop produces a deterministic,
	// model-free turn. NEVER a real provider.
	srv := newScriptedGateway(t)
	t.Setenv("LLM_GATEWAY_URL", srv)
	t.Setenv("LLM_GATEWAY_KEY", "tkn")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	reg := registryFromConfig(cfg)
	alias := reg.Default

	dir := t.TempDir()
	store := fsstore.NewDurableFSStore(dir)

	deps := buildLoopServerRuntimeDeps(nil, cfg, reg, alias, defaultToolRegistry(nil, cfg.Research, nil),
		spawnAgentBlock, permissions.AlwaysApprove, sessionRateGate(cfg))
	deps.DurableStore = store
	deps.Registry = store
	deps.BaseDir = "" // durable-only path
	// A short lease TTL so the re-own path is reachable via the runtime seam; the
	// heartbeat renewer keeps a live loop's lease fresh under this TTL.
	deps.LeaseTTL = 2 * time.Second
	rt := runtime.New(deps)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- serveNet(ctx, ln, rt, verifier, store) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-serveDone:
		case <-time.After(5 * time.Second):
			t.Error("serveNet did not return after cancel")
		}
	})
	return "http://" + ln.Addr().String(), store
}

// newScriptedGateway starts an httptest-style gateway that echoes a fixed
// assistant reply, and returns its URL. It is torn down via t.Cleanup.
func newScriptedGateway(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("gateway listen: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"REPLY-ACCEPTANCE"}}]}`)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String()
}

// twoTenantVerifier maps three tokens: OWNER and PEER share tenant "acme"
// (different subjects → cross-OWNER authz), and OTHER is tenant "globex" (a
// different tenant → cross-TENANT not-found).
func twoTenantVerifier() loopauth.Verifier {
	return loopauth.NewStaticVerifier(map[string]loopauth.Principal{
		"owner-tok": {Tenant: "acme", Subject: "owner"},
		"peer-tok":  {Tenant: "acme", Subject: "peer"},
		"other-tok": {Tenant: "globex", Subject: "other"},
	})
}

// TestAuthPassAndDeny is acceptance (1): a valid bearer token starts a loop over
// the real Connect server; a missing or an invalid token is rejected with
// CodeUnauthenticated BEFORE the handler runs.
func TestAuthPassAndDeny(t *testing.T) {
	v := loopauth.NewStaticVerifier(map[string]loopauth.Principal{
		"good-tok": {Tenant: event.DefaultTenant, Subject: "alice"},
	})
	base, _ := authTestServer(t, v)
	ctx := context.Background()

	// PASS: a valid token starts a loop.
	good := bearerClient(base, "good-tok")
	sr, err := good.StartLoop(ctx, connect.NewRequest(&loopv1.StartLoopRequest{Task: "hi"}))
	if err != nil {
		t.Fatalf("valid-token StartLoop: %v", err)
	}
	if sr.Msg.LoopId == "" {
		t.Fatal("valid-token StartLoop returned empty loop_id")
	}
	// Drain the started loop to turn.end so its async fsstore writes + heartbeat
	// renewer are quiescent before teardown (no store-dir cleanup race).
	waitForTurnEndByObserve(t, good, sr.Msg.LoopId)

	// DENY (missing token): no Authorization header → CodeUnauthenticated.
	none := bearerClient(base, "")
	_, err = none.StartLoop(ctx, connect.NewRequest(&loopv1.StartLoopRequest{Task: "hi"}))
	if got := connect.CodeOf(err); got != connect.CodeUnauthenticated {
		t.Fatalf("missing-token StartLoop code = %v, want Unauthenticated (err=%v)", got, err)
	}

	// DENY (invalid token): an unknown token → CodeUnauthenticated.
	bad := bearerClient(base, "nope")
	_, err = bad.StartLoop(ctx, connect.NewRequest(&loopv1.StartLoopRequest{Task: "hi"}))
	if got := connect.CodeOf(err); got != connect.CodeUnauthenticated {
		t.Fatalf("invalid-token StartLoop code = %v, want Unauthenticated (err=%v)", got, err)
	}
}

// TestAuthTenantSpoofRejected is acceptance (2): an authenticated caller whose
// wire tenant differs from its principal's tenant is rejected with
// CodePermissionDenied (a tenant spoof), even though the token itself is valid.
func TestAuthTenantSpoofRejected(t *testing.T) {
	base, _ := authTestServer(t, twoTenantVerifier())
	ctx := context.Background()

	owner := bearerClient(base, "owner-tok") // principal tenant = "acme"
	// Wire tenant "globex" != principal "acme" → spoof.
	_, err := owner.StartLoop(ctx, connect.NewRequest(&loopv1.StartLoopRequest{
		Task: "hi", Tenant: "globex",
	}))
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("tenant-spoof StartLoop code = %v, want PermissionDenied (err=%v)", got, err)
	}

	// Sanity: the same call with the MATCHING wire tenant succeeds.
	sr, err := owner.StartLoop(ctx, connect.NewRequest(&loopv1.StartLoopRequest{Task: "hi", Tenant: "acme"}))
	if err != nil {
		t.Fatalf("matching-tenant StartLoop: %v", err)
	}
	if sr.Msg.LoopId == "" {
		t.Fatal("matching-tenant StartLoop returned empty loop_id")
	}
	// Drain to turn.end so the store is quiescent before teardown (Observe under the
	// acme tenant: the owner's wire tenant is empty ⇒ its principal tenant applies).
	waitForTurnEndByObserve(t, owner, sr.Msg.LoopId)
}

// TestAuthzCrossTenantNotFoundVsCrossOwnerDenied is acceptance (3): after OWNER
// starts a loop (recording Owner=owner under tenant acme), a same-tenant PEER
// (different subject) hitting that loop gets CodePermissionDenied, while a
// different-tenant OTHER gets CodeNotFound (the tenant-scoped Resolve misses). The
// two distinct codes are the assertion.
func TestAuthzCrossTenantNotFoundVsCrossOwnerDenied(t *testing.T) {
	base, _ := authTestServer(t, twoTenantVerifier())
	ctx := context.Background()

	owner := bearerClient(base, "owner-tok")
	sr, err := owner.StartLoop(ctx, connect.NewRequest(&loopv1.StartLoopRequest{Task: "hi"}))
	if err != nil {
		t.Fatalf("owner StartLoop: %v", err)
	}
	loopID := sr.Msg.LoopId
	// Drain to turn.end: StartLoop records Owner synchronously before returning, so
	// the record is already owned; draining also quiesces the store before teardown.
	waitForTurnEndByObserve(t, owner, loopID)

	// Same-tenant, different subject → CodePermissionDenied (cross-owner).
	peer := bearerClient(base, "peer-tok")
	_, err = peer.Send(ctx, connect.NewRequest(&loopv1.SendRequest{LoopId: loopID, Input: "x"}))
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("cross-owner Send code = %v, want PermissionDenied (err=%v)", got, err)
	}

	// Different tenant → CodeNotFound (tenant-scoped Resolve misses the loop).
	other := bearerClient(base, "other-tok")
	_, err = other.Send(ctx, connect.NewRequest(&loopv1.SendRequest{LoopId: loopID, Input: "x"}))
	if got := connect.CodeOf(err); got != connect.CodeNotFound {
		t.Fatalf("cross-tenant Send code = %v, want NotFound (err=%v)", got, err)
	}
}

// TestReconnectNoLossNoDup is acceptance (4): OWNER starts a loop, drives it to a
// finished turn, Observes from 0, then RE-OPENS Observe(from_seq) RE-PRESENTING the
// token, asserting exactly-once (no loss / no dup) across the subscribe→replay
// handoff. A concurrent append is forced into the second Observe's subscribe→replay
// window (a direct store Append on the loop's StreamKey) to keep the dedup honest
// (replay-live-handoff-dedup-at-watermark).
func TestReconnectNoLossNoDup(t *testing.T) {
	v := loopauth.NewStaticVerifier(map[string]loopauth.Principal{
		"owner-tok": {Tenant: event.DefaultTenant, Subject: "owner"},
	})
	base, registry := authTestServer(t, v)
	store, ok := registry.(*fsstore.FSDurableStore)
	if !ok {
		t.Fatalf("registry is not an FSDurableStore (%T) — cannot force a gap append", registry)
	}
	owner := bearerClient(base, "owner-tok")
	ctx := context.Background()

	sr, err := owner.StartLoop(ctx, connect.NewRequest(&loopv1.StartLoopRequest{Task: "do the thing"}))
	if err != nil {
		t.Fatalf("owner StartLoop: %v", err)
	}
	loopID := sr.Msg.LoopId

	// Wait for the one-shot loop's turn.end to persist so the durable history is
	// complete before we observe.
	lastSeq := waitForTurnEndByObserve(t, owner, loopID)

	// First Observe from 0: replay the full durable history. Record the seqs.
	seen1 := collectObserveThrough(t, owner, loopID, 0, lastSeq)
	if len(seen1) == 0 {
		t.Fatal("first Observe saw no events")
	}
	assertNoDup(t, seen1, "first observe")

	// Reconnect: RE-PRESENT the token and re-open Observe(from_seq=0). Force a
	// concurrent append into the subscribe→replay window; the handler's subscribe-
	// before-replay + watermark dedup must deliver every seq EXACTLY once.
	key := event.StreamKey{Tenant: event.DefaultTenant, Loop: event.LoopID(loopID)}
	forcedSeq := lastSeq + 1

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(20 * time.Millisecond) // land during the observe setup
		_ = store.Append(context.Background(), key, event.Event{Kind: event.KindError})
	}()

	seen2 := collectObserveThrough(t, owner, loopID, 0, forcedSeq)
	wg.Wait()

	assertNoDup(t, seen2, "reconnect observe")
	if len(seen2) < len(seen1) {
		t.Fatalf("reconnect replayed %d events, want >= first observe's %d (loss on reconnect)", len(seen2), len(seen1))
	}
	if seen2[len(seen2)-1] != forcedSeq {
		t.Fatalf("reconnect last seq = %d, want the forced append seq %d", seen2[len(seen2)-1], forcedSeq)
	}
}

// --- helpers (this file) ----------------------------------------------------

// waitForTurnEndByObserve opens Observe(0) as the authenticated owner and drains
// until it sees a turn.end event, returning the last seq seen up to and including
// it. It resolves the durable-flush race so a subsequent reconnect replays a
// complete history.
func waitForTurnEndByObserve(t *testing.T, client loopv1connect.LoopServiceClient, loopID string) uint64 {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		stream, err := client.Observe(ctx, connect.NewRequest(&loopv1.ObserveRequest{LoopId: loopID, FromSeq: 0}))
		if err != nil {
			cancel()
			time.Sleep(20 * time.Millisecond)
			continue
		}
		var last uint64
		sawEnd := false
		for stream.Receive() {
			msg := stream.Msg()
			if msg.Keepalive || msg.Event == nil {
				continue
			}
			last = msg.Event.Seq
			if event.Kind(msg.Event.Kind) == event.KindTurnEnd {
				sawEnd = true
				break
			}
		}
		_ = stream.Close()
		cancel()
		if sawEnd {
			return last
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timeout waiting for loop turn.end")
	return 0
}

// collectObserveThrough opens Observe(from) as client and collects ordered seqs
// until it sees `through` (inclusive), returning them.
func collectObserveThrough(t *testing.T, client loopv1connect.LoopServiceClient, loopID string, from, through uint64) []uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	stream, err := client.Observe(ctx, connect.NewRequest(&loopv1.ObserveRequest{LoopId: loopID, FromSeq: from}))
	if err != nil {
		t.Fatalf("Observe(collect): %v", err)
	}
	var seqs []uint64
	for stream.Receive() {
		msg := stream.Msg()
		if msg.Keepalive || msg.Event == nil {
			continue
		}
		seqs = append(seqs, msg.Event.Seq)
		if msg.Event.Seq >= through {
			_ = stream.Close()
			return seqs
		}
	}
	t.Fatalf("Observe ended before seq %d (err=%v); saw %v", through, stream.Err(), seqs)
	return seqs
}

// assertNoDup asserts strictly-increasing seqs with no duplicates (the
// exactly-once contract). It does not require starting at 1.
func assertNoDup(t *testing.T, seqs []uint64, who string) {
	t.Helper()
	seen := map[uint64]bool{}
	var prev uint64
	for i, s := range seqs {
		if seen[s] {
			t.Fatalf("%s: seq %d delivered twice in %v", who, s, seqs)
		}
		seen[s] = true
		if i > 0 && s <= prev {
			t.Fatalf("%s: seq %d not strictly increasing after %d in %v", who, s, prev, seqs)
		}
		prev = s
	}
}
