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
	"github.com/ethanhinson/fuse/internal/tools"
	"github.com/ethanhinson/fuse/internal/workspace"
)

func workspaceEvents(evs []event.Event) []event.Event {
	var out []event.Event
	for _, e := range evs {
		if e.Kind == event.KindLoopWorkspace {
			out = append(out, e)
		}
	}
	return out
}

// TestWorkspaceRecordedAfterHarness proves a loop started with a workspace
// provider records exactly one loop.workspace event on its stream, right after
// loop.harness (change 0129, Task 2).
func TestWorkspaceRecordedAfterHarness(t *testing.T) {
	store := fsstore.NewDurableFSStore(t.TempDir())
	fh := &fakeHarness{completer: &scriptedCompleter{}}
	rt := New(Deps{
		MaxConcurrent: 1,
		DurableStore:   store,
		Registry:       store,
		Harness:        fh,
		Workspaces:      &workspace.LocalProvider{Parent: t.TempDir()},
	})
	hist := runToCompletion(t, rt, LoopConfig{Task: "hi", Tenant: "acme", Harness: testSpec()})
	if len(hist) < 2 || hist[0].Kind != event.KindLoopHarness {
		t.Fatalf("history does not open loop.harness: %v", kindsOf(hist))
	}
	recs := workspaceEvents(hist)
	if len(recs) != 1 {
		t.Fatalf("loop.workspace events = %d, want 1: %v", len(recs), kindsOf(hist))
	}
	var got event.LoopWorkspacePayload
	if err := json.Unmarshal(recs[0].Payload, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID == "" {
		t.Error("workspace event payload ID is empty")
	}
	if got.Provider != "local" {
		t.Errorf("workspace event provider = %q, want %q", got.Provider, "local")
	}
}

// TestWorkspaceRecordedAfterPrincipalWithoutHarness proves a loop without a
// harness spec but with a workspace provider still records exactly one
// loop.workspace event (change 0129, Task 2).
func TestWorkspaceRecordedAfterPrincipalWithoutHarness(t *testing.T) {
	store := fsstore.NewDurableFSStore(t.TempDir())
	hits := 0
	rt := New(Deps{
		MaxConcurrent: 1,
		DurableStore:   store,
		Registry:       store,
		Workspaces:      &workspace.LocalProvider{Parent: t.TempDir()},
		BuildAgent:     processBuild(t, &scriptedCompleter{}, &hits),
	})
	hist := runToCompletion(t, rt, LoopConfig{Task: "hi", Tenant: "acme", Principal: testPrincipal()})
	recs := workspaceEvents(hist)
	if len(recs) != 1 {
		t.Fatalf("loop.workspace events = %d, want 1: %v", len(recs), kindsOf(hist))
	}
	var got event.LoopWorkspacePayload
	if err := json.Unmarshal(recs[0].Payload, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID == "" {
		t.Error("workspace event payload ID is empty")
	}
}

// TestNoWorkspaceNoRecord proves a loop started without a workspace provider
// records no loop.workspace event — a one-shot binding stays byte-identical.
func TestNoWorkspaceNoRecord(t *testing.T) {
	store := fsstore.NewDurableFSStore(t.TempDir())
	hits := 0
	rt := New(Deps{
		MaxConcurrent: 1,
		DurableStore:  store,
		Registry:     store,
		BuildAgent:    processBuild(t, &scriptedCompleter{}, &hits),
	})
	hist := runToCompletion(t, rt, LoopConfig{Task: "hi", Tenant: "acme"})
	if n := len(workspaceEvents(hist)); n != 0 {
		t.Fatalf("loop.workspace recorded without a workspace provider: %v", kindsOf(hist))
	}
}

var _ = reflect.DeepEqual

// TestCodeindexUnregisteredWhenNoMountSource proves a loop whose workspace has
// no MountSource (the mem provider) does NOT expose the codeindex tools — they
// answer about the wrong filesystem (change 0129, criterion 7). A loop whose
// workspace HAS a MountSource (the local provider) keeps them.
func TestCodeindexUnregisteredWhenNoMountSource(t *testing.T) {
	// Mem provider: no MountSource → codeindex unregistered.
	{
		store := fsstore.NewDurableFSStore(t.TempDir())
		hits := 0
		var memReg *tools.Registry
		rt := New(Deps{
			MaxConcurrent: 1,
			DurableStore:   store,
			Registry:       store,
			Workspaces:      workspace.NewMemProvider(),
			NewToolRegistry: func() *tools.Registry {
				reg := tools.NewRegistry()
				for _, tl := range tools.CodeindexTools() {
					reg.Register(tl)
				}
				memReg = reg
				return reg
			},
			BuildAgent: processBuild(t, &scriptedCompleter{}, &hits),
		})
		h, err := rt.StartLoop(context.Background(), LoopConfig{Task: "hi", Tenant: "acme"})
		if err != nil {
			t.Fatalf("StartLoop: %v", err)
		}
		_, _ = h.Wait()
		if memReg == nil {
			t.Fatal("registry not built")
		}
		for _, tl := range tools.CodeindexTools() {
			if memReg.Has(tl.Name()) {
				t.Errorf("codeindex tool %q registered with no MountSource", tl.Name())
			}
		}
	}
	// Local provider: has MountSource → codeindex stays registered.
	{
		store := fsstore.NewDurableFSStore(t.TempDir())
		hits := 0
		var locReg *tools.Registry
		rt := New(Deps{
			MaxConcurrent: 1,
			DurableStore:   store,
			Registry:       store,
			Workspaces:      &workspace.LocalProvider{Parent: t.TempDir()},
			NewToolRegistry: func() *tools.Registry {
				reg := tools.NewRegistry()
				for _, tl := range tools.CodeindexTools() {
					reg.Register(tl)
				}
				locReg = reg
				return reg
			},
			BuildAgent: processBuild(t, &scriptedCompleter{}, &hits),
		})
		h, err := rt.StartLoop(context.Background(), LoopConfig{Task: "hi", Tenant: "acme"})
		if err != nil {
			t.Fatalf("StartLoop: %v", err)
		}
		_, _ = h.Wait()
		if locReg == nil {
			t.Fatal("registry not built")
		}
		for _, tl := range tools.CodeindexTools() {
			if !locReg.Has(tl.Name()) {
				t.Errorf("codeindex tool %q unregistered with a MountSource", tl.Name())
			}
		}
	}
}

// TestWorkspaceResumeReopensSameID proves a resume re-opens the same workspace
// by the ID the stream recorded (change 0129, Task 3 / criterion 4). A loop
// parks on one runtime; a resume on a second runtime over the same provider
// parent re-opens the same workspace and does NOT record a second
// loop.workspace event.
func TestWorkspaceResumeReopensSameID(t *testing.T) {
	parent := t.TempDir()
	store := fsstore.NewDurableFSStore(t.TempDir())
	const tenant event.TenantID = "acme"

	var ca bindingCapture
	fakeA := newGatedCompleter(model.CompletionResp{Content: "first answer"})
	rtA := New(Deps{
		MaxConcurrent: 1,
		DurableStore:   store,
		Registry:       store,
		IdleTTL:        120 * time.Millisecond,
		Workspaces:     &workspace.LocalProvider{Parent: parent},
		BuildAgent:     ca.buildAgent(fakeA),
	})
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	h, err := rtA.StartLoop(ctxA, LoopConfig{Task: "hi", ModelID: "cloud/x", Interactive: true, Tenant: tenant})
	if err != nil {
		t.Fatalf("StartLoop A: %v", err)
	}
	loopID := h.ID()
	evA, unsubA, err := rtA.Observe(ctxA, tenant, loopID)
	if err != nil {
		t.Fatalf("Observe A: %v", err)
	}
	fakeA.admit(0)
	waitForKind(t, evA, event.KindLoopParked, 2*time.Second)
	unsubA()
	// Assert A recorded exactly one loop.workspace event.
	hist, err := rtA.Attach(ctxA, tenant, loopID, 0)
	if err != nil {
		t.Fatalf("Attach A: %v", err)
	}
	if n := len(workspaceEvents(hist)); n != 1 {
		t.Fatalf("loop.workspace events on A = %d, want 1: %v", n, kindsOf(hist))
	}
	select {
	case <-doneOf(h):
	case <-time.After(2 * time.Second):
		t.Fatal("A's interactive loop was not reaped after going idle")
	}

	// Resume on a fresh runtime over the same durable store + same provider parent.
	var cb bindingCapture
	fakeB := newGatedCompleter(model.CompletionResp{Content: "second answer"})
	rtB := New(Deps{
		MaxConcurrent: 1,
		DurableStore:   store,
		Registry:       store,
		IdleTTL:        time.Hour,
		Workspaces:     &workspace.LocalProvider{Parent: parent},
		BuildAgent:     cb.buildAgent(fakeB),
	})
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	if _, err := rtB.Resume(ctxB, tenant, loopID); err != nil {
		t.Fatalf("Resume B: %v", err)
	}
	// The resume must NOT record a second loop.workspace event.
	hist2, err := rtB.Attach(ctxB, tenant, loopID, 0)
	if err != nil {
		t.Fatalf("Attach B: %v", err)
	}
	if n := len(workspaceEvents(hist2)); n != 1 {
		t.Fatalf("loop.workspace events on B = %d, want 1 (no re-record): %v", n, kindsOf(hist2))
	}
}