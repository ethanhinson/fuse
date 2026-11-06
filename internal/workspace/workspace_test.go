package workspace_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// TestNormalizeName pins the path contract directly: every spelling of the root
// is "." and never empty, and a ".." segment is refused however it is reached.
func TestNormalizeName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{".", "."},
		{"./", "."},
		{".//", "."},
		{"././", "."},
		{"a", "a"},
		{"a/", "a"},
		{"./a", "a"},
		{"a//b", "a/b"},
		{"a/./b", "a/b"},
		{"a/.", "a"},
		{"..a", "..a"},
		{"a..b/c", "a..b/c"},
	} {
		got, err := workspace.NormalizeName(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("NormalizeName(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"", "/", "/etc", "..", "../", "../a", "a/..", "a/../b", "a/../../b", "./.."} {
		if got, err := workspace.NormalizeName(bad); !errors.Is(err, workspace.ErrEscape) {
			t.Errorf("NormalizeName(%q) = %q, %v; want ErrEscape", bad, got, err)
		}
	}
}

// TestLocalProviderRefusesUnsafeIdentity: tenant and loop become path segments,
// so traversal, separators and hidden names must never reach the filesystem.
func TestLocalProviderRefusesUnsafeIdentity(t *testing.T) {
	parent := t.TempDir()
	p := &workspace.LocalProvider{Parent: parent}
	ctx := context.Background()
	for _, bad := range []string{"", ".", "..", "../x", "a/b", ".hidden", "a\x00b", strings.Repeat("a", 129)} {
		if _, err := p.Open(ctx, event.TenantID(bad), "loop"); err == nil {
			t.Errorf("tenant %q opened", bad)
		}
		if _, err := p.Open(ctx, "acme", event.LoopID(bad)); err == nil {
			t.Errorf("loop %q opened", bad)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(parent))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "x" {
			t.Errorf("a traversal identity created %q beside the parent", e.Name())
		}
	}
}

// TestLocalProviderTightensExistingModes: a tree provisioned wide out of band
// must not stay wide, or sibling uids can read a tenant's files.
func TestLocalProviderTightensExistingModes(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "acme", "loop")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	w, err := (&workspace.LocalProvider{Parent: parent}).Open(context.Background(), "acme", "loop")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Release(context.Background())
	for _, d := range []string{dir, filepath.Dir(dir), filepath.Join(dir, "scratch")} {
		fi, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o700 {
			t.Errorf("%s mode = %o, want 700", d, fi.Mode().Perm())
		}
	}
}

// TestLocalFSRefusesSymlinkEscapeForEveryMethod extends the Stat-only symlink
// check to each path-taking method, and to a symlink inside a parent directory.
func TestLocalFSRefusesSymlinkEscapeForEveryMethod(t *testing.T) {
	parent := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "s.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := (&workspace.LocalProvider{Parent: parent}).Open(context.Background(), "acme", "loop")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Release(context.Background())
	mount, _ := w.MountSource(context.Background())
	if err := os.Symlink(secret, filepath.Join(mount, "f")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(mount, "d")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	fsys := w.FS()
	for _, p := range []string{"f", "d/s.txt"} {
		if _, err := fsys.Open(p); !errors.Is(err, workspace.ErrEscape) {
			t.Errorf("Open(%q) = %v, want ErrEscape", p, err)
		}
		if _, err := fsys.Stat(p); !errors.Is(err, workspace.ErrEscape) {
			t.Errorf("Stat(%q) = %v, want ErrEscape", p, err)
		}
		if err := fsys.WriteFile(p, []byte("owned"), 0o644); !errors.Is(err, workspace.ErrEscape) {
			t.Errorf("WriteFile(%q) = %v, want ErrEscape", p, err)
		}
	}
	if err := fsys.MkdirAll("d/new", 0o755); !errors.Is(err, workspace.ErrEscape) {
		t.Errorf("MkdirAll through a symlink = %v, want ErrEscape", err)
	}
	if err := fsys.Rename("d/s.txt", "stolen"); !errors.Is(err, workspace.ErrEscape) {
		t.Errorf("Rename out through a symlink = %v, want ErrEscape", err)
	}
	if err := fsys.Remove("d/s.txt"); !errors.Is(err, workspace.ErrEscape) {
		t.Errorf("Remove through a symlink = %v, want ErrEscape", err)
	}
	if _, err := fsys.ReadDir("d"); !errors.Is(err, workspace.ErrEscape) {
		t.Errorf("ReadDir(symlink) = %v, want ErrEscape", err)
	}
	if b, _ := os.ReadFile(secret); string(b) != "secret" {
		t.Errorf("outside file changed: %q", b)
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); err == nil {
		t.Error("MkdirAll created a directory outside the workspace")
	}
}
