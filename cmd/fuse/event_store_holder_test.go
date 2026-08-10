package main

import (
	"testing"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
)

// TestEventStoreHolderDefaultIsNoop: with nothing installed, currentEventStore
// returns a usable no-op store (never nil), so emission never panics on
// one-shot / probe / mcp paths.
func TestEventStoreHolderDefaultIsNoop(t *testing.T) {
	setActiveEventStore(nil)
	got := currentEventStore()
	if got == nil {
		t.Fatal("currentEventStore returned nil; want no-op default")
	}
	if err := got.Append(event.Event{Kind: event.KindTurnStart}); err != nil {
		t.Errorf("default store Append error: %v", err)
	}
}

// TestEventStoreHolderSetGet: a real store installed via setActiveEventStore is
// returned by currentEventStore.
func TestEventStoreHolderSetGet(t *testing.T) {
	base := t.TempDir()
	s, err := fsstore.NewFSEventStore(base, "sess-holder")
	if err != nil {
		t.Fatalf("NewFSEventStore: %v", err)
	}
	defer func() { _ = s.Close() }()
	setActiveEventStore(s)
	defer setActiveEventStore(nil)
	if currentEventStore() != s {
		t.Error("currentEventStore did not return the installed store")
	}
}
