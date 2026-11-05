package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/runtime"
)

var updateGoldenPrompt = flag.Bool("update-golden", false, "rewrite cmd/fuse golden files")

const loopRootPromptGolden = "testdata/loop_root_prompt.golden"

const loopSpecFirstRequestGolden = "testdata/loop_spec_first_request.golden"

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

// TestLoopSpecFirstRequestGolden pins, byte for byte, the whole first
// chat-completions request body an ordinary harness spec (one that does not ask
// for a tool-less loop) sends on the production loop-server wiring: model,
// messages and every tool schema. Per-process values are normalized.
func TestLoopSpecFirstRequestGolden(t *testing.T) {
	raw := &rawCapture{next: &recordingGateway{}}
	deps, _ := harnessServerWith(t, harnessOperator, raw)
	rt := runtime.New(deps)
	one := uint32(1)
	h, err := rt.StartLoop(context.Background(), runtime.LoopConfig{Task: "go", Harness: &harnessv1.HarnessSpec{MaxTurns: &one}})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	_, _ = h.Wait() // the turn cap ends the run; its error is not the point
	raw.mu.Lock()
	if len(raw.bodies) == 0 {
		raw.mu.Unlock()
		t.Fatal("no model call recorded")
	}
	body := string(raw.bodies[0])
	raw.mu.Unlock()
	body = normalizeRequestBody(body)
	if *updateGoldenPrompt {
		if err := os.MkdirAll(filepath.Dir(loopSpecFirstRequestGolden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(loopSpecFirstRequestGolden, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(loopSpecFirstRequestGolden)
	if err != nil {
		t.Fatalf("read golden (run with -update-golden to create it): %v", err)
	}
	if body != string(want) {
		t.Errorf("first request body differs from golden:\n got: %q\nwant: %q", body, string(want))
	}
}

// normalizeRequestBody replaces per-process values in a request body: the
// per-test HOME (a t.TempDir path). The process-global scratch dir is retired
// (change 0129): the system prompt advertises "scratch/" (workspace-relative),
// never an absolute host path, so there is nothing to normalize out.
func normalizeRequestBody(s string) string {
	if home := os.Getenv("HOME"); home != "" {
		s = strings.ReplaceAll(s, home, "<HOME>")
	}
	return s
}
