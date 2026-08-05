package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
