package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/archive"
	"gopkg.in/yaml.v3"
)

func TestSpillOutputSmallPassesThrough(t *testing.T) {
	SetSpillDir("")
	small := strings.Repeat("x", 100)
	if got := SpillOutput("bash", small); got != small {
		t.Error("small output must pass through unchanged")
	}
}

func TestSpillOutputWritesFullCopyAndKeepsHeadTail(t *testing.T) {
	dir := t.TempDir()
	SetSpillDir(dir)
	defer SetSpillDir("")

	head := strings.Repeat("H", spillKeep)
	middle := strings.Repeat("M", spillThreshold)
	tail := strings.Repeat("T", spillKeep)
	full := head + middle + tail

	got := SpillOutput("bash", full)
	if !strings.HasPrefix(got, "H") || !strings.HasSuffix(got, "T") {
		t.Error("head and tail must survive inline")
	}
	if strings.Count(got, "M") > 0 {
		t.Error("middle should be elided inline")
	}
	if !strings.Contains(got, "Full output saved to: ") {
		t.Fatalf("marker missing spill path:\n%s", got[:300])
	}
	// The spill file holds the complete original.
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected 1 spill file, got %d (%v)", len(entries), err)
	}
	data, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != full {
		t.Error("spill file must contain the complete original output")
	}
	if !strings.Contains(entries[0].Name(), "bash") {
		t.Errorf("spill filename should carry the tool name: %s", entries[0].Name())
	}
}

func TestSpillOutputWithoutDirStillTruncates(t *testing.T) {
	SetSpillDir("")
	full := strings.Repeat("z", spillThreshold*2)
	got := SpillOutput("bash", full)
	if len(got) > spillThreshold+500 {
		t.Errorf("output not bounded without spill dir: %d bytes", len(got))
	}
	if strings.Contains(got, "Full output saved") {
		t.Error("no spill path should be advertised when disabled")
	}
	if !strings.Contains(got, "truncated") {
		t.Error("truncation marker missing")
	}
}

// TestSweepSpillArchivesStale: sweepSpillDir now ARCHIVES stale spill files
// (gzip + spill-domain metadata sidecar) instead of deleting them; the full
// output stays recoverable byte-for-byte (change 0030 scope expansion).
func TestSweepSpillArchivesStale(t *testing.T) {
	dir := t.TempDir()
	// A spill file named like writeSpill produces: "<unix>_<seq>_<tool>.txt".
	name := "1700000000_001_read_file.txt"
	path := filepath.Join(dir, name)
	body := "needle 4217 lives here\n" + strings.Repeat("payload ", 100)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}

	sweepSpillDir(dir)

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("stale spill original should be archived (removed) after gzip")
	}
	if _, err := os.Stat(path + ".gz"); err != nil {
		t.Errorf("stale spill not gzip-archived: %v", err)
	}
	// Recoverable byte-for-byte.
	got, err := archive.Open(path)
	if err != nil {
		t.Fatalf("recover archived spill: %v", err)
	}
	if string(got) != body {
		t.Error("archived spill not byte-identical")
	}
	// Sidecar carries spill-domain fields.
	sc, err := os.ReadFile(path + ".gz.meta.yml")
	if err != nil {
		t.Fatalf("spill sidecar missing: %v", err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(sc, &m); err != nil {
		t.Fatalf("spill sidecar not valid YAML: %v", err)
	}
	if m["tool_name"] != "read_file" {
		t.Errorf("spill sidecar tool_name = %v, want read_file", m["tool_name"])
	}
	if _, ok := m["created_unix"]; !ok {
		t.Error("spill sidecar missing created_unix")
	}
	if head, _ := m["head"].(string); !strings.Contains(head, "needle 4217") {
		t.Errorf("spill sidecar head preview missing the needle: %q", head)
	}
}

// TestSweepSpillSkipsGzAndMeta: an already-archived .gz and its .meta.yml are
// left alone (idempotent, no double-archive).
func TestSweepSpillSkipsGzAndMeta(t *testing.T) {
	dir := t.TempDir()
	gz := filepath.Join(dir, "1700000000_001_bash.txt.gz")
	meta := gz + ".meta.yml"
	for _, p := range []string{gz, meta} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		past := time.Now().Add(-30 * 24 * time.Hour)
		_ = os.Chtimes(p, past, past)
	}
	sweepSpillDir(dir)
	for _, p := range []string{gz, meta} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("already-archived file %s should be left alone", filepath.Base(p))
		}
	}
	// No sidecar-of-a-sidecar or double-gz.
	if _, err := os.Stat(gz + ".gz"); err == nil {
		t.Error("double-archived a .gz")
	}
}

// TestSpillRecoveryHintResolvesAfterArchival: the recovery hint names the spill
// path; after archival read_file must still resolve it via the .gz fallback.
func TestSpillRecoveryHintResolvesAfterArchival(t *testing.T) {
	dir := t.TempDir()
	SetSpillDir(dir)
	defer SetSpillDir("")

	head := strings.Repeat("H", spillKeep)
	middle := "MIDDLE-needle-4217-" + strings.Repeat("M", spillThreshold)
	tail := strings.Repeat("T", spillKeep)
	marker := SpillOutput("bash", head+middle+tail)

	// Extract the advertised path from the marker.
	const anchor = "Full output saved to: "
	i := strings.Index(marker, anchor)
	if i < 0 {
		t.Fatalf("no spill path in marker:\n%s", marker)
	}
	rest := marker[i+len(anchor):]
	spillPath := rest[:strings.IndexByte(rest, ' ')]

	// Archive it (simulating the sweep), then read_file the ORIGINAL path.
	if _, err := archive.Archive(spillPath, nil); err != nil {
		t.Fatalf("archive spill: %v", err)
	}
	res := NewReadFile().Execute(t.Context(), `{"path":"`+spillPath+`"}`)
	if res.IsError {
		t.Fatalf("read_file could not resolve archived spill path %q: %s", spillPath, res.Output)
	}
	if !strings.Contains(res.Output, "needle-4217") {
		t.Errorf("archived spill middle not recoverable via the hinted path")
	}
}

func TestRegistryExecuteAppliesSpill(t *testing.T) {
	dir := t.TempDir()
	SetSpillDir(dir)
	defer SetSpillDir("")

	// read_file on a file bigger than the spill threshold but under the
	// 1000-line window: the registry must bound it and save a full copy.
	big := strings.Repeat("wide-line ", 300) // ~3KB per line
	var sb strings.Builder
	for i := 0; i < 20; i++ {
		sb.WriteString(big)
		sb.WriteByte('\n')
	}
	p := filepath.Join(t.TempDir(), "big.txt")
	if err := os.WriteFile(p, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := NewRegistry()
	reg.Register(NewReadFile())
	res := reg.Execute(t.Context(), "read_file", `{"path":"`+p+`"}`)
	if res.IsError {
		t.Fatal(res.Output)
	}
	if len(res.Output) > spillThreshold+500 {
		t.Errorf("registry did not bound output: %d bytes", len(res.Output))
	}
	if !strings.Contains(res.Output, "Full output saved to: ") {
		t.Error("spill marker missing from registry-executed result")
	}
}
