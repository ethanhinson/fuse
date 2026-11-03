package fsstore_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/ethanhinson/fuse/internal/artifact"
	"github.com/ethanhinson/fuse/internal/artifact/artifacttest"
	"github.com/ethanhinson/fuse/internal/artifact/fsstore"
	"github.com/ethanhinson/fuse/internal/event"
)

func ref(tenant, loop string, kind artifact.Kind, id string) artifact.Ref {
	return artifact.Ref{Tenant: event.TenantID(tenant), Loop: event.LoopID(loop), Kind: kind, ID: id}
}

func mustPut(t *testing.T, s artifact.Store, r artifact.Ref, body string) artifact.Meta {
	t.Helper()
	m, err := s.Put(context.Background(), r, strings.NewReader(body), artifact.Meta{Tool: "bash"})
	if err != nil {
		t.Fatalf("Put(%s): %v", r, err)
	}
	return m
}

func mustRead(t *testing.T, s artifact.Store, r artifact.Ref) string {
	t.Helper()
	rc, _, err := s.Open(context.Background(), r)
	if err != nil {
		t.Fatalf("Open(%s): %v", r, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", r, err)
	}
	return string(b)
}

// files lists every regular file under dir, relative to it.
func files(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, rel)
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

func TestFSStoreConformance(t *testing.T) {
	artifacttest.RunStoreConformance(t, func(t *testing.T) artifact.Store { return fsstore.New(t.TempDir()) })
}

func TestFSStoreLayoutAndGzipAtRest(t *testing.T) {
	base := t.TempDir()
	s := fsstore.New(base)
	r := ref("acme", "0190abc", artifact.KindSegment, "seg1")
	mustPut(t, s, r, strings.Repeat("segment body ", 100))

	body := filepath.Join(base, "acme", "0190abc", "segment", "seg1")
	raw, err := os.ReadFile(body)
	if err != nil {
		t.Fatalf("body not at %s: %v", body, err)
	}
	if len(raw) < 2 || raw[0] != 0x1f || raw[1] != 0x8b {
		t.Fatalf("body is not gzip at rest: % x", raw[:min(len(raw), 4)])
	}
	metaPath := filepath.Join(base, "acme", "0190abc", "segment", ".meta", "seg1.json")
	mb, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("meta not at %s: %v", metaPath, err)
	}
	var m artifact.Meta
	if err := json.Unmarshal(mb, &m); err != nil || m.Ref != r || m.Tool != "bash" {
		t.Fatalf("meta = %+v, %v", m, err)
	}
	// Host-filesystem artifacts are private to the process owner.
	for _, p := range []string{body, metaPath} {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v, %v; want 0600", p, fi.Mode().Perm(), err)
		}
	}
	for _, d := range []string{"acme", "acme/0190abc", "acme/0190abc/segment", "acme/0190abc/segment/.meta"} {
		if fi, err := os.Stat(filepath.Join(base, d)); err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %v, %v; want 0700", d, fi.Mode().Perm(), err)
		}
	}
}

func TestFSStoreRefusesUnsafeSegments(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	s := fsstore.New(base)

	unsafe := map[string]artifact.Ref{
		"uppercase tenant":    ref("Acme", "l1", artifact.KindSpill, "x1"),
		"uppercase loop":      ref("acme", "L1", artifact.KindSpill, "x1"),
		"uppercase id":        ref("acme", "l1", artifact.KindSpill, "X1"),
		"leading-dot id":      ref("acme", "l1", artifact.KindSpill, ".meta"),
		"dotdot id":           ref("acme", "l1", artifact.KindSpill, ".."),
		"dotdot loop":         ref("acme", "..", artifact.KindSpill, "x1"),
		"trailing-dot tenant": ref("acme.", "l1", artifact.KindSpill, "x1"),
	}
	for name, r := range unsafe {
		if _, err := s.Put(ctx, r, strings.NewReader("x"), artifact.Meta{}); !errors.Is(err, artifact.ErrInvalidRef) {
			t.Errorf("%s: Put err = %v, want ErrInvalidRef", name, err)
		}
		if _, _, err := s.Open(ctx, r); !errors.Is(err, artifact.ErrInvalidRef) {
			t.Errorf("%s: Open err = %v, want ErrInvalidRef", name, err)
		}
		scopeUnsafe := strings.Contains(name, "tenant") || strings.Contains(name, "loop")
		if _, err := s.List(ctx, r.Tenant, r.Loop, r.Kind); scopeUnsafe && !errors.Is(err, artifact.ErrInvalidRef) {
			t.Errorf("%s: List err = %v, want ErrInvalidRef", name, err)
		}
	}
	// Validation happens before any I/O: refused calls created nothing.
	if entries, err := os.ReadDir(base); err != nil || len(entries) != 0 {
		t.Fatalf("refused calls touched the base dir: %v, %v", entries, err)
	}

	// Refuse, never lowercase: acme's sentinel must not be reachable as Acme.
	mustPut(t, s, ref("acme", "l1", artifact.KindSpill, "x1"), "acme sentinel")
	if mustRead(t, s, ref("acme", "l1", artifact.KindSpill, "x1")) != "acme sentinel" {
		t.Fatal("owner cannot read its sentinel")
	}
	rc, _, err := s.Open(ctx, ref("Acme", "l1", artifact.KindSpill, "x1"))
	if err == nil {
		b, _ := io.ReadAll(rc)
		rc.Close()
		t.Fatalf("Acme read acme's artifact: %q", b)
	}
	if !errors.Is(err, artifact.ErrInvalidRef) {
		t.Fatalf("Acme Open err = %v, want ErrInvalidRef", err)
	}
	if entries, _ := os.ReadDir(base); len(entries) != 1 || entries[0].Name() != "acme" {
		t.Fatalf("base entries = %v, want only acme", entries)
	}
}

func TestFSStoreTenantSiblingsDisjoint(t *testing.T) {
	base := t.TempDir()
	s := fsstore.New(base)
	ra := ref("a", "l1", artifact.KindSpill, "shared-id")
	rb := ref("b", "l1", artifact.KindSpill, "shared-id")

	mustPut(t, s, ra, "tenant a sentinel")
	// Positive isolation: b cannot read a's sentinel under the same loop/kind/id.
	if _, _, err := s.Open(context.Background(), rb); !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("tenant b Open err = %v, want ErrNotFound", err)
	}
	if list, err := s.List(context.Background(), "b", "l1", artifact.KindSpill); err != nil || len(list) != 0 {
		t.Fatalf("tenant b List = %v, %v; want empty", list, err)
	}
	mustPut(t, s, rb, "tenant b body")
	if got := mustRead(t, s, ra); got != "tenant a sentinel" {
		t.Fatalf("tenant b's write clobbered a: %q", got)
	}
	if got := mustRead(t, s, rb); got != "tenant b body" {
		t.Fatalf("tenant b read %q", got)
	}

	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			t.Fatalf("non-directory %q directly under base", e.Name())
		}
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "a,b" {
		t.Fatalf("base children = %v, want [a b]", names)
	}
	for _, tn := range []string{"a", "b"} {
		got := files(t, filepath.Join(base, tn))
		want := []string{filepath.Join("l1", "spill", ".meta", "shared-id.json"), filepath.Join("l1", "spill", "shared-id")}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("tenant %s files = %v, want %v", tn, got, want)
		}
	}
}

func TestFSStoreEmptyTenantIsDefault(t *testing.T) {
	base := t.TempDir()
	s := fsstore.New(base)
	mustPut(t, s, ref("", "l1", artifact.KindSpill, "x1"), "default tenant")
	if got := mustRead(t, s, ref(string(event.DefaultTenant), "l1", artifact.KindSpill, "x1")); got != "default tenant" {
		t.Fatalf("read %q", got)
	}
	if _, err := os.Stat(filepath.Join(base, string(event.DefaultTenant), "l1", "spill", "x1")); err != nil {
		t.Fatalf("empty tenant not stored under %s: %v", event.DefaultTenant, err)
	}
}

func TestFSStoreAtomicWrite(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	s := fsstore.New(base)

	mustPut(t, s, ref("t1", "l1", artifact.KindSpill, "ok"), "complete")
	for _, f := range files(t, base) {
		if strings.Contains(f, ".tmp") {
			t.Fatalf("temp file left after successful Put: %s", f)
		}
	}

	boom := errors.New("reader broke")
	failing := func() io.Reader {
		return io.MultiReader(strings.NewReader(strings.Repeat("partial ", 1000)), iotest.ErrReader(boom))
	}
	fresh := ref("t1", "l1", artifact.KindSpill, "broken")
	if _, err := s.Put(ctx, fresh, failing(), artifact.Meta{}); !errors.Is(err, boom) {
		t.Fatalf("Put err = %v, want the reader's error", err)
	}
	if _, _, err := s.Open(ctx, fresh); !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("Open after failed Put err = %v, want ErrNotFound", err)
	}
	// A failed overwrite keeps the previous artifact intact.
	if _, err := s.Put(ctx, ref("t1", "l1", artifact.KindSpill, "ok"), failing(), artifact.Meta{}); !errors.Is(err, boom) {
		t.Fatalf("overwrite Put err = %v, want the reader's error", err)
	}
	if got := mustRead(t, s, ref("t1", "l1", artifact.KindSpill, "ok")); got != "complete" {
		t.Fatalf("failed overwrite changed the artifact: %q", got)
	}
	// A body over the cap is refused mid-stream and leaves nothing behind either.
	big := io.LimitReader(bytes.NewReader(make([]byte, artifact.MaxArtifactBytes+1)), artifact.MaxArtifactBytes+1)
	if _, err := s.Put(ctx, ref("t1", "l1", artifact.KindSpill, "big"), big, artifact.Meta{}); !errors.Is(err, artifact.ErrTooLarge) {
		t.Fatalf("oversized Put err = %v, want ErrTooLarge", err)
	}

	got := files(t, base)
	want := []string{
		filepath.Join("t1", "l1", "spill", ".meta", "ok.json"),
		filepath.Join("t1", "l1", "spill", "ok"),
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("files after failed Puts = %v, want %v", got, want)
	}
	if list, err := s.List(ctx, "t1", "l1", artifact.KindSpill); err != nil || len(list) != 1 || list[0].Ref.ID != "ok" {
		t.Fatalf("List = %v, %v; want only ok", list, err)
	}
}

func TestFSStoreCrossInstance(t *testing.T) {
	dir := t.TempDir()
	a, b := fsstore.New(dir), fsstore.New(dir)
	r := ref("t1", "loop9", artifact.KindSegment, "")
	m := mustPut(t, a, r, "written by instance a")
	if got := mustRead(t, b, m.Ref); got != "written by instance a" {
		t.Fatalf("instance b read %q", got)
	}
	list, err := b.List(context.Background(), "t1", "loop9", artifact.KindSegment)
	if err != nil || len(list) != 1 || list[0].Ref != m.Ref || list[0].Tool != "bash" {
		t.Fatalf("instance b List = %+v, %v", list, err)
	}
}

func TestFSStoreRefusesMetaNamingAnotherRef(t *testing.T) {
	// A meta file is trusted only when it names exactly the ref whose path it
	// was found under; a tree copied or moved into another tenant is not served.
	base := t.TempDir()
	s := fsstore.New(base)
	mustPut(t, s, ref("a", "l1", artifact.KindSpill, "x1"), "tenant a sentinel")
	src := filepath.Join(base, "a")
	dst := filepath.Join(base, "b")
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Open(context.Background(), ref("b", "l1", artifact.KindSpill, "x1")); !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("Open of copied tree err = %v, want ErrNotFound", err)
	}
	if list, err := s.List(context.Background(), "b", "l1", artifact.KindSpill); err != nil || len(list) != 0 {
		t.Fatalf("List of copied tree = %+v, %v; want empty", list, err)
	}
}

func TestFSStoreListMissingIsEmpty(t *testing.T) {
	s := fsstore.New(filepath.Join(t.TempDir(), "never-created"))
	list, err := s.List(context.Background(), "t1", "l1", artifact.KindSpill)
	if err != nil || len(list) != 0 {
		t.Fatalf("List = %v, %v; want empty, nil", list, err)
	}
}

// rootedPath matches an absolute host path anywhere in an error string ("art://"
// refs are not rooted: their '/' follows a ':').
var rootedPath = regexp.MustCompile(`(^|[\s"'(=])/[^/\s]`)

// TestFSStoreErrorsNameNoHostPath: an I/O failure other than "not exist"
// (here EACCES on the kind directory) must not carry the base directory, or
// any absolute path, into the error: tools write store errors into durable
// tool results.
func TestFSStoreErrorsNameNoHostPath(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	ctx := context.Background()
	base := t.TempDir()
	s := fsstore.New(base)
	r := ref("acme", "l1", artifact.KindSpill, "x1")
	mustPut(t, s, r, "body")
	kindDir := filepath.Join(base, "acme", "l1", "spill")
	if err := os.Chmod(kindDir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(kindDir, 0o700) })

	check := func(op string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: err = nil, want a permission failure", op)
		}
		if errors.Is(err, artifact.ErrNotFound) {
			t.Fatalf("%s: err = %v, want a permission failure, not ErrNotFound", op, err)
		}
		if !errors.Is(err, fs.ErrPermission) {
			t.Errorf("%s: err = %v, want errors.Is fs.ErrPermission preserved", op, err)
		}
		msg := err.Error()
		if strings.Contains(msg, base) || rootedPath.MatchString(msg) {
			t.Errorf("%s: error names a host path: %q", op, msg)
		}
		var pe *fs.PathError
		var le *os.LinkError
		if errors.As(err, &pe) || errors.As(err, &le) {
			t.Errorf("%s: error chain still carries a path-bearing error: %#v", op, err)
		}
	}

	_, _, err := s.Open(ctx, r)
	check("Open", err)
	_, err = s.List(ctx, "acme", "l1", artifact.KindSpill)
	check("List", err)
	_, err = s.Put(ctx, ref("acme", "l1", artifact.KindSpill, "x2"), strings.NewReader("b"), artifact.Meta{})
	check("Put", err)
}
