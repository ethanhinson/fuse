// Package workspacetest is the conformance suite for workspace.Provider
// implementations. RunProviderConformance exercises the full Provider/Workspace
// contract: idempotent open, escape rejection, tenant isolation, the scratch/
// directory, Release-then-ReOpen persistence, and concurrent writers.
//
// Every provider implementation (the local provider and MemProvider) must pass
// it; it is also the seam a future provider (object storage, kubernetes volume)
// is validated against.
package workspacetest

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/workspace"
)

// RunProviderConformance runs the full conformance suite against newProvider.
// Each call to newProvider must return an independent provider instance whose
// workspaces land in a fresh parent (for the local provider) or a fresh memory
// pool.
func RunProviderConformance(t *testing.T, newProvider func() workspace.Provider) {
	t.Helper()
	t.Run("open_idempotent", func(t *testing.T) { TestOpenIsIdempotent(t, newProvider) })
	t.Run("escape_rejection", func(t *testing.T) { TestEscapeRejection(t, newProvider) })
	t.Run("scratch_exists", func(t *testing.T) { TestScratchExists(t, newProvider) })
	t.Run("tenant_isolation", func(t *testing.T) { TestTenantIsolation(t, newProvider) })
	t.Run("release_then_reopen", func(t *testing.T) { TestReleaseThenReopen(t, newProvider) })
	t.Run("concurrent_writers", func(t *testing.T) { TestConcurrentWriters(t, newProvider) })
}

// TestOpenIsIdempotent proves a second open of the same (tenant, loop) returns
// a workspace with the same ID and the same files.
func TestOpenIsIdempotent(t *testing.T, newProvider func() workspace.Provider) {
	t.Helper()
	p := newProvider()
	cctx := context.Background()
	w1, err := p.Open(cctx, event.DefaultTenant, event.LoopID("conform"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	w2, err := p.Open(cctx, event.DefaultTenant, event.LoopID("conform"))
	if err != nil {
		t.Fatalf("Open 2: %v", err)
	}
	if w1.ID() != w2.ID() {
		t.Fatalf("expected same ID, got %q vs %q", w1.ID(), w2.ID())
	}
	if err := w1.FS().WriteFile("idempotent.txt", []byte("same"), 0o644); err != nil {
		t.Fatalf("WriteFile via w1: %v", err)
	}
	f, err := w2.FS().Open("idempotent.txt")
	if err != nil {
		t.Fatalf("Open via w2 after w1 wrote: %v", err)
	}
	buf := make([]byte, 4096); n, err := f.Read(buf); data := buf[:n]
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(data) != "same" {
		t.Fatalf("expected 'same', got %q", string(data))
	}
	_ = f.Close()
}

// TestEscapeRejection proves "..", absolute paths and nested traversal are
// ErrEscape for every FileSystem method that takes a path.
func TestEscapeRejection(t *testing.T, newProvider func() workspace.Provider) {
	t.Helper()
	p := newProvider()
	cctx := context.Background()
	w, err := p.Open(cctx, event.TenantID("esc-tn"), event.LoopID("esc-lo"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	fsys := w.FS()
	for _, bad := range []string{"../x", "/etc/passwd", "a/../../b", ".."} {
		// Read paths.
		if _, err := fsys.Open(bad); !errors.Is(err, workspace.ErrEscape) {
			t.Fatalf("Open(%q): expected ErrEscape, got %v", bad, err)
		}
		if _, err := fsys.Stat(bad); !errors.Is(err, workspace.ErrEscape) {
			t.Fatalf("Stat(%q): expected ErrEscape, got %v", bad, err)
		}
		if _, err := fsys.ReadDir(bad); !errors.Is(err, workspace.ErrEscape) {
			t.Fatalf("ReadDir(%q): expected ErrEscape, got %v", bad, err)
		}
		// Write paths.
		if err := fsys.WriteFile(bad, []byte("x"), 0o644); !errors.Is(err, workspace.ErrEscape) {
			t.Fatalf("WriteFile(%q): expected ErrEscape, got %v", bad, err)
		}
		if err := fsys.MkdirAll(bad, 0o700); !errors.Is(err, workspace.ErrEscape) {
			t.Fatalf("MkdirAll(%q): expected ErrEscape, got %v", bad, err)
		}
		if err := fsys.Rename(bad, "ok"); !errors.Is(err, workspace.ErrEscape) {
			t.Fatalf("Rename(%q, ok): expected ErrEscape, got %v", bad, err)
		}
		if err := fsys.Remove(bad); !errors.Is(err, workspace.ErrEscape) {
			t.Fatalf("Remove(%q): expected ErrEscape, got %v", bad, err)
		}
		if err := fsys.Walk(bad, func(string, os.DirEntry, error) error { return nil }); !errors.Is(err, workspace.ErrEscape) {
			t.Fatalf("Walk(%q): expected ErrEscape, got %v", bad, err)
		}
	}
}

// TestScratchExists proves every workspace has a scratch/ dir after open.
func TestScratchExists(t *testing.T, newProvider func() workspace.Provider) {
	t.Helper()
	p := newProvider()
	cctx := context.Background()
	w, err := p.Open(cctx, event.TenantID("scr-tn"), event.LoopID("scr-lo"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	found := false
	des, err := w.FS().ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, de := range des {
		if de.Name() == "scratch" && de.IsDir() {
			found = true
		}
	}
	if !found {
		t.Fatal("expected scratch/ in workspace root listing")
	}
}

// TestTenantIsolation proves tenant A cannot read tenant B's workspace files.
func TestTenantIsolation(t *testing.T, newProvider func() workspace.Provider) {
	t.Helper()
	p := newProvider()
	cctx := context.Background()
	wA, err := p.Open(cctx, event.TenantID("tn-a"), event.LoopID("iso"))
	if err != nil {
		t.Fatalf("Open A: %v", err)
	}
	wB, err := p.Open(cctx, event.TenantID("tn-b"), event.LoopID("iso"))
	if err != nil {
		t.Fatalf("Open B: %v", err)
	}
	if err := wA.FS().WriteFile("secret.txt", []byte("A-data"), 0o644); err != nil {
		t.Fatalf("WriteFile A: %v", err)
	}
	if _, err := wB.FS().Open("secret.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected ErrNotExist cross-tenant, got %v", err)
	}
}

// TestReleaseThenReopen proves Release followed by Open returns the same
// contents (Release never deletes anything in this change).
func TestReleaseThenReopen(t *testing.T, newProvider func() workspace.Provider) {
	t.Helper()
	p := newProvider()
	cctx := context.Background()
	w, err := p.Open(cctx, event.DefaultTenant, event.LoopID("relopen"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := w.FS().WriteFile("persist.txt", []byte("still-here"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := w.Release(cctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	w2, err := p.Open(cctx, event.DefaultTenant, event.LoopID("relopen"))
	if err != nil {
		t.Fatalf("Re-Open: %v", err)
	}
	f, err := w2.FS().Open("persist.txt")
	if err != nil {
		t.Fatalf("Re-Open read: %v", err)
	}
	buf := make([]byte, 4096); n, err := f.Read(buf); data := buf[:n]
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(data) != "still-here" {
		t.Fatalf("expected 'still-here', got %q", string(data))
	}
	_ = f.Close()
}

// TestConcurrentWriters proves two Provider instances over the same parent see
// each other's files (the local provider's concurrent-writer property).
func TestConcurrentWriters(t *testing.T, newProvider func() workspace.Provider) {
	t.Helper()
	p1 := newProvider()
	p2 := newProvider()
	cctx := context.Background()
	w1, err := p1.Open(cctx, event.DefaultTenant, event.LoopID("concurrent"))
	if err != nil {
		t.Fatalf("Open1: %v", err)
	}
	w2, err := p2.Open(cctx, event.DefaultTenant, event.LoopID("concurrent"))
	if err != nil {
		t.Fatalf("Open2: %v", err)
	}
	if err := w1.FS().WriteFile("from1.txt", []byte("one"), 0o644); err != nil {
		t.Fatalf("WriteFile1: %v", err)
	}
	if err := w2.FS().WriteFile("from2.txt", []byte("two"), 0o644); err != nil {
		t.Fatalf("WriteFile2: %v", err)
	}
	f, err := w1.FS().Open("from2.txt")
	if err != nil {
		t.Fatalf("Open from2 via w1: %v", err)
	}
	buf := make([]byte, 4096); n, err := f.Read(buf); data := buf[:n]
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(data) != "two" {
		t.Fatalf("expected 'two', got %q", string(data))
	}
	_ = f.Close()
}