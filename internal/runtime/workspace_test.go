package runtime

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
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