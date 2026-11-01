package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/runtime"
)

var updateGoldenPrompt = flag.Bool("update-golden", false, "rewrite cmd/fuse golden files")

const loopRootPromptGolden = "testdata/loop_root_prompt.golden"

// TestLoopRootPromptGolden pins, byte for byte, the system prompt the model
// receives for the root of a loop started on the production loop-server wiring
// without a harness spec. The per-process scratch directory is normalized.
func TestLoopRootPromptGolden(t *testing.T) {
	rt, gw, _ := harnessServer(t, harnessOperator)
	h, err := rt.StartLoop(context.Background(), runtime.LoopConfig{Task: "go"})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	_, _ = h.Wait() // the turn cap ends the run; its error is not the point
	calls := gw.snapshot()
	if len(calls) == 0 || len(calls[0].Messages) == 0 {
		t.Fatalf("no model call recorded")
	}
	sys := calls[0].Messages[0].Content
	if dir := sessionScratchDir(); dir != "" {
		sys = strings.ReplaceAll(sys, dir, "<SCRATCH>")
	}
	if *updateGoldenPrompt {
		if err := os.MkdirAll(filepath.Dir(loopRootPromptGolden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(loopRootPromptGolden, []byte(sys), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(loopRootPromptGolden)
	if err != nil {
		t.Fatalf("read golden (run with -update-golden to create it): %v", err)
	}
	if sys != string(want) {
		t.Errorf("loop root system prompt differs from golden:\n got: %q\nwant: %q", sys, string(want))
	}
}
