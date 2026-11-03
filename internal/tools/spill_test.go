package tools

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/artifact"
	"github.com/ethanhinson/fuse/internal/event"
)

// bigTool returns a fixed, oversized output with a unique needle in the middle.
type bigTool struct {
	name string
	out  string
}

func (b bigTool) Name() string               { return b.name }
func (b bigTool) Description() string        { return "big" }
func (b bigTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (b bigTool) Execute(context.Context, string) Result {
	return Result{Output: b.out}
}

const spillNeedle = "MIDDLE-needle-4217-unique"

func bigOutput() string {
	head := strings.Repeat("H", 20<<10)
	tail := strings.Repeat("T", 20<<10)
	middle := strings.Repeat("M", 5<<10) + spillNeedle + strings.Repeat("M", 5<<10)
	return head + middle + tail // ~50 KiB
}

// failingStore refuses every Put.
type failingStore struct{ artifact.Store }

func (failingStore) Put(context.Context, artifact.Ref, io.Reader, artifact.Meta) (artifact.Meta, error) {
	return artifact.Meta{}, errors.New("store down")
}

func TestSpillThroughStore(t *testing.T) {
	store := artifact.NewMemStore()
	reg := NewRegistry().WithArtifacts(store, "acme", "loop1")
	reg.Register(bigTool{name: "big_tool", out: bigOutput()})

	res := reg.Execute(t.Context(), "big_tool", `{}`)
	if res.IsError {
		t.Fatal(res.Output)
	}
	if !strings.HasPrefix(res.Output, "H") || !strings.HasSuffix(res.Output, "T") {
		t.Error("head and tail must survive inline")
	}
	if strings.Contains(res.Output, spillNeedle) {
		t.Error("middle should be elided inline")
	}
	if len(res.Output) > spillThreshold+600 {
		t.Errorf("output not bounded: %d bytes", len(res.Output))
	}
	if !strings.Contains(res.Output, "Full output saved as artifact art://acme/loop1/spill/") {
		t.Fatalf("marker missing artifact ref:\n%s", res.Output[spillKeep:spillKeep+600])
	}
	if strings.Contains(res.Output, os.TempDir()) {
		t.Error("marker must not leak a filesystem path")
	}

	metas, err := store.List(t.Context(), "acme", "loop1", artifact.KindSpill)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 {
		t.Fatalf("want 1 spill artifact, got %d", len(metas))
	}
	if metas[0].Tool != "big_tool" {
		t.Errorf("Meta.Tool = %q, want big_tool", metas[0].Tool)
	}
	if !strings.Contains(res.Output, metas[0].Ref.String()) {
		t.Errorf("marker does not name the stored ref %s", metas[0].Ref)
	}
	rc, _, err := store.Open(t.Context(), metas[0].Ref)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	if !strings.Contains(string(body), spillNeedle) || string(body) != bigOutput() {
		t.Error("spilled artifact must hold the complete original output")
	}
}

func TestSpillWithoutBindingTruncatesOnly(t *testing.T) {
	reg := NewRegistry()
	reg.Register(bigTool{name: "big_tool", out: bigOutput()})
	res := reg.Execute(t.Context(), "big_tool", `{}`)
	if !strings.Contains(res.Output, "truncated") {
		t.Error("truncation marker missing")
	}
	if !strings.Contains(res.Output, "Re-run the tool with a narrower scope") {
		t.Error("no-binding marker missing")
	}
	if strings.Contains(res.Output, "Full output saved") || strings.Contains(res.Output, artifact.Scheme) {
		t.Error("no artifact should be advertised without a binding")
	}
	if len(res.Output) > spillThreshold+600 {
		t.Errorf("output not bounded: %d bytes", len(res.Output))
	}
}

func TestSpillStoreFailureFallsBack(t *testing.T) {
	reg := NewRegistry().WithArtifacts(failingStore{artifact.NewMemStore()}, "acme", "loop1")
	reg.Register(bigTool{name: "big_tool", out: bigOutput()})
	res := reg.Execute(t.Context(), "big_tool", `{}`)
	if !strings.Contains(res.Output, "Re-run the tool with a narrower scope") {
		t.Error("store failure must fall back to the narrower-scope marker")
	}
	if strings.Contains(res.Output, "Full output saved") {
		t.Error("no artifact should be advertised when Put fails")
	}
}

func TestSmallOutputPassesThrough(t *testing.T) {
	store := artifact.NewMemStore()
	reg := NewRegistry().WithArtifacts(store, "acme", "loop1")
	small := strings.Repeat("x", 100)
	reg.Register(bigTool{name: "small_tool", out: small})
	res := reg.Execute(t.Context(), "small_tool", `{}`)
	if res.Output != small {
		t.Error("small output must pass through unchanged")
	}
	metas, err := store.List(t.Context(), "acme", "loop1", artifact.KindSpill)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 0 {
		t.Errorf("small output must not be stored, got %d artifacts", len(metas))
	}
}

// TestSpillSurvivesCancelledContext: a tool call whose context is already done
// (timeout, cancel) still records its full output.
func TestSpillSurvivesCancelledContext(t *testing.T) {
	store := artifact.NewMemStore()
	reg := NewRegistry().WithArtifacts(store, "acme", "loop1")
	reg.Register(bigTool{name: "big_tool", out: bigOutput()})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	res := reg.Execute(ctx, "big_tool", `{}`)
	if !strings.Contains(res.Output, "Full output saved as artifact") {
		t.Errorf("cancelled call lost its spill:\n%s", res.Output[spillKeep:spillKeep+400])
	}
}

func TestSpillTenantNormalized(t *testing.T) {
	store := artifact.NewMemStore()
	reg := NewRegistry().WithArtifacts(store, "", "loop1")
	if got := reg.Artifacts().Tenant; got != event.DefaultTenant {
		t.Errorf("tenant = %q, want %q", got, event.DefaultTenant)
	}
}
