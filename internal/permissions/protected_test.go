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

func TestReadScopeAllowed(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	root := filepath.Join(home, "repo")
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{root, filepath.Join(home, ".ssh"), filepath.Join(home, ".fuse")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "out")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	cases := []struct {
		tool, args string
		roots      []string
		want       bool
	}{
		{"read_file", `{"path":"main.go"}`, []string{root}, true},
		{"read_file", `{"path":"art://loop/x"}`, []string{root}, true},
		{"grep", `{"pattern":"x"}`, []string{root}, true},
		{"list_directory", `{}`, []string{root}, true},
		{"read_file", `{}`, []string{root}, true},
		{"read_file", `garbled`, []string{root}, false},
		{"read_file", `{"path":"../secret"}`, []string{root}, false},
		{"read_file", `{"path":"` + outside + `/s"}`, []string{root}, false},
		{"read_file", `{"path":"out/s"}`, []string{root}, false},
		{"grep", `{"pattern":"x","path":"/etc"}`, []string{root}, false},
		{"list_directory", `{"path":".."}`, []string{root}, false},
		// A home directory workspace must not expose credentials.
		{"read_file", `{"path":"` + home + `/.ssh/id_rsa"}`, []string{home}, false},
		{"read_file", `{"path":"` + home + `/.fuse/config.yml"}`, []string{home}, false},
		{"grep", `{"pattern":"x","path":"` + home + `"}`, []string{home}, false},
		// Tools with no filesystem path are untouched.
		{"spawn_agent", `{}`, []string{root}, true},
	}
	for _, c := range cases {
		if got := readScopeAllowed(c.tool, c.args, c.roots); got != c.want {
			t.Errorf("readScopeAllowed(%s, %s) = %v, want %v", c.tool, c.args, got, c.want)
		}
	}
}
