package runtime

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/artifact"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/tools"
)

// bindingCapture records the artifact binding each BuildAgent call sees on the
// loop's tool registry.
type bindingCapture struct {
	mu   sync.Mutex
	seen []artifact.Binding
}

func (c *bindingCapture) buildAgent(comp agent.Completer) func(event.EventStore, *agent.AgentTree, string, *tools.Registry) (*agent.Agent, error) {
	return func(_ event.EventStore, _ *agent.AgentTree, modelID string, reg *tools.Registry) (*agent.Agent, error) {
		c.mu.Lock()
		c.seen = append(c.seen, reg.Artifacts())
		c.mu.Unlock()
		return agent.New(comp, execAll{reg}, nopRenderer{}, modelID, "", 1, 0), nil
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

func runOneLoop(t *testing.T, rt Runtime) string {
	t.Helper()
	h, err := rt.StartLoop(context.Background(), LoopConfig{Task: "hi", ModelID: "cloud/x"})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	return h.ID()
}

func TestStartLoopBindsRegistryToLoopKey(t *testing.T) {
	store := artifact.NewMemStore()
	var c bindingCapture
	rt := New(Deps{
		BaseDir:       t.TempDir(),
		MaxConcurrent: 1,
		Artifacts:     store,
		BuildAgent:    c.buildAgent(&scriptedCompleter{}),
	})
	id := runOneLoop(t, rt)
	b := c.last(t)
	if b.Tenant != event.DefaultTenant {
		t.Errorf("binding tenant = %q, want %q", b.Tenant, event.DefaultTenant)
	}
	if b.Loop != event.LoopID(id) {
		t.Errorf("binding loop = %q, want %q", b.Loop, id)
	}
	if b.Store != artifact.Store(store) {
		t.Errorf("binding store = %v, want Deps.Artifacts", b.Store)
	}
}

func TestStartLoopNilArtifactsDefaultsToCappedMemStore(t *testing.T) {
	var c bindingCapture
	rt := New(Deps{
		BaseDir:       t.TempDir(),
		MaxConcurrent: 1,
		BuildAgent:    c.buildAgent(&scriptedCompleter{}),
	})
	runOneLoop(t, rt)
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
	id := runOneLoop(t, rt)
	b := c.last(t)
	if b.Store != artifact.Store(store) || b.Tenant != event.DefaultTenant || b.Loop != event.LoopID(id) {
		t.Fatalf("binding = %+v, want the runtime's (store, default tenant, %q)", b, id)
	}
}
