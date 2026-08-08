package main

import (
	"context"
	"testing"

	"github.com/ethanhinson/fuse/internal/tools"
)

// staticTool is a minimal tools.Tool for reconcile diffing tests.
type staticTool struct{ name string }

func (t staticTool) Name() string                                 { return t.name }
func (t staticTool) Description() string                          { return t.name + " desc" }
func (t staticTool) Parameters() map[string]any                   { return map[string]any{"type": "object"} }
func (t staticTool) Execute(context.Context, string) tools.Result { return tools.Result{Output: "ok"} }

func toolNames(reg *tools.Registry) []string {
	var names []string
	for _, s := range reg.Schemas() {
		names = append(names, s.Name)
	}
	return names
}

// TestReconcileToolRegistryPushesOnChange: a reconcile whose desired tool set
// differs from the live registry mutates the registry IN PLACE (so the server's
// shared pointer sees it) and pushes exactly one fuse://tools update. An
// unchanged reconcile pushes nothing.
func TestReconcileToolRegistryPushesOnChange(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(staticTool{name: "a"})
	reg.Register(staticTool{name: "b"})

	var pushes int
	push := func() { pushes++ }

	// Unchanged: same tool set → no push, registry unchanged.
	same := []tools.Tool{staticTool{name: "a"}, staticTool{name: "b"}}
	reconcileMCPToolRegistry(reg, same, push)
	if pushes != 0 {
		t.Errorf("unchanged reconcile pushed %d times, want 0", pushes)
	}
	if got := len(toolNames(reg)); got != 2 {
		t.Errorf("unchanged reconcile left %d tools, want 2", got)
	}

	// Changed: drop "b", add "c" → exactly one push, registry reflects new set.
	changed := []tools.Tool{staticTool{name: "a"}, staticTool{name: "c"}}
	reconcileMCPToolRegistry(reg, changed, push)
	if pushes != 1 {
		t.Errorf("changed reconcile pushed %d times, want 1", pushes)
	}
	if reg.Has("b") {
		t.Error("reconcile should have unregistered dropped tool b")
	}
	if !reg.Has("c") {
		t.Error("reconcile should have registered new tool c")
	}
	if !reg.Has("a") {
		t.Error("reconcile should keep unchanged tool a")
	}

	// A second identical reconcile is a no-op (no further push).
	reconcileMCPToolRegistry(reg, changed, push)
	if pushes != 1 {
		t.Errorf("idempotent reconcile pushed again (total %d), want 1", pushes)
	}
}
