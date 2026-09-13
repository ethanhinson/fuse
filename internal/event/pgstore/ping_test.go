//go:build pgstore

package pgstore

// ping_test.go covers the event.Pinger cheap-liveness seam added for change 0076.
// Like the rest of the tagged suite it is skip-clean: no container runtime, no run.

import (
	"context"
	"testing"

	"github.com/ethanhinson/fuse/internal/event"
)

func TestPGStorePingHealthy(t *testing.T) {
	s := openHandle(t)
	var p event.Pinger = s
	if err := p.Ping(context.Background()); err != nil {
		t.Fatalf("Ping on a healthy store: got %v, want nil", err)
	}
}

func TestPGStorePingAfterClose(t *testing.T) {
	s := openHandle(t)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Ping(context.Background()); err == nil {
		t.Fatal("Ping on a closed store: got nil, want an error")
	}
}
