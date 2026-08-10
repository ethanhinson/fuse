package fsstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/eventtest"
)

// TestFSLoopRegistryRoundTrip: Register→Resolve returns the record; SetLive flips
// liveness/owner; an unregistered loop resolves ErrLoopUnknown; List is
// tenant-scoped; the sidecar lives at <base>/<tenant>/<loop>/loop.json.
func TestFSLoopRegistryRoundTrip(t *testing.T) {
	base := t.TempDir()
	s := NewDurableFSStore(base)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	k := event.StreamKey{Tenant: "acme", Loop: "L1"}
	if _, err := s.Resolve(ctx, k); err != event.ErrLoopUnknown {
		t.Fatalf("Resolve before Register = %v, want ErrLoopUnknown", err)
	}
	if err := s.Register(ctx, event.LoopRecord{Key: k, OwnerNodeID: "nodeA", Live: true}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Sidecar on disk.
	if _, err := os.Stat(filepath.Join(base, "acme", "L1", "loop.json")); err != nil {
		t.Fatalf("loop.json sidecar not at expected path: %v", err)
	}
	rec, err := s.Resolve(ctx, k)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if rec.Key != k || rec.OwnerNodeID != "nodeA" || !rec.Live {
		t.Fatalf("resolved record mismatch: %+v", rec)
	}
	if rec.CreatedAt.IsZero() || rec.UpdatedAt.IsZero() {
		t.Fatalf("CreatedAt/UpdatedAt must be set: %+v", rec)
	}

	if err := s.SetLive(ctx, k, false, "nodeB"); err != nil {
		t.Fatalf("SetLive: %v", err)
	}
	rec2, err := s.Resolve(ctx, k)
	if err != nil {
		t.Fatalf("Resolve after SetLive: %v", err)
	}
	if rec2.Live || rec2.OwnerNodeID != "nodeB" {
		t.Fatalf("SetLive did not flip liveness/owner: %+v", rec2)
	}
	if rec2.CreatedAt != rec.CreatedAt {
		t.Fatalf("SetLive must preserve CreatedAt: was %v now %v", rec.CreatedAt, rec2.CreatedAt)
	}

	// Register a loop under a different tenant; List is tenant-scoped.
	if err := s.Register(ctx, event.LoopRecord{Key: event.StreamKey{Tenant: "beta", Loop: "L9"}, OwnerNodeID: "nodeC", Live: true}); err != nil {
		t.Fatalf("Register beta: %v", err)
	}
	ls, err := s.List(ctx, "acme")
	if err != nil {
		t.Fatalf("List(acme): %v", err)
	}
	if len(ls) != 1 || ls[0].Key != k {
		t.Fatalf("List(acme) = %+v, want only acme/L1", ls)
	}
}

// TestFSStoreConformance runs the ONE shared behavioral suite against fsstore's
// real constructor (parity-test-feeds-each-side-its-own-production-source). The
// same *FSDurableStore is both the DurableStore and the LoopRegistry.
func TestFSStoreConformance(t *testing.T) {
	eventtest.RunDurableStoreConformance(t, func(t *testing.T) (event.DurableStore, event.LoopRegistry) {
		s := NewDurableFSStore(t.TempDir())
		t.Cleanup(func() { _ = s.Close() })
		return s, s
	})
}
