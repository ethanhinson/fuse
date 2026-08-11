package event_test

// store_hooks_test.go is the D5 observability-hooks gate for change 0047. D5 is
// HOOKS-ONLY: 0047 adds NO OTEL import, exporter, or /metrics surface — it only
// guarantees that the durable seam CARRIES what an out-of-band observability
// projection (0051) would need:
//   - a bare context.Context on every durable-store + registry op (by construction),
//     so a tracer/deadline can be threaded without a signature change;
//   - the (tenant_id, loop_id, node_id) labeling triple available in a
//     consumer-readable form (StreamKey.Tenant + StreamKey.Loop + Event.NodeID);
//   - and NO go.opentelemetry.io dependency reachable from internal/event.
//
// These are structural/by-construction assertions: if a durable op lost its
// context.Context or the labeling triple, this file would fail to COMPILE, and the
// OTEL check runs `go list -deps`.

import (
	"bytes"
	"context"
	"os/exec"
	"testing"

	"github.com/ethanhinson/fuse/internal/event"
)

// TestDurableOpsCarryContext asserts, by construction, that every DurableStore and
// LoopRegistry op takes a context.Context as its first parameter. This is a
// compile-time contract: the function values below only type-check if the signatures
// still lead with context.Context. If a signature dropped ctx, this file would not
// compile — which is exactly the gate D5 wants.
func TestDurableOpsCarryContext(t *testing.T) {
	var store event.DurableStore
	var reg event.LoopRegistry

	if store != nil {
		// These assignments pin the ctx-first shape of each DurableStore op.
		var _ func(context.Context, event.StreamKey, event.Event) error = store.Append
		var _ func(context.Context, event.StreamKey) (<-chan event.Event, func(), error) = store.Subscribe
		var _ func(context.Context, event.StreamKey, event.Seq) ([]event.Event, error) = store.Replay
	}
	if reg != nil {
		// These assignments pin the ctx-first shape of each LoopRegistry op.
		var _ func(context.Context, event.LoopRecord) error = reg.Register
		var _ func(context.Context, event.StreamKey, bool, string) error = reg.SetLive
		var _ func(context.Context, event.StreamKey) (event.LoopRecord, error) = reg.Resolve
		var _ func(context.Context, event.TenantID) ([]event.LoopRecord, error) = reg.List
	}
}

// TestLabelingTripleConsumerReadable asserts the (tenant_id, loop_id, node_id)
// labeling triple an observability projection needs is available in a
// consumer-readable form: tenant + loop off the StreamKey, node_id off the Event.
// This is again by-construction (field access), so a rename/removal breaks the
// build.
func TestLabelingTripleConsumerReadable(t *testing.T) {
	key := event.StreamKey{Tenant: "acme", Loop: "L1"}
	ev := event.Event{NodeID: "node-7"}

	if key.Tenant != "acme" {
		t.Fatalf("StreamKey.Tenant not consumer-readable: %q", key.Tenant)
	}
	if key.Loop != "L1" {
		t.Fatalf("StreamKey.Loop not consumer-readable: %q", key.Loop)
	}
	if ev.NodeID != "node-7" {
		t.Fatalf("Event.NodeID not consumer-readable: %q", ev.NodeID)
	}
	// The record ties the triple together: a LoopRecord carries the StreamKey and
	// the owning node, so a projection can label a stream without a side channel.
	rec := event.LoopRecord{Key: key, OwnerNodeID: "node-7"}
	if rec.Key.Tenant != "acme" || rec.Key.Loop != "L1" || rec.OwnerNodeID != "node-7" {
		t.Fatalf("LoopRecord does not carry the labeling triple: %+v", rec)
	}
}

// TestNoOTELDependency asserts internal/event imports no OTEL in 0047 (D5 is
// hooks-only — no exporter, no emission). The pgstore backend (which transitively
// pulls OTEL via testcontainers in its TESTS) is a separate, build-tagged package
// and is NOT part of internal/event's dependency closure here.
func TestNoOTELDependency(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "./...").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	if bytes.Contains(out, []byte("go.opentelemetry.io")) {
		t.Fatal("internal/event must not import OTEL in 0047 (D5 is hooks-only)")
	}
}
