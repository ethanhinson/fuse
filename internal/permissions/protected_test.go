package permissions

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProtectedInRoot(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	scratch, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, ".git", "hooks"), filepath.Join(root, "innocent")); err != nil {
		t.Fatal(err)
	}
	roots := []string{root, scratch}
	cases := []struct {
		path string
		want bool
	}{
		{filepath.Join(root, "main.go"), false},
		{filepath.Join(root, "new", "dir", "file.go"), false},
		{filepath.Join(root, ".gitignore"), false},
		{filepath.Join(scratch, "note.txt"), false},
		{filepath.Join(root, ".git", "hooks", "pre-commit"), true},
		{filepath.Join(root, ".git", "config"), true},
		{filepath.Join(root, "vendor", "x", ".git", "config"), true},
		{filepath.Join(root, ".fuse", "sandbox.local.yml"), true},
		{filepath.Join(root, ".fuse.local.yml"), true},
		{filepath.Join(root, "innocent", "pre-commit"), true},
	}
	for _, c := range cases {
		if got := protectedInRoot(c.path, roots); got != c.want {
			t.Errorf("protectedInRoot(%s) = %v, want %v", c.path, got, c.want)
		}
	}
}

// A scratch root under ~/.fuse must stay writable: only segments below the root count.
func TestProtectedInRootIgnoresSegmentsAboveTheRoot(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(base, ".fuse", "tmp", "session")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	if protectedInRoot(filepath.Join(scratch, "note.txt"), []string{scratch}) {
		t.Error("a file in a scratch root under .fuse was treated as protected")
	}
}
