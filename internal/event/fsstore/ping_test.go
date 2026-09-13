package fsstore

// ping_test.go covers the event.Pinger cheap-liveness seam added for change 0076.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethanhinson/fuse/internal/event"
)

func TestFSDurableStorePingHealthy(t *testing.T) {
	dir := t.TempDir()
	s := NewDurableFSStore(dir)
	t.Cleanup(func() { _ = s.Close() })

	var p event.Pinger = s
	if err := p.Ping(context.Background()); err != nil {
		t.Fatalf("Ping on a healthy store: got %v, want nil", err)
	}
}

func TestFSDurableStorePingMissingBaseDir(t *testing.T) {
	base := filepath.Join(t.TempDir(), "events")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	s := NewDurableFSStore(base)
	t.Cleanup(func() { _ = s.Close() })

	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("Ping before removal: got %v, want nil", err)
	}
	if err := os.RemoveAll(base); err != nil {
		t.Fatal(err)
	}
	if err := s.Ping(context.Background()); err == nil {
		t.Fatal("Ping after baseDir removal: got nil, want an error")
	}
}

func TestFSDurableStorePingBaseDirNotADirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewDurableFSStore(file)
	t.Cleanup(func() { _ = s.Close() })

	if err := s.Ping(context.Background()); err == nil {
		t.Fatal("Ping when baseDir is a regular file: got nil, want an error")
	}
}
