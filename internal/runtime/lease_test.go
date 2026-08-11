package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// fakeRegistry is an in-memory event.LoopRegistry that records Heartbeat calls so a
// test can assert the renewer runs (and stops). It is tenant-scoped by StreamKey.
type fakeRegistry struct {
	mu         sync.Mutex
	records    map[event.StreamKey]event.LoopRecord
	heartbeats int             // total Heartbeat calls
	hbKeys     []event.StreamKey // keys observed by Heartbeat, in order
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{records: map[event.StreamKey]event.LoopRecord{}}
}

func (f *fakeRegistry) Register(ctx context.Context, rec event.LoopRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Idempotent upsert; preserve CreatedAt on re-register.
	if prev, ok := f.records[rec.Key]; ok && !prev.CreatedAt.IsZero() {
		rec.CreatedAt = prev.CreatedAt
	}
	f.records[rec.Key] = rec
	return nil
}

func (f *fakeRegistry) SetLive(ctx context.Context, key event.StreamKey, live bool, ownerNodeID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.records[key]
	if !ok {
		return event.ErrLoopUnknown
	}
	rec.Live = live
	rec.OwnerNodeID = ownerNodeID
	f.records[key] = rec
	return nil
}

func (f *fakeRegistry) Heartbeat(ctx context.Context, key event.StreamKey, ownerNodeID string, expiry time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.records[key]
	if !ok {
		return event.ErrLoopUnknown
	}
	rec.LeaseExpiry = expiry
	rec.OwnerNodeID = ownerNodeID
	f.records[key] = rec
	f.heartbeats++
	f.hbKeys = append(f.hbKeys, key)
	return nil
}

func (f *fakeRegistry) Resolve(ctx context.Context, key event.StreamKey) (event.LoopRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.records[key]
	if !ok {
		return event.LoopRecord{}, event.ErrLoopUnknown
	}
	return rec, nil
}

func (f *fakeRegistry) List(ctx context.Context, tenant event.TenantID) ([]event.LoopRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tenant = event.NormalizeTenant(tenant)
	var out []event.LoopRecord
	for _, rec := range f.records {
		if rec.Key.Tenant == tenant {
			out = append(out, rec)
		}
	}
	return out, nil
}

func (f *fakeRegistry) heartbeatCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.heartbeats
}

func (f *fakeRegistry) record(key event.StreamKey) (event.LoopRecord, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.records[key]
	return rec, ok
}

func (f *fakeRegistry) seed(rec event.LoopRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records[rec.Key] = rec
}

// TestHeartbeatRenewerRunsWhileLiveAndStopsAtCompletion drives a loop with a short TTL
// and a fast heartbeat interval and asserts the renewer emits multiple Heartbeats while
// the run is live, then STOPS once the loop completes (no goroutine keeps ticking).
func TestHeartbeatRenewerRunsWhileLiveAndStopsAtCompletion(t *testing.T) {
	reg := newFakeRegistry()
	release := make(chan struct{})

	buildAgent := func(s event.EventStore, tree *agent.AgentTree, modelID string, r *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
		return agent.New(blockingCompleter{release: release}, execAll{r}, nopRenderer{}, modelID, "", 1, 0), nil, modelID, nil
	}

	rt := New(Deps{
		Registry:          reg,
		MaxConcurrent:     1,
		BuildAgent:        buildAgent,
		LeaseTTL:          90 * time.Millisecond,
		HeartbeatInterval: 5 * time.Millisecond, // fast tick for the test
	})

	h, err := rt.StartLoop(context.Background(), LoopConfig{Task: "hi", ModelID: "cloud/x"})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	key := event.StreamKey{Tenant: event.DefaultTenant, Loop: event.LoopID(h.ID())}

	// The loop is blocked (completer waits on release). The renewer should tick.
	deadline := time.Now().Add(2 * time.Second)
	for reg.heartbeatCount() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("renewer did not emit >=2 heartbeats while live (got %d)", reg.heartbeatCount())
		}
		time.Sleep(5 * time.Millisecond)
	}

	// LeaseExpiry must have been set (non-zero) and be in the future relative to Register.
	if rec, ok := reg.record(key); !ok || rec.LeaseExpiry.IsZero() {
		t.Fatalf("expected a non-zero LeaseExpiry on the live record, got %+v (ok=%v)", rec, ok)
	}

	// Let the loop finish; the renewer must stop.
	close(release)
	if _, err := h.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	// Give the renewer a moment to observe ctx/done and exit, then confirm it stopped
	// emitting: sample the count, wait several tick intervals, and require no growth.
	time.Sleep(60 * time.Millisecond)
	stable := reg.heartbeatCount()
	time.Sleep(80 * time.Millisecond)
	if grew := reg.heartbeatCount(); grew != stable {
		t.Fatalf("renewer kept ticking after completion: %d -> %d", stable, grew)
	}

	// After completion the record is not-live (SetLive(false) landed).
	if rec, _ := reg.record(key); rec.Live {
		t.Fatalf("record should be not-live after completion, got %+v", rec)
	}
}

// TestResolveReOwnsExpiredLease seeds a Live record whose lease has already expired and
// asserts resolveDurable does NOT surface it as ErrLoopNotFound and refreshes the lease
// (re-own under this instance) rather than treating it as a hard error.
func TestResolveReOwnsExpiredLease(t *testing.T) {
	reg := newFakeRegistry()
	rt := New(Deps{Registry: reg, LeaseTTL: 30 * time.Second}).(*inProcRuntime)

	// Freeze a clock so "now" is deterministic.
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	rt.deps.Now = func() time.Time { return now }

	key := event.StreamKey{Tenant: event.DefaultTenant, Loop: event.LoopID("abandoned")}
	reg.seed(event.LoopRecord{
		Key:         key,
		OwnerNodeID: "dead-node",
		Owner:       "alice",
		Live:        true,
		LeaseExpiry: now.Add(-1 * time.Minute), // already expired
	})

	rec, gotKey, err := rt.resolveDurable(context.Background(), event.DefaultTenant, "abandoned")
	if err != nil {
		t.Fatalf("resolveDurable of an expired-lease live loop must not error, got %v", err)
	}
	if errors.Is(err, ErrLoopNotFound) {
		t.Fatal("expired-lease live loop must not be ErrLoopNotFound")
	}
	if gotKey != key {
		t.Fatalf("key mismatch: %+v", gotKey)
	}
	// The lease must have been refreshed to now+TTL under this instance.
	stored, _ := reg.record(key)
	if !stored.LeaseExpiry.After(now) {
		t.Fatalf("lease should be renewed to a future time, got %v (now=%v)", stored.LeaseExpiry, now)
	}
	if reg.heartbeatCount() == 0 {
		t.Fatal("expected a re-own Heartbeat on the expired-lease resolve")
	}
	// The returned record reflects the refreshed lease.
	if !rec.LeaseExpiry.After(now) {
		t.Fatalf("returned record lease not refreshed: %v", rec.LeaseExpiry)
	}
	// Owner (authorization subject) is left intact by re-own.
	if stored.Owner != "alice" {
		t.Fatalf("re-own must not change Owner, got %q", stored.Owner)
	}
}

// TestResolveLeavesNonExpiredLiveRecordUntouched proves a live record whose lease is
// still valid is returned as-is with NO reap/re-own Heartbeat.
func TestResolveLeavesNonExpiredLiveRecordUntouched(t *testing.T) {
	reg := newFakeRegistry()
	rt := New(Deps{Registry: reg, LeaseTTL: 30 * time.Second}).(*inProcRuntime)
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	rt.deps.Now = func() time.Time { return now }

	key := event.StreamKey{Tenant: event.DefaultTenant, Loop: event.LoopID("healthy")}
	reg.seed(event.LoopRecord{
		Key:         key,
		OwnerNodeID: "live-node",
		Live:        true,
		LeaseExpiry: now.Add(1 * time.Minute), // still valid
	})

	if _, _, err := rt.resolveDurable(context.Background(), event.DefaultTenant, "healthy"); err != nil {
		t.Fatalf("resolveDurable of a healthy live loop: %v", err)
	}
	if reg.heartbeatCount() != 0 {
		t.Fatalf("a non-expired live record must NOT be reaped, got %d heartbeats", reg.heartbeatCount())
	}
}

// TestResolveNotLiveRecordServedReplay proves a completed (not-live) record is returned
// for replay/observe with no re-own (lease irrelevant once not-live).
func TestResolveNotLiveRecordServedReplay(t *testing.T) {
	reg := newFakeRegistry()
	rt := New(Deps{Registry: reg, LeaseTTL: 30 * time.Second}).(*inProcRuntime)
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	rt.deps.Now = func() time.Time { return now }

	key := event.StreamKey{Tenant: event.DefaultTenant, Loop: event.LoopID("finished")}
	reg.seed(event.LoopRecord{
		Key:         key,
		Live:        false,
		LeaseExpiry: now.Add(-1 * time.Hour), // expired, but not-live so irrelevant
	})

	rec, _, err := rt.resolveDurable(context.Background(), event.DefaultTenant, "finished")
	if err != nil {
		t.Fatalf("resolveDurable of a finished loop: %v", err)
	}
	if rec.Live {
		t.Fatal("finished record should stay not-live")
	}
	if reg.heartbeatCount() != 0 {
		t.Fatalf("a not-live record must NOT be re-owned, got %d heartbeats", reg.heartbeatCount())
	}
}

// TestStartLoopNoRegistryNoRenewer proves the nil-registry (single-loop CLI) path still
// starts a loop, launches no renewer, and never panics.
func TestStartLoopNoRegistryNoRenewer(t *testing.T) {
	fake := &scriptedCompleter{responses: []model.CompletionResp{{Content: "final"}}}
	rt := New(Deps{
		BaseDir:       t.TempDir(),
		MaxConcurrent: 1,
		LeaseTTL:      10 * time.Millisecond,
		BuildAgent: func(store event.EventStore, tree *agent.AgentTree, modelID string, reg *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			return agent.New(fake, execAll{reg}, nopRenderer{}, modelID, "", 1, 0), nil, modelID, nil
		},
	})
	h, err := rt.StartLoop(context.Background(), LoopConfig{Task: "hi", ModelID: "cloud/x"})
	if err != nil {
		t.Fatalf("StartLoop with nil registry: %v", err)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	// No registry means resolveDurable returns ErrLoopNotFound (in-memory only).
	rti := rt.(*inProcRuntime)
	if _, _, err := rti.resolveDurable(context.Background(), event.DefaultTenant, h.ID()); !errors.Is(err, ErrLoopNotFound) {
		t.Fatalf("nil-registry resolveDurable: want ErrLoopNotFound, got %v", err)
	}
}

// TestRenewerExitsOnContextCancel proves the renewer goroutine exits when the loop's run
// context is canceled (no leak), stopping heartbeats.
func TestRenewerExitsOnContextCancel(t *testing.T) {
	reg := newFakeRegistry()
	release := make(chan struct{})
	buildAgent := func(s event.EventStore, tree *agent.AgentTree, modelID string, r *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
		return agent.New(cancelAwareCompleter{release: release}, execAll{r}, nopRenderer{}, modelID, "", 1, 0), nil, modelID, nil
	}
	rt := New(Deps{
		Registry:          reg,
		MaxConcurrent:     1,
		BuildAgent:        buildAgent,
		LeaseTTL:          90 * time.Millisecond,
		HeartbeatInterval: 5 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	h, err := rt.StartLoop(ctx, LoopConfig{Task: "hi", ModelID: "cloud/x"})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	// Wait for at least one heartbeat, then cancel the run ctx.
	deadline := time.Now().Add(2 * time.Second)
	for reg.heartbeatCount() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("no heartbeat before cancel")
		}
		time.Sleep(3 * time.Millisecond)
	}
	cancel()
	_, _ = h.Wait()

	// Renewer must stop after ctx cancel.
	time.Sleep(50 * time.Millisecond)
	stable := reg.heartbeatCount()
	time.Sleep(60 * time.Millisecond)
	if grew := reg.heartbeatCount(); grew != stable {
		t.Fatalf("renewer kept ticking after ctx cancel: %d -> %d", stable, grew)
	}
}

// cancelAwareCompleter blocks until either release is closed or ctx is canceled.
type cancelAwareCompleter struct {
	release <-chan struct{}
}

func (c cancelAwareCompleter) Complete(ctx context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	select {
	case <-c.release:
		return model.CompletionResp{Content: "ok"}, nil
	case <-ctx.Done():
		return model.CompletionResp{}, ctx.Err()
	}
}
