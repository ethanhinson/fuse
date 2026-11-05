package workspace_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/workspace"
	"github.com/ethanhinson/fuse/internal/workspace/workspacetest"
)

// TestLocalProviderConformance runs the full conformance suite against the
// local (os.Root-backed) provider.
func TestLocalProviderConformance(t *testing.T) {
	parent := t.TempDir()
	workspacetest.RunProviderConformance(t, func() workspace.Provider {
		return &workspace.LocalProvider{Parent: parent}
	})
}

// TestMemProviderConformance runs the full conformance suite against the
// in-memory provider.
func TestMemProviderConformance(t *testing.T) {
	// MemProvider is in-memory: two calls to newProvider() must share the
	// same backing pool for the concurrent-writers check (two providers over
	// the same parent). A single MemProvider instance serves both opens.
	p := workspace.NewMemProvider()
	workspacetest.RunProviderConformance(t, func() workspace.Provider { return p })
}

// TestLocalEscapeRejectsAbsoluteAndDotDot verifies the local provider's
// FileSystem rejects "..", absolute paths, and symlink escapes with ErrEscape
// for every path-taking method.
func TestLocalEscapeRejectsAbsoluteAndDotDot(t *testing.T) {
	parent := t.TempDir()
	p := &workspace.LocalProvider{Parent: parent}
	w, err := p.Open(context.Background(), event.DefaultTenant, event.LoopID("escape"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	fsys := w.FS()
	for _, bad := range []string{"../x", "/etc/passwd", "a/../../b", ".."} {
		if _, err := fsys.Stat(bad); !errors.Is(err, workspace.ErrEscape) {
			t.Errorf("Stat(%q): want ErrEscape, got %v", bad, err)
		}
		if err := fsys.MkdirAll(bad, 0o755); !errors.Is(err, workspace.ErrEscape) {
			t.Errorf("MkdirAll(%q): want ErrEscape, got %v", bad, err)
		}
		if err := fsys.WriteFile(bad, []byte("x"), 0o644); !errors.Is(err, workspace.ErrEscape) {
			t.Errorf("WriteFile(%q): want ErrEscape, got %v", bad, err)
		}
		if _, err := fsys.Open(bad); !errors.Is(err, workspace.ErrEscape) {
			t.Errorf("Open(%q): want ErrEscape, got %v", bad, err)
		}
		if _, err := fsys.ReadDir(bad); !errors.Is(err, workspace.ErrEscape) {
			t.Errorf("ReadDir(%q): want ErrEscape, got %v", bad, err)
		}
	}
}

// TestLocalSymlinkEscape verifies a symlink pointing outside the workspace is
// rejected with ErrEscape.
func TestLocalSymlinkEscape(t *testing.T) {
	parent := t.TempDir()
	// Create a secret file outside the workspace.
	outside := filepath.Join(parent, "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &workspace.LocalProvider{Parent: parent}
	w, err := p.Open(context.Background(), event.TenantID("sym-tn"), event.LoopID("sym-lo"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	fsys := w.FS()
	// Write a symlink pointing outside the workspace dir.
	linkTarget := filepath.Join(parent, "secret.txt")
	wsDir := filepath.Join(parent, "sym-tn", "sym-lo")
	if err := os.Symlink(linkTarget, filepath.Join(wsDir, "escape")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	if _, err := fsys.Stat("escape"); !errors.Is(err, workspace.ErrEscape) {
		t.Errorf("Stat(symlink escape): want ErrEscape, got %v", err)
	}
}

// Ensure errors.Is is available.
var _ = errors.Is