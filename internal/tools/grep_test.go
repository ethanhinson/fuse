package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeGrepFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"a.go":          "package a\n\nfunc SpawnAgent() {}\nvar x = 1\n",
		"b.go":          "package b\n\n// SpawnAgent is called here\nfunc caller() { SpawnAgent() }\n",
		"sub/c.txt":     "nothing relevant\n",
		"sub/d.go":      "package sub\nfunc other() {}\n",
		".git/objects":  "binary-ish\x00data",
		"vendor/dep.go": "package dep\nfunc SpawnAgent() {}\n",
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func grepRun(t *testing.T, args grepArgs) Result {
	t.Helper()
	b, _ := json.Marshal(args)
	return NewGrep().Execute(context.Background(), string(b))
}

func TestGrepFindsMatchesWithPathAndLine(t *testing.T) {
	dir := writeGrepFixture(t)
	res := grepRun(t, grepArgs{Pattern: "SpawnAgent", Path: dir})
	if res.IsError {
		t.Fatalf("grep error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "a.go:3:") {
		t.Errorf("missing a.go:3 match:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "b.go:4:") {
		t.Errorf("missing b.go:4 match:\n%s", res.Output)
	}
	if strings.Contains(res.Output, "vendor") {
		t.Errorf("vendor/ should be skipped:\n%s", res.Output)
	}
}

func TestGrepGlobAndCaseInsensitive(t *testing.T) {
	dir := writeGrepFixture(t)
	res := grepRun(t, grepArgs{Pattern: "spawnagent", Path: dir, Glob: "*.go", CaseInsensitive: true})
	if res.IsError {
		t.Fatalf("grep error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "a.go:3:") {
		t.Errorf("case-insensitive glob search failed:\n%s", res.Output)
	}
}

func TestGrepNoMatches(t *testing.T) {
	dir := writeGrepFixture(t)
	res := grepRun(t, grepArgs{Pattern: "zZzNotThere", Path: dir})
	if res.IsError || res.Output != "no matches" {
		t.Errorf("want 'no matches', got err=%v %q", res.IsError, res.Output)
	}
}

func TestGrepMaxResultsTruncates(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := 0; i < 100; i++ {
		b.WriteString("match line\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "many.txt"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	res := grepRun(t, grepArgs{Pattern: "match", Path: dir, MaxResults: 10})
	if res.IsError {
		t.Fatalf("grep error: %s", res.Output)
	}
	if got := strings.Count(res.Output, "many.txt:"); got != 10 {
		t.Errorf("want 10 matches, got %d", got)
	}
	if !strings.Contains(res.Output, "showing first 10") {
		t.Error("truncation marker missing")
	}
}

func TestGrepBadPatternErrors(t *testing.T) {
	res := grepRun(t, grepArgs{Pattern: "(unclosed"})
	if !res.IsError {
		t.Error("bad regex should error")
	}
}

func TestGrepSingleFile(t *testing.T) {
	dir := writeGrepFixture(t)
	res := grepRun(t, grepArgs{Pattern: "caller", Path: filepath.Join(dir, "b.go")})
	if res.IsError {
		t.Fatalf("grep error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "b.go:4:") {
		t.Errorf("single-file search failed:\n%s", res.Output)
	}
}
