package tools

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/workspace"
)

// openToolWorkspace opens a real local workspace with a file beside it, so an
// escape has something to reach.
func openToolWorkspace(t *testing.T) (fsys workspace.FileSystem, mount, secret string) {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	secret = filepath.Join(parent, "secret.txt")
	if err := os.WriteFile(secret, []byte("topsecret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := (&workspace.LocalProvider{Parent: parent}).Open(context.Background(), "acme", "loop1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Release(context.Background()) })
	mount, err = w.MountSource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return w.FS(), mount, secret
}

func runTool(t *testing.T, tl Tool, args map[string]any) Result {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return tl.Execute(context.Background(), string(b))
}

// TestWorkspaceTools_RoundTrip drives every file tool through a confined
// FileSystem; the change's own tests only exercised the raw os.* fallback.
func TestWorkspaceTools_RoundTrip(t *testing.T) {
	fsys, mount, _ := openToolWorkspace(t)
	for _, p := range []string{"a.md", "./b.md", "notes/deep/c.md", "/workspace/d.md"} {
		if r := runTool(t, NewWriteFile(fsys), map[string]any{"path": p, "content": "hello\n"}); r.IsError {
			t.Fatalf("write %q: %s", p, r.Output)
		}
	}
	for _, rel := range []string{"a.md", "b.md", "notes/deep/c.md", "d.md"} {
		if _, err := os.Stat(filepath.Join(mount, rel)); err != nil {
			t.Errorf("%s did not land in the workspace: %v", rel, err)
		}
	}
	if r := runTool(t, NewReadFile(fsys), map[string]any{"path": "/workspace/notes/deep/c.md"}); r.IsError || !strings.Contains(r.Output, "hello") {
		t.Errorf("read via /workspace prefix: %+v", r)
	}
	if r := runTool(t, NewEditFile(fsys), map[string]any{"path": "a.md", "old_string": "hello", "new_string": "bye"}); r.IsError {
		t.Fatalf("edit: %s", r.Output)
	}
	if r := runTool(t, NewReadFile(fsys), map[string]any{"path": "a.md"}); !strings.Contains(r.Output, "bye") {
		t.Errorf("edit did not persist: %q", r.Output)
	}
	if r := runTool(t, NewListDirectory(fsys), map[string]any{"path": "."}); r.IsError || !strings.Contains(r.Output, "notes/") || !strings.Contains(r.Output, "scratch/") {
		t.Errorf("list: %+v", r)
	}
	if r := runTool(t, NewListDirectory(fsys), map[string]any{"glob": "*.md"}); strings.Contains(r.Output, "notes/") || !strings.Contains(r.Output, "a.md") {
		t.Errorf("list glob: %+v", r)
	}
	if r := runTool(t, NewGrep(fsys), map[string]any{"pattern": "hello"}); r.IsError || !strings.Contains(r.Output, "notes/deep/c.md:1:") {
		t.Errorf("grep: %+v", r)
	}
	if r := runTool(t, NewGrep(fsys), map[string]any{"pattern": "hello", "glob": "b*"}); !strings.Contains(r.Output, "b.md") || strings.Contains(r.Output, "c.md") {
		t.Errorf("grep glob: %+v", r)
	}
}

// TestWorkspaceTools_EveryToolRefusesEscapes is acceptance criterion 2 ("test
// pins it for every file tool"), which the conformance suite covers only at the
// FileSystem layer.
func TestWorkspaceTools_EveryToolRefusesEscapes(t *testing.T) {
	fsys, mount, secret := openToolWorkspace(t)
	if err := os.Symlink(secret, filepath.Join(mount, "lnk")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if err := os.Symlink(filepath.Dir(secret), filepath.Join(mount, "dirlnk")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	for _, p := range []string{"/etc/passwd", secret, "../secret.txt", "../../acme/loop1/../../secret.txt", "a/../../secret.txt", "/workspace/../secret.txt", "lnk", "dirlnk/secret.txt"} {
		cases := map[string]Result{
			"read":  runTool(t, NewReadFile(fsys), map[string]any{"path": p}),
			"write": runTool(t, NewWriteFile(fsys), map[string]any{"path": p, "content": "owned"}),
			"edit":  runTool(t, NewEditFile(fsys), map[string]any{"path": p, "old_string": "top", "new_string": "own"}),
			"list":  runTool(t, NewListDirectory(fsys), map[string]any{"path": p}),
			"grep":  runTool(t, NewGrep(fsys), map[string]any{"pattern": "topsecret", "path": p}),
		}
		for name, r := range cases {
			if !r.IsError {
				t.Errorf("%s(%q) succeeded: %q", name, p, r.Output)
			}
			if strings.Contains(r.Output, "topsecret") {
				t.Errorf("%s(%q) leaked the secret: %q", name, p, r.Output)
			}
		}
	}
	if b, _ := os.ReadFile(secret); string(b) != "topsecret\n" {
		t.Errorf("the outside file was modified: %q", b)
	}
	// A recursive grep must not follow the planted links out of the workspace.
	if r := runTool(t, NewGrep(fsys), map[string]any{"pattern": "topsecret"}); strings.Contains(r.Output, "topsecret") {
		t.Errorf("recursive grep followed a link out: %q", r.Output)
	}
}

// TestWorkspaceTools_ReadFallsBackToGzip pins parity with archive.Open's
// transparent .gz recovery through the FileSystem code path.
func TestWorkspaceTools_ReadFallsBackToGzip(t *testing.T) {
	fsys, _, _ := openToolWorkspace(t)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte("archived\n"))
	_ = zw.Close()
	if err := fsys.WriteFile("old.log.gz", buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"old.log", "old.log.gz"} {
		if r := runTool(t, NewReadFile(fsys), map[string]any{"path": p}); r.IsError || !strings.Contains(r.Output, "archived") {
			t.Errorf("read %q: %+v", p, r)
		}
	}
}

// TestWorkspaceTools_ValidRootSpellingsAreNotEscapes: "./" and "/workspace/" name
// the workspace root. Reporting them as an escape tells the model it did
// something forbidden when it asked for the most ordinary path there is.
func TestWorkspaceTools_ValidRootSpellingsAreNotEscapes(t *testing.T) {
	fsys, _, _ := openToolWorkspace(t)
	if r := runTool(t, NewWriteFile(fsys), map[string]any{"path": "a.md", "content": "hello\n"}); r.IsError {
		t.Fatal(r.Output)
	}
	for _, p := range []string{"", ".", "./", "/workspace", "/workspace/", ".//", "././"} {
		if r := runTool(t, NewListDirectory(fsys), map[string]any{"path": p}); r.IsError || !strings.Contains(r.Output, "a.md") {
			t.Errorf("list %q: %+v", p, r)
		}
		if r := runTool(t, NewGrep(fsys), map[string]any{"pattern": "hello", "path": p}); r.IsError || !strings.Contains(r.Output, "a.md") {
			t.Errorf("grep %q: %+v", p, r)
		}
	}
}

// TestWorkspaceTools_AcceptPathsBashPrints: the spec strips "a leading
// /workspace/ or the mount prefix ... so the model can use what bash prints".
// On the host substrate `pwd` prints the mount source.
func TestWorkspaceTools_AcceptPathsBashPrints(t *testing.T) {
	fsys, mount, _ := openToolWorkspace(t)
	p := filepath.Join(mount, "from-pwd.md")
	if r := runTool(t, NewWriteFile(fsys), map[string]any{"path": p, "content": "x"}); r.IsError {
		t.Fatalf("write through the mount prefix: %s", r.Output)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("file did not land in the workspace: %v", err)
	}
}
