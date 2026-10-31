package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/ethanhinson/fuse/harness"
	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// fakeHarness is a HarnessResolver that records each call and hands back a
// factory building agents over its completer, or a fixed error.
type fakeHarness struct {
	mu        sync.Mutex
	calls     []*harnessv1.HarnessSpec
	tenants   []event.TenantID
	err       error
	completer agent.Completer
	built     int
	regs      int
}

func (f *fakeHarness) ResolveHarness(_ context.Context, tenant event.TenantID, spec *harnessv1.HarnessSpec) (LoopFactory, error) {
	f.mu.Lock()
	f.calls = append(f.calls, spec)
	f.tenants = append(f.tenants, tenant)
	f.mu.Unlock()
	if f.err != nil {
		return LoopFactory{}, f.err
	}
	return LoopFactory{
		MaxConcurrent: 1,
		NewToolRegistry: func() *tools.Registry {
			f.mu.Lock()
			f.regs++
			f.mu.Unlock()
			return tools.NewRegistry()
		},
		BuildAgent: func(s event.EventStore, tree *agent.AgentTree, modelID string, r *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			f.mu.Lock()
			f.built++
			f.mu.Unlock()
			return agent.New(f.completer, execAll{r}, nopRenderer{}, "spec-model", "", 1, 0), nil, "spec-model", nil
		},
	}, nil
}

func (f *fakeHarness) snapshot() (calls []*harnessv1.HarnessSpec, tenants []event.TenantID, built, regs int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*harnessv1.HarnessSpec(nil), f.calls...), append([]event.TenantID(nil), f.tenants...), f.built, f.regs
}

// processBuild is the deps-level BuildAgent: a loop with a spec must never reach it.
func processBuild(t *testing.T, c agent.Completer, hits *int) func(event.EventStore, *agent.AgentTree, string, *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
	return func(s event.EventStore, tree *agent.AgentTree, modelID string, r *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
		*hits++
		return agent.New(c, execAll{r}, nopRenderer{}, modelID, "", 1, 0), nil, modelID, nil
	}
}

func testSpec() *harnessv1.HarnessSpec {
	turns := uint32(2)
	return &harnessv1.HarnessSpec{
		Models:   &harnessv1.Models{DefaultAlias: "glm"},
		MaxTurns: &turns,
		Skills:   []*harnessv1.SkillRef{{Name: "research"}},
	}
}

func harnessEvents(evs []event.Event) []event.Event {
	var out []event.Event
	for _, e := range evs {
		if e.Kind == event.KindLoopHarness {
			out = append(out, e)
		}
	}
	return out
}

func TestStartLoopWithHarnessBuildsFromTheResolverFactory(t *testing.T) {
	fh := &fakeHarness{completer: &scriptedCompleter{}}
	hits := 0
	rt := New(Deps{
		MaxConcurrent:  1,
		InMemoryEvents: true,
		Harness:        fh,
		NewToolRegistry: func() *tools.Registry {
			t.Error("deps NewToolRegistry used for a spec loop")
			return tools.NewRegistry()
		},
		BuildAgent: processBuild(t, &scriptedCompleter{}, &hits),
	})
	spec := testSpec()
	h, err := rt.StartLoop(context.Background(), LoopConfig{Task: "hi", Tenant: "acme", Harness: spec})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	calls, tenants, built, regs := fh.snapshot()
	if len(calls) != 1 || !proto.Equal(calls[0], spec) || tenants[0] != "acme" {
		t.Fatalf("resolver calls = %v tenants = %v", calls, tenants)
	}
	if built != 1 || regs != 1 || hits != 0 {
		t.Fatalf("factory built=%d regs=%d, deps BuildAgent hits=%d", built, regs, hits)
	}
}

func TestStartLoopWithoutHarnessNeverCallsTheResolver(t *testing.T) {
	fh := &fakeHarness{completer: &scriptedCompleter{}}
	hits := 0
	rt := New(Deps{MaxConcurrent: 1, Harness: fh, BuildAgent: processBuild(t, &scriptedCompleter{}, &hits)})
	h, err := rt.StartLoop(context.Background(), LoopConfig{Task: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatal(err)
	}
	if calls, _, _, _ := fh.snapshot(); len(calls) != 0 || hits != 1 {
		t.Fatalf("resolver calls = %d, deps BuildAgent hits = %d", len(calls), hits)
	}
}

func TestStartLoopWithHarnessButNoResolverIsUnsupported(t *testing.T) {
	hits := 0
	rt := New(Deps{MaxConcurrent: 1, BuildAgent: processBuild(t, &scriptedCompleter{}, &hits)})
	_, err := rt.StartLoop(context.Background(), LoopConfig{Task: "hi", Harness: testSpec()})
	if !errors.Is(err, ErrHarnessUnsupported) {
		t.Fatalf("err = %v, want ErrHarnessUnsupported", err)
	}
	if hits != 0 {
		t.Fatal("an unsupported spec still built an agent")
	}
}

func TestStartLoopWithInvalidHarnessCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	store := fsstore.NewDurableFSStore(dir)
	reg := newFakeRegistry()
	ve := &harness.ValidationError{Violations: []harness.Violation{{FieldPath: "skills[0].name", Message: "nope", RuleID: harness.RuleSkillRef}}}
	fh := &fakeHarness{err: ve}
	hits := 0
	rt := New(Deps{MaxConcurrent: 1, DurableStore: store, Registry: reg, Harness: fh, BuildAgent: processBuild(t, &scriptedCompleter{}, &hits)})
	_, err := rt.StartLoop(context.Background(), LoopConfig{Task: "hi", Harness: testSpec()})
	var got *harness.ValidationError
	if !errors.As(err, &got) || got.Violations[0].FieldPath != "skills[0].name" {
		t.Fatalf("err = %v, want the resolver's ValidationError", err)
	}
	if hits != 0 {
		t.Fatal("agent built for an invalid spec")
	}
	if n := len(rt.(*inProcRuntime).loops); n != 0 {
		t.Fatalf("%d loops held after a refused spec", n)
	}
	if n := reg.count(); n != 0 {
		t.Fatalf("%d registry records after a refused spec", n)
	}
}

func TestHarnessIsRecordedOnceAndResumeRebuildsFromIt(t *testing.T) {
	dir := t.TempDir()
	store := fsstore.NewDurableFSStore(dir)
	var reg event.LoopRegistry = store
	spec := testSpec()

	fhA := &fakeHarness{completer: newGatedCompleter(model.CompletionResp{Content: "first"})}
	rtA := New(Deps{DurableStore: store, Registry: reg, MaxConcurrent: 1, IdleTTL: 120 * time.Millisecond, Harness: fhA})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hA, err := rtA.StartLoop(ctx, LoopConfig{Task: "hello", Tenant: "acme", Interactive: true, Harness: spec})
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

	hist, err := rtA.Attach(ctx, "acme", loopID, 0)
	if err != nil {
		t.Fatal(err)
	}
	recs := harnessEvents(hist)
	if len(recs) != 1 {
		t.Fatalf("loop.harness events = %d, want 1: %v", len(recs), kindsOf(hist))
	}
	if hist[0].Kind != event.KindLoopHarness {
		t.Errorf("loop.harness is not the first event: %v", kindsOf(hist))
	}
	var p event.LoopHarnessPayload
	if err := json.Unmarshal(recs[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Digest != harness.Digest(spec) {
		t.Errorf("digest = %q, want %q", p.Digest, harness.Digest(spec))
	}
	var recorded harnessv1.HarnessSpec
	if err := protojson.Unmarshal(p.Spec, &recorded); err != nil || !proto.Equal(&recorded, spec) {
		t.Fatalf("recorded spec = %v (%v), want %v", &recorded, err, spec)
	}

	// A fresh replica with no spec of its own resumes the loop from the record.
	fhB := &fakeHarness{completer: newGatedCompleter(model.CompletionResp{Content: "second"})}
	hitsB := 0
	rtB := New(Deps{DurableStore: store, Registry: reg, MaxConcurrent: 1, IdleTTL: time.Hour, Harness: fhB,
		BuildAgent: processBuild(t, &scriptedCompleter{}, &hitsB)})
	if _, err := rtB.Resume(ctx, "acme", loopID); err != nil {
		t.Fatalf("B Resume: %v", err)
	}
	calls, tenants, built, _ := fhB.snapshot()
	if len(calls) != 1 || !proto.Equal(calls[0], spec) || tenants[0] != "acme" || built != 1 || hitsB != 0 {
		t.Fatalf("resume did not rebuild from the recorded spec: calls=%v tenants=%v built=%d deps hits=%d", calls, tenants, built, hitsB)
	}
	hist, err = rtB.Attach(ctx, "acme", loopID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(harnessEvents(hist)); n != 1 {
		t.Fatalf("a resume re-recorded the harness: %d loop.harness events", n)
	}
}

func TestResumeRefusesATamperedHarnessRecord(t *testing.T) {
	dir := t.TempDir()
	store := fsstore.NewDurableFSStore(dir)
	ctx := context.Background()
	key := event.StreamKey{Tenant: event.DefaultTenant, Loop: "tampered"}
	if err := store.Register(ctx, event.LoopRecord{Key: key, OwnerNodeID: "x"}); err != nil {
		t.Fatal(err)
	}
	specJSON, _ := protojson.Marshal(testSpec())
	payload, _ := json.Marshal(event.LoopHarnessPayload{Spec: specJSON, SpecBytes: harness.Encode(testSpec()), Digest: "sha256:00"})
	if err := store.Append(ctx, key, event.Event{Kind: event.KindLoopHarness, NodeID: "tampered", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	fh := &fakeHarness{completer: newGatedCompleter()}
	rt := New(Deps{DurableStore: store, Registry: store, MaxConcurrent: 1, Harness: fh})
	_, err := rt.Resume(ctx, event.DefaultTenant, "tampered")
	if err == nil {
		t.Fatal("Resume accepted a harness record whose digest does not match")
	}
	if calls, _, _, _ := fh.snapshot(); len(calls) != 0 {
		t.Fatal("a tampered spec reached the resolver")
	}
}

// count is the number of loop records the registry holds.
func (f *fakeRegistry) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.records)
}

// TestHarnessRecordKeepsFieldsThisReplicaDoesNotKnow: a record written by a
// newer replica (a field this binary has no descriptor for) still verifies and
// resumes here, and keeps the field, because the bytes are what is recorded.
func TestHarnessRecordKeepsFieldsThisReplicaDoesNotKnow(t *testing.T) {
	b := append(harness.Encode(testSpec()), 0xf8, 0x06, 0x01) // field 111, varint 1
	payload, _ := json.Marshal(event.LoopHarnessPayload{SpecBytes: b, Digest: harness.DigestBytes(b)})
	spec, err := harnessFromEvents([]event.Event{{Kind: event.KindLoopHarness, Payload: payload}})
	if err != nil {
		t.Fatalf("harnessFromEvents: %v", err)
	}
	if len(spec.ProtoReflect().GetUnknown()) == 0 {
		t.Fatal("the unknown field was dropped")
	}
	if spec.GetModels().GetDefaultAlias() != "glm" || spec.GetMaxTurns() != 2 {
		t.Fatalf("known fields lost: %v", spec)
	}
	if got := harness.Encode(spec); harness.DigestBytes(got) != harness.DigestBytes(b) {
		t.Fatalf("re-encoding changed the digest: %x vs %x", got, b)
	}
}

// failingAppendStore refuses every append, as an unreachable durable store does.
type failingAppendStore struct{ event.DurableStore }

func (failingAppendStore) Append(context.Context, event.StreamKey, event.Event) error {
	return errors.New("store down")
}

func TestFailedHarnessRecordLeavesTheLoopNotLive(t *testing.T) {
	reg := newFakeRegistry()
	fh := &fakeHarness{completer: &scriptedCompleter{}}
	store := failingAppendStore{fsstore.NewDurableFSStore(t.TempDir())}
	rt := New(Deps{MaxConcurrent: 1, DurableStore: store, Registry: reg, Harness: fh})
	if _, err := rt.StartLoop(context.Background(), LoopConfig{Task: "hi", Harness: testSpec()}); err == nil {
		t.Fatal("StartLoop succeeded without recording its harness")
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
	if _, _, built, _ := fh.snapshot(); built != 0 {
		t.Fatal("an agent was built for a loop whose harness was not recorded")
	}
}
