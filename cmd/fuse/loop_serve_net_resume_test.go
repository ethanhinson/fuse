package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/loopconnect"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/runtime"
)

// TestColdResumeAcceptance is change 0054's D6 test 3: the real-server, real-runtime
// cold-resume acceptance. An INTERACTIVE loop is started on instance A over a shared
// durable store and driven through one turn to a park; A's session is then reaped
// (short IdleTTL) so the loop is finished/not-live. A FRESH instance B — same store,
// empty in-memory loop map — receives a Send from the client and TRANSPARENTLY resumes
// the loop (D3 handler routing): the Send succeeds instead of CodeFailedPrecondition,
// and an Observe on B from seq 0 replays the ORIGINAL exchange's transcript (the loop
// survived as durable events), proving refresh-to-restore end to end against the real
// Connect wire.
//
// Store: fsstore (no Postgres/testcontainers). Gateway: a scripted LLM_GATEWAY_URL
// double (project policy — never a real provider).
func TestColdResumeAcceptance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		// A no-tool answer: an interactive loop parks after it (deterministic, model-free).
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"REPLY-RESUME"}}]}`)
	}))
	defer srv.Close()
	t.Setenv("LLM_GATEWAY_URL", srv.URL)
	t.Setenv("LLM_GATEWAY_KEY", "tkn")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	reg := registryFromConfig(cfg)
	alias := reg.Default

	dir := t.TempDir()
	store := fsstore.NewDurableFSStore(dir)

	buildDeps := func(idleTTL time.Duration) runtime.Deps {
		d := buildLoopServerRuntimeDeps(cfg, reg, alias, defaultToolRegistry(cfg.Research, nil),
			spawnAgentBlock, permissions.AlwaysApprove, sessionRateGate(cfg))
		d.DurableStore = store
		d.Registry = store
		d.BaseDir = "" // durable-only
		d.IdleTTL = idleTTL
		return d
	}
	// A reaps quickly (so the interactive loop becomes not-live after its park); B keeps
	// the resumed loop alive for the assertions.
	rtA := runtime.New(buildDeps(150 * time.Millisecond))
	rtB := runtime.New(buildDeps(time.Hour))

	serverFor := func(rt runtime.Runtime) (loopv1connect.LoopServiceClient, func()) {
		_, h := loopv1connect.NewLoopServiceHandler(loopconnect.NewHandler(rt).WithKeepalive(time.Hour))
		s := httptest.NewServer(h)
		return loopv1connect.NewLoopServiceClient(s.Client(), s.URL), s.Close
	}
	clientA, closeA := serverFor(rtA)
	defer closeA()
	clientB, closeB := serverFor(rtB)
	defer closeB()

	ctx := context.Background()

	// Start an INTERACTIVE loop on A and drive its first turn to a park.
	startResp, err := clientA.StartLoop(ctx, connect.NewRequest(&loopv1.StartLoopRequest{
		Task: "hello there", Model: alias, Interactive: true,
	}))
	if err != nil {
		t.Fatalf("A StartLoop: %v", err)
	}
	loopID := startResp.Msg.LoopId
	if loopID == "" {
		t.Fatal("A StartLoop returned empty loop_id")
	}

	// Wait for the first exchange to park (loop.parked persisted).
	waitForKindInStore(t, rtA, loopID, event.KindLoopParked)

	// Let A's idle reaper flip the loop to not-live (no live Observe, no Send). Poll the
	// registry until Resolve reports Live=false.
	waitForNotLive(t, store, loopID)

	// On instance B (cold: empty loop map), Send via the real client. The handler must
	// transparently RESUME the finished loop and retry, so this succeeds.
	if _, err := clientB.Send(ctx, connect.NewRequest(&loopv1.SendRequest{
		LoopId: loopID, Input: "and then?",
	})); err != nil {
		t.Fatalf("cold Send should resume the finished loop and succeed, got: %v", err)
	}

	// Observe on B from 0 must replay the ORIGINAL exchange's transcript — proving the
	// session survived the eviction as durable events and B rehydrated the same loop_id.
	// Wait until the durable history shows the resumed turn parked too (>= 2 parks).
	waitForParkCount(t, rtB, loopID, 2)

	hist, err := rtB.Attach(ctx, event.DefaultTenant, loopID, 0)
	if err != nil {
		t.Fatalf("B Attach: %v", err)
	}
	if !storeHasKind(hist, event.KindUserInput) {
		t.Fatalf("resumed loop history missing the original user input: %v", kindList(hist))
	}
	parks := 0
	for _, e := range hist {
		if e.Kind == event.KindLoopParked {
			parks++
		}
	}
	if parks < 2 {
		t.Fatalf("resumed loop history has %d parks, want >= 2 (original + resumed exchange)", parks)
	}
}

// waitForKindInStore polls the runtime's durable Attach until the loop's history
// contains an event of kind k.
func waitForKindInStore(t *testing.T, rt runtime.Runtime, loopID string, k event.Kind) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		hist, err := rt.Attach(context.Background(), event.DefaultTenant, loopID, 0)
		if err == nil && storeHasKind(hist, k) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s in loop %s durable history", k, loopID)
}

// waitForParkCount polls until the loop's durable history holds at least n loop.parked
// events.
func waitForParkCount(t *testing.T, rt runtime.Runtime, loopID string, n int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		hist, err := rt.Attach(context.Background(), event.DefaultTenant, loopID, 0)
		if err == nil {
			c := 0
			for _, e := range hist {
				if e.Kind == event.KindLoopParked {
					c++
				}
			}
			if c >= n {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %d parks in loop %s", n, loopID)
}

// waitForNotLive polls the durable registry until the loop's record is not live (its
// session reaped).
func waitForNotLive(t *testing.T, reg event.LoopRegistry, loopID string) {
	t.Helper()
	key := event.StreamKey{Tenant: event.DefaultTenant, Loop: event.LoopID(loopID)}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		rec, err := reg.Resolve(context.Background(), key)
		if err == nil && !rec.Live {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for loop %s to become not-live (reaped)", loopID)
}

func storeHasKind(evs []event.Event, k event.Kind) bool {
	for _, e := range evs {
		if e.Kind == k {
			return true
		}
	}
	return false
}

func kindList(evs []event.Event) []event.Kind {
	out := make([]event.Kind, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}
