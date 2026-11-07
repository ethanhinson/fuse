//go:build !windows

package tools

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func noTempLeft(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if len(e.Name()) > 10 && e.Name()[:10] == ".fuse-tmp-" {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestWriteFileAtomic_KeepsModeOfExistingFile(t *testing.T) {
	for _, mode := range []os.FileMode{0o755, 0o700, 0o640, 0o600} {
		dir := t.TempDir()
		p := filepath.Join(dir, "f")
		if err := os.WriteFile(p, []byte("old"), mode); err != nil {
			t.Fatal(err)
		}
		if err := writeFileAtomic(p, []byte("new")); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Errorf("mode = %o, want %o", info.Mode().Perm(), mode)
		}
		noTempLeft(t, dir)
	}
}

func TestWriteFileAtomic_NewFileHonorsUmask(t *testing.T) {
	// Process-global, so this test is not parallel and restores it at once.
	old := syscall.Umask(0o027)
	defer syscall.Umask(old)
	p := filepath.Join(t.TempDir(), "f")
	if err := writeFileAtomic(p, []byte("x")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %o, want 0644 under umask 027 = 0640", info.Mode().Perm())
	}
}

func TestWriteFileAtomic_FollowsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "sub", "target")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink("sub/target", link); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(link, []byte("new")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the link was replaced by a regular file")
	}
	if b, _ := os.ReadFile(target); string(b) != "new" {
		t.Errorf("target = %q", b)
	}
	if ti, _ := os.Stat(target); ti.Mode().Perm() != 0o600 {
		t.Errorf("target mode = %o", ti.Mode().Perm())
	}
	noTempLeft(t, filepath.Dir(target))
}

func TestWriteFileAtomic_DanglingSymlinkCreatesTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "absent")
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(link, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "new" {
		t.Errorf("target = %q", b)
	}
	if info, _ := os.Lstat(link); info.Mode()&os.ModeSymlink == 0 {
		t.Error("the link was replaced")
	}
}

// A relative link through a symlinked directory: "..", from where the link really
// is, differs from what lexical cleaning of the path would conclude.
func TestWriteFileAtomic_RelativeLinkThroughSymlinkedDir(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../target", filepath.Join(real, "link")); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(filepath.Join(dir, "alias")+"/link", []byte("new")); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "a", "target")); err != nil || string(b) != "new" {
		t.Errorf("target = %q, %v", b, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "target")); err == nil {
		t.Error("written to the lexically cleaned path")
	}
}

func TestWriteFileAtomic_SymlinkLoopRefused(t *testing.T) {
	p := filepath.Join(t.TempDir(), "loop")
	if err := os.Symlink(p, p); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(p, []byte("x")); err == nil {
		t.Fatal("wrote through a symlink loop")
	}
}

// Rename needs only directory permission, so nothing else would refuse this.
func TestWriteFileAtomic_ReadOnlyFileStaysRefused(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("old"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(p, []byte("new")); !os.IsPermission(err) {
		t.Fatalf("err = %v, want permission denied", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "old" {
		t.Errorf("content = %q", b)
	}
	noTempLeft(t, dir)
}

// Renaming over a device or a FIFO would replace it with a regular file.
func TestWriteFileAtomic_NonRegularWrittenInPlace(t *testing.T) {
	if err := writeFileAtomic(os.DevNull, []byte("x")); err != nil {
		t.Fatalf("write to %s: %v", os.DevNull, err)
	}
	if info, err := os.Stat(os.DevNull); err != nil || info.Mode().IsRegular() {
		t.Fatalf("%s is now regular (err %v)", os.DevNull, err)
	}
	dir := t.TempDir()
	if err := writeFileAtomic(dir, []byte("x")); err == nil {
		t.Fatal("wrote over a directory")
	}
	noTempLeft(t, dir)
}

func TestWriteFileAtomic_FailedRenameKeepsOriginal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)
	if err := writeFileAtomic(p, []byte("new")); err == nil {
		t.Fatal("wrote into a read-only directory")
	}
	if b, _ := os.ReadFile(p); string(b) != "old" {
		t.Errorf("content = %q", b)
	}
}

func TestEditAndWriteToolsUseAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("hello"), 0o640); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	res := NewEditFile().Execute(t.Context(), `{"path":"`+p+`","old_string":"hello","new_string":"bye"}`)
	if res.IsError {
		t.Fatal(res.Output)
	}
	after, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Error("edit_file wrote in place: same inode")
	}
	res = NewWriteFile().Execute(t.Context(), `{"path":"`+p+`","content":"x"}`)
	if res.IsError {
		t.Fatal(res.Output)
	}
	final, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(after, final) {
		t.Error("write_file wrote in place: same inode")
	}
	if final.Mode().Perm() != 0o640 {
		t.Errorf("mode = %o", final.Mode().Perm())
	}
}
