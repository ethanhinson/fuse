package event

import "testing"

// compile-time assertion that NoopStore implements EventStore.
var _ EventStore = NoopStore{}

// TestNormalizeTenantDefaults asserts the durable seam's tenant normalization:
// the empty tenant maps to DefaultTenant (single-tenant local behavior), any other
// tenant passes through unchanged.
func TestNormalizeTenantDefaults(t *testing.T) {
	if got := NormalizeTenant(""); got != DefaultTenant {
		t.Fatalf("NormalizeTenant(\"\") = %q, want %q", got, DefaultTenant)
	}
	if got := NormalizeTenant("acme"); got != TenantID("acme") {
		t.Fatalf("NormalizeTenant(acme) = %q, want acme", got)
	}
}

// TestNoopStoreInert asserts the no-op default store is safe and does nothing —
// it is the nil-holder default so no emission point ever nil-panics.
func TestNoopStoreInert(t *testing.T) {
	s := NoopStore{}
	if err := s.Append(Event{Kind: KindTurnStart}); err != nil {
		t.Errorf("Noop Append returned error: %v", err)
	}
	ch, cancel := s.Subscribe()
	if ch == nil {
		t.Fatal("Noop Subscribe returned nil channel")
	}
	// The channel must be non-blocking to read from (closed) so a consumer range
	// terminates immediately rather than hanging.
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("Noop Subscribe channel yielded a value")
		}
	default:
		t.Error("Noop Subscribe channel is open and empty (would block a range)")
	}
	cancel() // must be safe and idempotent
	cancel()
	evs, err := s.Replay(0)
	if err != nil {
		t.Errorf("Noop Replay error: %v", err)
	}
	if len(evs) != 0 {
		t.Errorf("Noop Replay returned %d events, want 0", len(evs))
	}
}
