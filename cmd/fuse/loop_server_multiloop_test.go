package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/runtime"
)

// TestLoopServerHostsNConcurrentLoops is the load-bearing multi-loop proof (plan
// Task 4): ONE loop-server Deps / Runtime (binding #2, its real production wiring via
// buildLoopServerRuntimeDeps) hosts N loops started CONCURRENTLY — the exact shape a
// `fuse loop-server` process serves when N clients each issue loop.start. It drives a
// distinct single-turn task into each loop through a scripted httptest gateway
// (learning verify-tool-loop-at-gateway-seam) and asserts, per loop:
//   - a distinct loop id (distinct tree ⇒ distinct fsstore),
//   - Attach sees ONLY that loop's own events — the assistant reply carried in its
//     stream is the one scripted for ITS task, never another loop's (no cross-loop
//     bleed through a shared store),
//   - per-loop Seq is contiguous and monotonic from 1.
//
// This is the test that would have gone RED with the retired process-global event
// store in place: N concurrent StartLoop calls each installed their fsstore into the
// single global slot, so a later loop clobbered an earlier loop's store and their
// streams bled together. A sequential N-loop version stays green even with the global,
// so this drives them concurrently on purpose
// (learning race-invisible-to-race-detector-without-concurrent-test).
func TestLoopServerHostsNConcurrentLoops(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	const n = 4

	// One scripted gateway shared as the MODEL only. It echoes the requested task's
	// unique marker back as the assistant reply, so each loop's stream carries a reply
	// that is unique to ITS task — the load-bearing signal that proves isolation.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		reply := "no-marker"
		for i := 0; i < n; i++ {
			marker := fmt.Sprintf("MARKER-%d", i)
			if containsSub(string(body), marker) {
				reply = "REPLY-" + marker
				break
			}
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"`+reply+`"}}]}`)
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

	// ONE Deps / ONE Runtime — the production loop-server wiring. BaseDir is overridden
	// to a temp dir so the run is isolated from the real session dir; the wiring under
	// test (per-loop tree + per-loop fsstore keyed by loop id) is unchanged.
	deps := buildLoopServerRuntimeDeps(cfg, reg, alias, defaultToolRegistry(cfg.Research, nil),
		spawnAgentBlock, permissions.AlwaysApprove, sessionRateGate(cfg))
	deps.BaseDir = t.TempDir()
	rt := runtime.New(deps)

	// Start N loops CONCURRENTLY, each with a task carrying a unique marker.
	var wg sync.WaitGroup
	ids := make([]string, n)
	var idsMu sync.Mutex
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			task := fmt.Sprintf("do the thing MARKER-%d", i)
			h, err := rt.StartLoop(context.Background(), runtime.LoopConfig{Task: task, ModelID: alias})
			if err != nil {
				t.Errorf("loop %d StartLoop: %v", i, err)
				return
			}
			if _, err := h.Wait(); err != nil {
				t.Errorf("loop %d Wait: %v", i, err)
				return
			}
			idsMu.Lock()
			ids[i] = h.ID()
			idsMu.Unlock()
		}(i)
	}
	wg.Wait()

	// Distinct ids: distinct per-loop trees ⇒ distinct fsstores.
	seen := map[string]bool{}
	for i, id := range ids {
		if id == "" {
			t.Fatalf("loop %d never recorded an id", i)
		}
		if seen[id] {
			t.Fatalf("loop %d reused id %q — two loops share a tree/store", i, id)
		}
		seen[id] = true
	}

	// Each loop's durable stream carries ONLY its own reply, with contiguous per-loop
	// Seq from 1.
	for i, id := range ids {
		evs, aerr := rt.Attach(id, 0)
		if aerr != nil {
			t.Fatalf("loop %d Attach: %v", i, aerr)
		}
		if len(evs) == 0 {
			t.Fatalf("loop %d produced no events", i)
		}
		wantReply := fmt.Sprintf("REPLY-MARKER-%d", i)
		var prev event.Seq
		sawOwnReply := false
		for j, e := range evs {
			if want := event.Seq(j + 1); e.Seq != want {
				t.Errorf("loop %d event %d Seq = %d, want %d (per-loop allocator must be contiguous from 1)", i, j, e.Seq, want)
			}
			if e.Seq <= prev {
				t.Errorf("loop %d Seq not monotonic: %d after %d", i, e.Seq, prev)
			}
			prev = e.Seq
			// Any other loop's reply appearing here is cross-loop bleed.
			for k := 0; k < n; k++ {
				if k == i {
					continue
				}
				otherReply := fmt.Sprintf("REPLY-MARKER-%d", k)
				if containsSub(string(e.Payload), otherReply) {
					t.Fatalf("loop %d stream contains loop %d's reply %q — cross-loop bleed", i, k, otherReply)
				}
			}
			if containsSub(string(e.Payload), wantReply) {
				sawOwnReply = true
			}
		}
		if !sawOwnReply {
			t.Errorf("loop %d stream never carried its own reply %q", i, wantReply)
		}
	}
}

// containsSub is a tiny substring check kept local so this test does not depend on
// strings import churn elsewhere.
func containsSub(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
