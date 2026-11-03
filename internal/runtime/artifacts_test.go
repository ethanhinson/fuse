package runtime

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/artifact"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// bindingCapture records the artifact binding each BuildAgent call sees on the
// loop's tool registry.
type bindingCapture struct {
	mu   sync.Mutex
	seen []artifact.Binding
}

func (c *bindingCapture) buildAgent(comp agent.Completer) func(event.EventStore, *agent.AgentTree, string, *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
	return func(_ event.EventStore, _ *agent.AgentTree, modelID string, reg *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
		c.mu.Lock()
		c.seen = append(c.seen, reg.Artifacts())
		c.mu.Unlock()
		return agent.New(comp, execAll{reg}, nopRenderer{}, modelID, "", 1, 0), nil, modelID, nil
	}
}

func (c *bindingCapture) last(t *testing.T) artifact.Binding {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.seen) == 0 {
		t.Fatal("BuildAgent was never called")
	}
	return c.seen[len(c.seen)-1]
}

func runOneLoop(t *testing.T, rt Runtime, tenant event.TenantID) string {
	t.Helper()
	h, err := rt.StartLoop(context.Background(), LoopConfig{Task: "hi", ModelID: "cloud/x", Tenant: tenant})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	return h.ID()
}

func TestStartLoopBindsRegistryToLoopKey(t *testing.T) {
	for _, tenant := range []event.TenantID{"", "acme"} {
		store := artifact.NewMemStore()
		var c bindingCapture
		rt := New(Deps{
			BaseDir:       t.TempDir(),
			MaxConcurrent: 1,
			Artifacts:     store,
			BuildAgent:    c.buildAgent(&scriptedCompleter{}),
		})
		id := runOneLoop(t, rt, tenant)
		b := c.last(t)
		if b.Tenant != event.NormalizeTenant(tenant) {
			t.Errorf("tenant %q: binding tenant = %q, want %q", tenant, b.Tenant, event.NormalizeTenant(tenant))
		}
		if b.Loop != event.LoopID(id) {
			t.Errorf("tenant %q: binding loop = %q, want %q", tenant, b.Loop, id)
		}
		if b.Store != artifact.Store(store) {
			t.Errorf("tenant %q: binding store = %v, want Deps.Artifacts", tenant, b.Store)
		}
	}
}

func TestStartLoopNilArtifactsDefaultsToCappedMemStore(t *testing.T) {
	var c bindingCapture
	rt := New(Deps{
		BaseDir:       t.TempDir(),
		MaxConcurrent: 1,
		BuildAgent:    c.buildAgent(&scriptedCompleter{}),
	})
	runOneLoop(t, rt, "")
	b := c.last(t)
	if !b.Valid() {
		t.Fatalf("binding with nil Deps.Artifacts is not valid: %+v", b)
	}
	ctx := context.Background()
	chunk := bytes.Repeat([]byte("x"), artifact.MaxArtifactBytes)
	var written int64
	for written+int64(len(chunk)) <= artifact.DefaultMemLoopCap {
		if _, err := b.Put(ctx, artifact.KindSpill, bytes.NewReader(chunk), artifact.Meta{}); err != nil {
			t.Fatalf("Put within the cap (after %d bytes): %v", written, err)
		}
		written += int64(len(chunk))
	}
	rest := artifact.DefaultMemLoopCap + 1 - written
	_, err := b.Put(ctx, artifact.KindSpill, bytes.NewReader(bytes.Repeat([]byte("y"), int(rest))), artifact.Meta{})
	if !errors.Is(err, artifact.ErrQuotaExceeded) {
		t.Fatalf("Put past DefaultMemLoopCap: err = %v, want ErrQuotaExceeded", err)
	}
}

func TestResumeRebindsSameLoop(t *testing.T) {
	dir := t.TempDir()
	store := fsstore.NewDurableFSStore(dir)
	var reg event.LoopRegistry = store
	arts := artifact.NewMemStore()
	const tenant event.TenantID = "acme"

	var ca bindingCapture
	fakeA := newGatedCompleter(model.CompletionResp{Content: "first answer"})
	rtA := New(Deps{
		DurableStore:  store,
		Registry:      reg,
		MaxConcurrent: 1,
		IdleTTL:       120 * time.Millisecond,
		Artifacts:     arts,
		BuildAgent:    ca.buildAgent(fakeA),
	})
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	hA, err := rtA.StartLoop(ctxA, LoopConfig{Task: "hello", ModelID: "cloud/x", Interactive: true, Tenant: tenant})
	if err != nil {
		t.Fatalf("A StartLoop: %v", err)
	}
	loopID := hA.ID()
	evA, unsubA, err := rtA.Observe(ctxA, tenant, loopID)
	if err != nil {
		t.Fatalf("A Observe: %v", err)
	}
	fakeA.admit(0)
	waitForKind(t, evA, event.KindLoopParked, 2*time.Second)
	unsubA()
	select {
	case <-doneOf(hA):
	case <-time.After(2 * time.Second):
		t.Fatal("A's interactive loop was not reaped after going idle")
	}

	var cb bindingCapture
	fakeB := newGatedCompleter(model.CompletionResp{Content: "second answer"})
	rtB := New(Deps{
		DurableStore:  store,
		Registry:      reg,
		MaxConcurrent: 1,
		IdleTTL:       time.Hour,
		Artifacts:     arts,
		BuildAgent:    cb.buildAgent(fakeB),
	})
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	if _, err := rtB.Resume(ctxB, tenant, loopID); err != nil {
		t.Fatalf("B Resume: %v", err)
	}
	b := cb.last(t)
	if b.Loop != event.LoopID(loopID) || b.Tenant != tenant || b.Store != artifact.Store(arts) {
		t.Fatalf("resumed binding = %+v, want (store, %q, %q)", b, tenant, loopID)
	}
}

func TestRuntimeBindingOverridesBindingSuppliedOne(t *testing.T) {
	store := artifact.NewMemStore()
	other := artifact.NewMemStore()
	var c bindingCapture
	rt := New(Deps{
		BaseDir:       t.TempDir(),
		MaxConcurrent: 1,
		Artifacts:     store,
		NewToolRegistry: func() *tools.Registry {
			return tools.NewRegistry().WithArtifacts(other, "other", "other")
		},
		BuildAgent: c.buildAgent(&scriptedCompleter{}),
	})
	id := runOneLoop(t, rt, "acme")
	b := c.last(t)
	if b.Store != artifact.Store(store) || b.Tenant != "acme" || b.Loop != event.LoopID(id) {
		t.Fatalf("binding = %+v, want the runtime's (store, acme, %q)", b, id)
	}
}
