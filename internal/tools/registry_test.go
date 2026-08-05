package tools

import (
	"context"
	"strings"
	"testing"
)

type fakeTool struct{ name string }

func (f fakeTool) Name() string               { return f.name }
func (f fakeTool) Description() string        { return "fake" }
func (f fakeTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (f fakeTool) Execute(context.Context, string) Result {
	return Result{Output: "ok"}
}

func TestRegistrySchemasAndExecute(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeTool{name: "alpha"})
	schemas := r.Schemas()
	if len(schemas) != 1 || schemas[0].Name != "alpha" {
		t.Fatalf("schemas = %+v", schemas)
	}
	res := r.Execute(context.Background(), "alpha", "{}")
	if res.IsError || res.Output != "ok" {
		t.Fatalf("execute = %+v", res)
	}
}

func TestRegistryUnknownToolIsError(t *testing.T) {
	r := NewRegistry()
	res := r.Execute(context.Background(), "missing", "{}")
	if !res.IsError {
		t.Fatal("expected error result for unknown tool")
	}
}

type panickyTool struct{}

func (panickyTool) Name() string               { return "boom" }
func (panickyTool) Description() string        { return "" }
func (panickyTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (panickyTool) Execute(context.Context, string) Result {
	panic("tool exploded")
}

// TestRegistryExecuteRecoversPanics: a panicking tool must become an error
// Result, never crash the process (tool calls run on agent goroutines).
func TestRegistryExecuteRecoversPanics(t *testing.T) {
	r := NewRegistry()
	r.Register(panickyTool{})
	res := r.Execute(context.Background(), "boom", "{}")
	if !res.IsError {
		t.Fatal("panic should surface as error Result")
	}
	if !strings.Contains(res.Output, "tool exploded") {
		t.Errorf("panic message missing: %q", res.Output)
	}
}
