package runtime

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/model"
)

func testPrincipal() *event.LoopPrincipalPayload {
	return &event.LoopPrincipalPayload{Subject: "alice", ClientID: "client-1", Issuer: "cms", Scopes: []string{"loop:start"}, TokenID: "x"}
}

func principalEvents(evs []event.Event) []event.Event {
	var out []event.Event
	for _, e := range evs {
		if e.Kind == event.KindLoopPrincipal {
			out = append(out, e)
		}
	}
	return out
}

// runToCompletion starts cfg on rt, waits for it, and returns its whole history.
func runToCompletion(t *testing.T, rt Runtime, cfg LoopConfig) []event.Event {
	t.Helper()
	ctx := context.Background()
	h, err := rt.StartLoop(ctx, cfg)
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	hist, err := rt.Attach(ctx, event.NormalizeTenant(cfg.Tenant), h.ID(), 0)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	return hist
}

func TestPrincipalRecordedAfterHarness(t *testing.T) {
	store := fsstore.NewDurableFSStore(t.TempDir())
	fh := &fakeHarness{completer: &scriptedCompleter{}}
	rt := New(Deps{MaxConcurrent: 1, DurableStore: store, Registry: store, Harness: fh})
	rec := testPrincipal()
	hist := runToCompletion(t, rt, LoopConfig{Task: "hi", Tenant: "acme", Harness: testSpec(), Principal: rec})
	if len(hist) < 2 || hist[0].Kind != event.KindLoopHarness || hist[1].Kind != event.KindLoopPrincipal {
		t.Fatalf("history does not open loop.harness, loop.principal: %v", kindsOf(hist))
	}
	recs := principalEvents(hist)
	if len(recs) != 1 {
		t.Fatalf("loop.principal events = %d, want 1: %v", len(recs), kindsOf(hist))
	}
	var got event.LoopPrincipalPayload
	if err := json.Unmarshal(recs[0].Payload, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, *rec) {
		t.Fatalf("payload = %+v, want %+v", got, *rec)
	}
}

func TestPrincipalRecordedFirstWithoutHarness(t *testing.T) {
	store := fsstore.NewDurableFSStore(t.TempDir())
	hits := 0
	rt := New(Deps{MaxConcurrent: 1, DurableStore: store, Registry: store, BuildAgent: processBuild(t, &scriptedCompleter{}, &hits)})
	hist := runToCompletion(t, rt, LoopConfig{Task: "hi", Tenant: "acme", Principal: testPrincipal()})
	if len(hist) == 0 || hist[0].Kind != event.KindLoopPrincipal {
		t.Fatalf("loop.principal is not the first event: %v", kindsOf(hist))
	}
	if n := len(principalEvents(hist)); n != 1 {
		t.Fatalf("loop.principal events = %d, want 1", n)
	}
}

func TestPrincipalNilScopesRecordedAsEmptyArray(t *testing.T) {
	ev, err := principalEvent("root", &event.LoopPrincipalPayload{Subject: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(ev.Payload, &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["scopes"]) != "[]" {
		t.Fatalf("scopes = %s, want []", raw["scopes"])
	}
	if ev.Kind != event.KindLoopPrincipal || ev.NodeID != "root" {
		t.Fatalf("event = %+v", ev)
	}
}

func TestNoPrincipalNoRecord(t *testing.T) {
	store := fsstore.NewDurableFSStore(t.TempDir())
	hits := 0
	rt := New(Deps{MaxConcurrent: 1, DurableStore: store, Registry: store, BuildAgent: processBuild(t, &scriptedCompleter{}, &hits)})
	hist := runToCompletion(t, rt, LoopConfig{Task: "hi", Tenant: "acme"})
	if n := len(principalEvents(hist)); n != 0 {
		t.Fatalf("loop.principal recorded without a principal: %v", kindsOf(hist))
	}
}

func TestPrincipalNotRecordedAgainOnResume(t *testing.T) {
	store := fsstore.NewDurableFSStore(t.TempDir())
	var reg event.LoopRegistry = store
	spec := testSpec()

	fhA := &fakeHarness{completer: newGatedCompleter(model.CompletionResp{Content: "first"})}
	rtA := New(Deps{DurableStore: store, Registry: reg, MaxConcurrent: 1, IdleTTL: 120 * time.Millisecond, Harness: fhA})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hA, err := rtA.StartLoop(ctx, LoopConfig{Task: "hello", Tenant: "acme", Interactive: true, Harness: spec, Principal: testPrincipal()})
	if err != nil {
		t.Fatalf("A StartLoop: %v", err)
	}
	loopID := hA.ID()
	evA, unsubA, err := rtA.Observe(ctx, "acme", loopID)
	if err != nil {
		t.Fatal(err)
	}
	fhA.completer.(*gatedCompleter).admit(0)
	waitForKind(t, evA, event.KindLoopParked, 2*time.Second)
	unsubA()
	select {
	case <-doneOf(hA):
	case <-time.After(2 * time.Second):
		t.Fatal("A's loop was not reaped")
	}

	fhB := &fakeHarness{completer: newGatedCompleter(model.CompletionResp{Content: "second"})}
	rtB := New(Deps{DurableStore: store, Registry: reg, MaxConcurrent: 1, IdleTTL: time.Hour, Harness: fhB})
	if _, err := rtB.Resume(ctx, "acme", loopID); err != nil {
		t.Fatalf("B Resume: %v", err)
	}
	hist, err := rtB.Attach(ctx, "acme", loopID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(principalEvents(hist)); n != 1 {
		t.Fatalf("loop.principal events after resume = %d, want 1: %v", n, kindsOf(hist))
	}
}

func TestFailedPrincipalRecordLeavesTheLoopNotLive(t *testing.T) {
	reg := newFakeRegistry()
	store := failingAppendStore{fsstore.NewDurableFSStore(t.TempDir())}
	hits := 0
	rt := New(Deps{MaxConcurrent: 1, DurableStore: store, Registry: reg, BuildAgent: processBuild(t, &scriptedCompleter{}, &hits)})
	if _, err := rt.StartLoop(context.Background(), LoopConfig{Task: "hi", Principal: testPrincipal()}); err == nil {
		t.Fatal("StartLoop succeeded without recording its principal")
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if len(reg.records) != 1 {
		t.Fatalf("records = %d", len(reg.records))
	}
	for _, rec := range reg.records {
		if rec.Live {
			t.Fatal("a loop that never ran is still recorded live")
		}
	}
	if hits != 0 {
		t.Fatal("an agent was built for a loop whose principal was not recorded")
	}
}
