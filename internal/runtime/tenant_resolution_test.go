package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// TestSendResolvesUnderRequestedTenant is the FIX-1 (tenant threading on Send) guard:
// a Send under a NON-DEFAULT tenant must resolve against THAT tenant's registry record,
// not tenant _default's. A loop registered only under _default whose run has finished
// resolves as ErrLoopFinished when Send targets _default, but must resolve as
// ErrLoopNotFound when Send targets a different tenant (whose registry has no such
// record) — proving Send no longer hardcodes event.DefaultTenant on the durable
// fall-through.
func TestSendResolvesUnderRequestedTenant(t *testing.T) {
	dir := t.TempDir()
	store := fsstore.NewDurableFSStore(dir)
	var reg event.LoopRegistry = store

	buildAgent := func(s event.EventStore, tree *agent.AgentTree, modelID string, r *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
		fake := &scriptedCompleter{responses: []model.CompletionResp{{Content: "done"}}}
		return agent.New(fake, execAll{r}, nopRenderer{}, modelID, "", 1, 0), nil, modelID, nil
	}

	// Runtime A starts + finishes a loop under the DEFAULT tenant, then a FRESH runtime
	// B (empty cache) resolves it via the durable registry — so all Send resolution goes
	// through resolveDurable(tenant, loopID), the path FIX 1 tenant-threads.
	rtA := New(Deps{DurableStore: store, Registry: reg, MaxConcurrent: 1, BuildAgent: buildAgent})
	h, err := rtA.StartLoop(context.Background(), LoopConfig{Task: "hi", ModelID: "cloud/x", Tenant: event.DefaultTenant})
	if err != nil {
		t.Fatalf("A StartLoop: %v", err)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatalf("A Wait: %v", err)
	}
	loopID := h.ID()

	rtB := New(Deps{DurableStore: store, Registry: reg, MaxConcurrent: 1, BuildAgent: buildAgent})

	// Send under the DEFAULT tenant resolves the finished record ⇒ ErrLoopFinished.
	if err := rtB.Send(context.Background(), event.DefaultTenant, loopID, "hi"); !errors.Is(err, ErrLoopFinished) {
		t.Fatalf("Send under default tenant: want ErrLoopFinished, got %v", err)
	}

	// Send under a DIFFERENT tenant must NOT resolve tenant _default's record. That
	// tenant has no registry entry for this loopID, so it is ErrLoopNotFound — proving
	// Send does not cross tenants (the FIX-1 gap: a hardcoded DefaultTenant here would
	// have wrongly returned ErrLoopFinished).
	if err := rtB.Send(context.Background(), event.TenantID("tenant-x"), loopID, "hi"); !errors.Is(err, ErrLoopNotFound) {
		t.Fatalf("Send under tenant-x for a _default-only loop: want ErrLoopNotFound, got %v", err)
	}
}

// TestCacheHitDoesNotCrossTenants is the FIX-2 (tenant-check on cache hit) guard. The
// r.loops cache is keyed by BARE loopID while the durable layer is strictly
// tenant-scoped. A live loop cached under tenant X must never satisfy a request under
// tenant Y: on the tenant mismatch the cache hit is treated as a miss and falls through
// to the tenant-scoped durable registry (which, having no tenant-Y record, returns
// not-found) — Observe/Attach/Send must not return tenant X's live store.
func TestCacheHitDoesNotCrossTenants(t *testing.T) {
	dir := t.TempDir()
	store := fsstore.NewDurableFSStore(dir)
	var reg event.LoopRegistry = store

	release := make(chan struct{})
	buildAgent := func(s event.EventStore, tree *agent.AgentTree, modelID string, r *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
		return agent.New(blockingCompleter{release: release}, execAll{r}, nopRenderer{}, modelID, "", 1, 0), nil, modelID, nil
	}

	rt := New(Deps{DurableStore: store, Registry: reg, MaxConcurrent: 1, BuildAgent: buildAgent})

	// Start a live loop under tenant X (the completer blocks, so it stays in the cache).
	tenantX := event.TenantID("tenant-x")
	h, err := rt.StartLoop(context.Background(), LoopConfig{Task: "hi", ModelID: "cloud/x", Tenant: tenantX})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	loopID := h.ID()
	// Unblock the run and await completion before TempDir cleanup, so the loop's fsstore
	// writer is closed before the temp dir is removed (the assertions below hold while the
	// loop is still live in the cache — completion does not affect tenant scoping).
	t.Cleanup(func() {
		close(release)
		_, _ = h.Wait()
	})

	// Sanity: under the CORRECT tenant the cache hit resolves (Observe succeeds).
	ch, cancel, err := rt.Observe(context.Background(), tenantX, loopID)
	if err != nil {
		t.Fatalf("Observe under correct tenant: %v", err)
	}
	cancel()
	_ = ch

	// Under a DIFFERENT tenant the bare-loopID cache hit must NOT return tenant X's
	// store. The tenant mismatch falls through to the durable registry, which has no
	// tenant-Y record ⇒ ErrLoopNotFound. A regression (returning tenant X's store) would
	// succeed here.
	tenantY := event.TenantID("tenant-y")
	if _, _, err := rt.Observe(context.Background(), tenantY, loopID); !errors.Is(err, ErrLoopNotFound) {
		t.Fatalf("Observe under wrong tenant: want ErrLoopNotFound (no cross-tenant cache hit), got %v", err)
	}
	if _, err := rt.Attach(context.Background(), tenantY, loopID, 0); !errors.Is(err, ErrLoopNotFound) {
		t.Fatalf("Attach under wrong tenant: want ErrLoopNotFound, got %v", err)
	}
	// Send under the wrong tenant: the loop IS live under tenant X, but under tenant Y it
	// must not enqueue on tenant X's human-bus. The durable registry has no tenant-Y
	// record ⇒ ErrLoopNotFound.
	if err := rt.Send(context.Background(), tenantY, loopID, "cross"); !errors.Is(err, ErrLoopNotFound) {
		t.Fatalf("Send under wrong tenant: want ErrLoopNotFound (no cross-tenant enqueue), got %v", err)
	}
}
