package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/artifact"
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

func TestRegistryHas(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeTool{name: "alpha"})
	if !r.Has("alpha") {
		t.Error("Has(alpha) = false, want true for a registered tool")
	}
	if r.Has("missing") {
		t.Error("Has(missing) = true, want false for an unknown tool")
	}
	r.Unregister("alpha")
	if r.Has("alpha") {
		t.Error("Has(alpha) = true after Unregister, want false")
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

// TestRegistryConcurrentReadWrite drives concurrent writers (Register/Unregister)
// against concurrent readers (Schemas/Has/Execute/Tools). The fuse mcp-server's
// config-watch goroutine reconciles the registry (Register/Unregister) in-place
// while the MCP Serve loop dispatches tools/list -> Schemas, tools/call ->
// Has+Execute, and resources/read -> Schemas concurrently. Without a mutex on
// Registry the concurrent map read+write panics under -race. Run with
// `go test -race` to exercise the invariant.
func TestRegistryConcurrentReadWrite(t *testing.T) {
	r := NewRegistry()
	// Seed a stable tool so readers always have something to observe.
	r.Register(fakeTool{name: "seed"})

	const goroutines = 8
	const iterations = 200
	var wg sync.WaitGroup

	// Writers: churn a per-goroutine tool name via Register/Unregister.
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			name := fmt.Sprintf("tool-%d", g)
			for i := 0; i < iterations; i++ {
				r.Register(fakeTool{name: name})
				r.Unregister(name)
			}
		}(g)
	}

	// Readers: hammer every read path while the writers mutate the maps.
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				_ = r.Schemas()
				_ = r.Tools()
				_ = r.Has("seed")
				_ = r.Execute(context.Background(), "seed", "{}")
			}
		}()
	}

	wg.Wait()
}

// bindingProbe records the artifact binding its context carries.
type bindingProbe struct {
	got   artifact.Binding
	found bool
}

func (p *bindingProbe) Name() string               { return "probe" }
func (p *bindingProbe) Description() string        { return "probe" }
func (p *bindingProbe) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (p *bindingProbe) Execute(ctx context.Context, _ string) Result {
	p.got, p.found = artifact.BindingFrom(ctx)
	return Result{Output: "ok"}
}

func TestCloneAndSubsetCarryBinding(t *testing.T) {
	store := artifact.NewMemStore()
	r := NewRegistry().WithArtifacts(store, "acme", "loop1")
	r.Register(fakeTool{name: "alpha"})
	want := r.Artifacts()
	if !want.Valid() || want.Store != store || want.Tenant != "acme" || want.Loop != "loop1" {
		t.Fatalf("binding not set: %+v", want)
	}
	if got := r.Clone().Artifacts(); got != want {
		t.Errorf("Clone binding = %+v, want %+v", got, want)
	}
	sub, _ := r.Subset([]string{"alpha"})
	if got := sub.Artifacts(); got != want {
		t.Errorf("Subset binding = %+v, want %+v", got, want)
	}
}

func TestExecuteStampsBinding(t *testing.T) {
	store := artifact.NewMemStore()
	r := NewRegistry().WithArtifacts(store, "acme", "loop1")
	p := &bindingProbe{}
	r.Register(p)
	r.Execute(t.Context(), "probe", `{}`)
	if !p.found || p.got != r.Artifacts() {
		t.Errorf("tool saw binding %+v (found=%v), want %+v", p.got, p.found, r.Artifacts())
	}

	unbound := NewRegistry()
	p2 := &bindingProbe{}
	unbound.Register(p2)
	unbound.Execute(t.Context(), "probe", `{}`)
	if p2.found {
		t.Error("an unbound registry must not stamp a binding")
	}
}
