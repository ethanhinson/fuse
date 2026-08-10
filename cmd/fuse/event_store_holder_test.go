package main

import (
	"testing"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
)

// TestEventStoreHolderDefaultIsNoop: a zero-value per-loop holder (nothing set)
// returns a usable no-op store (never nil), so an emission before StartLoop sets the
// loop's store never panics. This replaces the retired process-global
// currentEventStore() default (change 0046).
func TestEventStoreHolderDefaultIsNoop(t *testing.T) {
	h := &eventStoreHolder{}
	got := h.get()
	if got == nil {
		t.Fatal("holder.get() returned nil; want no-op default")
	}
	if err := got.Append(event.Event{Kind: event.KindTurnStart}); err != nil {
		t.Errorf("default store Append error: %v", err)
	}
}

// TestEventStoreHolderSetGet: a real store published via set() is returned by get().
// This is the per-loop indirection that replaces the setActiveEventStore global — each
// Deps builder owns its OWN holder, so concurrent single-loop bindings never clobber.
func TestEventStoreHolderSetGet(t *testing.T) {
	base := t.TempDir()
	s, err := fsstore.NewFSEventStore(base, "sess-holder")
	if err != nil {
		t.Fatalf("NewFSEventStore: %v", err)
	}
	defer func() { _ = s.Close() }()

	h := &eventStoreHolder{}
	h.set(s)
	if h.get() != s {
		t.Error("holder.get() did not return the published store")
	}
}

// TestEventStoreHoldersAreIndependent proves two holders do NOT clobber each other —
// the property that makes N concurrent single-loop bindings isolated (change 0046).
// The retired package-global had exactly one slot, so a second binding overwrote the
// first; two instance-scoped holders cannot.
func TestEventStoreHoldersAreIndependent(t *testing.T) {
	base := t.TempDir()
	s1, err := fsstore.NewFSEventStore(base, "loop-1")
	if err != nil {
		t.Fatalf("NewFSEventStore 1: %v", err)
	}
	defer func() { _ = s1.Close() }()
	s2, err := fsstore.NewFSEventStore(base, "loop-2")
	if err != nil {
		t.Fatalf("NewFSEventStore 2: %v", err)
	}
	defer func() { _ = s2.Close() }()

	h1, h2 := &eventStoreHolder{}, &eventStoreHolder{}
	h1.set(s1)
	h2.set(s2)
	if h1.get() != s1 {
		t.Error("holder 1 was clobbered")
	}
	if h2.get() != s2 {
		t.Error("holder 2 was clobbered")
	}
}
