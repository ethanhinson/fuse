package tools

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/artifact"
)

// artCtx returns a context bound to (acme, l1) on a fresh MemStore, with a
// 3000-line spill artifact seeded. Line n reads "line <n>"; line 2222 also
// carries NEEDLE.
func artCtx(t *testing.T) (context.Context, artifact.Binding, artifact.Ref) {
	t.Helper()
	b := artifact.Binding{Store: artifact.NewMemStore(), Tenant: "acme", Loop: "l1"}
	var sb strings.Builder
	for i := 1; i <= 3000; i++ {
		if i > 1 {
			sb.WriteByte('\n')
		}
		fmt.Fprintf(&sb, "line %d", i)
		if i == 2222 {
			sb.WriteString(" NEEDLE")
		}
	}
	m, err := b.Put(t.Context(), artifact.KindSpill, strings.NewReader(sb.String()), artifact.Meta{Tool: "bash"})
	if err != nil {
		t.Fatal(err)
	}
	return artifact.WithBinding(t.Context(), b), b, m.Ref
}

func toolArgs(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestReadFileArtifactRefRange(t *testing.T) {
	ctx, _, ref := artCtx(t)
	res := NewReadFile().Execute(ctx, toolArgs(t, readArgs{Path: ref.String(), StartLine: 1500, EndLine: 1502}))
	if res.IsError {
		t.Fatal(res.Output)
	}
	if res.Output != "line 1500\nline 1501\nline 1502" {
		t.Errorf("output = %q", res.Output)
	}
}

func TestReadFileArtifactRefWindowed(t *testing.T) {
	ctx, _, ref := artCtx(t)
	res := NewReadFile().Execute(ctx, toolArgs(t, readArgs{Path: ref.String()}))
	if res.IsError {
		t.Fatal(res.Output)
	}
	if !strings.Contains(res.Output, "(Showing lines 1-1000 of 3000. Use start_line=1001 to continue.)") {
		t.Errorf("missing window footer: %q", res.Output[len(res.Output)-120:])
	}
	if strings.Contains(res.Output, "line 1001\n") {
		t.Error("unranged read must be windowed")
	}
}

// TestReadFileArtifactRefNoOracle: another tenant's ref and a ref that does not
// exist fail with the same message, so a caller cannot probe for artifacts.
func TestReadFileArtifactRefNoOracle(t *testing.T) {
	ctx, _, ref := artCtx(t)
	// The sentinel really exists under tenant "other"; it must still be unreachable.
	other := artifact.Binding{Store: mustBindingStore(t, ctx), Tenant: "other", Loop: "l1"}
	om, err := other.Put(t.Context(), artifact.KindSpill, strings.NewReader("other-tenant-secret"), artifact.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	missing := artifact.Ref{Tenant: "acme", Loop: "l1", Kind: artifact.KindSpill, ID: "0000000000000000-deadbeefdeadbeef"}
	otherLoop := ref
	otherLoop.Loop = "l2"

	var msgs []string
	for _, p := range []string{om.Ref.String(), missing.String(), otherLoop.String(), "art://acme/l1/spill/../x"} {
		res := NewReadFile().Execute(ctx, toolArgs(t, readArgs{Path: p}))
		if !res.IsError {
			t.Fatalf("%s: want error, got %q", p, res.Output)
		}
		if !strings.Contains(res.Output, "artifact not found") {
			t.Errorf("%s: output = %q", p, res.Output)
		}
		if strings.Contains(res.Output, "other-tenant-secret") {
			t.Fatal("cross-tenant read leaked the sentinel")
		}
		msgs = append(msgs, strings.ReplaceAll(res.Output, p, "<ref>"))
	}
	for _, m := range msgs[1:] {
		if m != msgs[0] {
			t.Errorf("error messages differ (oracle): %q vs %q", msgs[0], m)
		}
	}
}

func mustBindingStore(t *testing.T, ctx context.Context) artifact.Store {
	t.Helper()
	b, ok := artifact.BindingFrom(ctx)
	if !ok {
		t.Fatal("no binding")
	}
	return b.Store
}

func TestArtifactRefWithoutBinding(t *testing.T) {
	_, _, ref := artCtx(t)
	ctx := context.Background()
	for name, res := range map[string]Result{
		"read_file":      NewReadFile().Execute(ctx, toolArgs(t, readArgs{Path: ref.String()})),
		"grep":           NewGrep().Execute(ctx, toolArgs(t, grepArgs{Pattern: "x", Path: ref.String()})),
		"list_directory": NewListDirectory().Execute(ctx, toolArgs(t, listArgs{Path: "art://acme/l1"})),
	} {
		if !res.IsError || !strings.Contains(res.Output, "artifact refs are not available in this session") {
			t.Errorf("%s: want unavailable error, got %v %q", name, res.IsError, res.Output)
		}
	}
}

func TestReadFileArtifactGzipBodyAndBinary(t *testing.T) {
	ctx, b, _ := artCtx(t)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte("gz-line-1\ngz-line-2"))
	zw.Close()
	gm, err := b.Put(t.Context(), artifact.KindSpill, &buf, artifact.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	res := NewReadFile().Execute(ctx, toolArgs(t, readArgs{Path: gm.Ref.String()}))
	if res.IsError || res.Output != "gz-line-1\ngz-line-2" {
		t.Errorf("gzip body: %v %q", res.IsError, res.Output)
	}

	bm, err := b.Put(t.Context(), artifact.KindSpill, strings.NewReader("ELF\x00\x01\x02"), artifact.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	res = NewReadFile().Execute(ctx, toolArgs(t, readArgs{Path: bm.Ref.String()}))
	if !res.IsError || !strings.Contains(res.Output, "binary file") {
		t.Errorf("binary body: %v %q", res.IsError, res.Output)
	}
}

func TestGrepArtifactRef(t *testing.T) {
	ctx, _, ref := artCtx(t)
	res := NewGrep().Execute(ctx, toolArgs(t, grepArgs{Pattern: "NEEDLE", Path: ref.String()}))
	if res.IsError {
		t.Fatal(res.Output)
	}
	if res.Output != ref.String()+":2222: line 2222 NEEDLE" {
		t.Errorf("output = %q", res.Output)
	}

	res = NewGrep().Execute(ctx, toolArgs(t, grepArgs{Pattern: "^line", Path: ref.String(), MaxResults: 3}))
	if res.IsError {
		t.Fatal(res.Output)
	}
	if got := strings.Count(res.Output, ref.String()+":"); got != 3 {
		t.Errorf("result cap: got %d matches:\n%s", got, res.Output)
	}
	if !strings.Contains(res.Output, "showing first 3 matches") {
		t.Errorf("missing cap footer: %q", res.Output)
	}

	res = NewGrep().Execute(ctx, toolArgs(t, grepArgs{Pattern: "x", Path: "art://acme/l1/spill"}))
	if !res.IsError || !strings.Contains(res.Output, "full artifact ref") {
		t.Errorf("prefix ref: %v %q", res.IsError, res.Output)
	}

	res = NewGrep().Execute(ctx, toolArgs(t, grepArgs{Pattern: "x", Path: "art://other/l1/spill/" + ref.ID}))
	if !res.IsError || !strings.Contains(res.Output, "artifact not found") {
		t.Errorf("cross-tenant grep: %v %q", res.IsError, res.Output)
	}
}

func TestListDirectoryArtifactPrefix(t *testing.T) {
	ctx, b, ref := artCtx(t)
	if _, err := b.Put(t.Context(), artifact.KindSegment, strings.NewReader("seg"), artifact.Meta{}); err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile(`^(art://\S+)  (\d+) bytes  (\S+)  (\S+)$`)

	res := NewListDirectory().Execute(ctx, toolArgs(t, listArgs{Path: "art://acme/l1/spill"}))
	if res.IsError {
		t.Fatal(res.Output)
	}
	lines := strings.Split(res.Output, "\n")
	if len(lines) != 1 {
		t.Fatalf("spill listing: %q", res.Output)
	}
	m := row.FindStringSubmatch(lines[0])
	if m == nil {
		t.Fatalf("row format: %q", lines[0])
	}
	if m[1] != ref.String() || m[3] != "bash" {
		t.Errorf("row = %q", lines[0])
	}
	if _, err := time.Parse(time.RFC3339, m[4]); err != nil {
		t.Errorf("created not RFC3339: %v", err)
	}

	res = NewListDirectory().Execute(ctx, toolArgs(t, listArgs{Path: "art://acme/l1"}))
	if res.IsError {
		t.Fatal(res.Output)
	}
	if !strings.Contains(res.Output, "/spill/") || !strings.Contains(res.Output, "/segment/") {
		t.Errorf("loop listing must include every kind: %q", res.Output)
	}

	for _, p := range []string{"art://other/l1", "art://acme/l2/spill"} {
		res = NewListDirectory().Execute(ctx, toolArgs(t, listArgs{Path: p}))
		if !res.IsError || !strings.Contains(res.Output, "artifact not found") {
			t.Errorf("%s: %v %q", p, res.IsError, res.Output)
		}
	}
}

// TestSpillHintRefReadableByFileTools: the ref a spill marker advertises is
// readable via read_file and grep through the same registry, which stamps the
// loop's binding on every tool call.
func TestSpillHintRefReadableByFileTools(t *testing.T) {
	var sb strings.Builder
	for i := 1; i <= 4000; i++ {
		fmt.Fprintf(&sb, "row %04d filler-filler\n", i)
		if i == 2000 {
			sb.WriteString(spillNeedle + "\n")
		}
	}
	reg := NewRegistry().WithArtifacts(artifact.NewMemStore(), "acme", "loop1")
	reg.Register(bigTool{name: "big_tool", out: sb.String()})
	reg.Register(NewReadFile())
	reg.Register(NewGrep())

	res := reg.Execute(t.Context(), "big_tool", `{}`)
	if strings.Contains(res.Output, spillNeedle) {
		t.Fatal("needle should be elided inline")
	}
	ref := regexp.MustCompile(`art://\S+`).FindString(res.Output)
	if ref == "" {
		t.Fatalf("no ref in spill marker")
	}

	g := reg.Execute(t.Context(), "grep", toolArgs(t, grepArgs{Pattern: spillNeedle, Path: ref}))
	if g.IsError || g.Output != ref+":2001: "+spillNeedle {
		t.Fatalf("grep of the spill ref: %v %q", g.IsError, g.Output)
	}

	read := reg.Execute(t.Context(), "read_file", toolArgs(t, readArgs{Path: ref, StartLine: 2000, EndLine: 2002}))
	if read.IsError {
		t.Fatal(read.Output)
	}
	if read.Output != "row 2000 filler-filler\n"+spillNeedle+"\nrow 2001 filler-filler" {
		t.Errorf("read_file of the spill ref = %q", read.Output)
	}
}
