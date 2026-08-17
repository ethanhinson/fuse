// Package tools provides the tool registry and the built-in tools every model
// can call.
package tools

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"

	"github.com/ethanhinson/fuse/internal/model"
)

// Result is the outcome of a tool execution.
type Result struct {
	Output  string
	IsError bool
	// Denied marks a permission-gate policy denial (change 0067). Only the
	// permission gate sets it — gate denial results return directly to the agent
	// loop, never through the registry wrappers — so the loop can distinguish a
	// policy denial from an ordinary tool error without string matching, and
	// stop counting denied retries toward the generic doom-loop abort.
	Denied bool
	// DenyLayer, set only when Denied, names the gate pipeline layer that denied
	// (permissions.Layer* constants, e.g. "rules", "classifier", "valve").
	DenyLayer string
}

// Tool is a single named, schema-described, executable capability.
type Tool interface {
	Name() string
	Description() string
	Parameters() map[string]any
	Execute(ctx context.Context, args string) Result
}

// Registry holds tools keyed by name in registration order.
//
// It is safe for concurrent use: the fuse mcp-server's config-watch goroutine
// reconciles the registry in-place (Register/Unregister) while the MCP Serve
// loop concurrently reads it (tools/list -> Schemas, tools/call -> Has+Execute,
// resources/read -> Schemas). mu guards order and byName: read methods take an
// RLock, write methods a Lock.
type Registry struct {
	mu     sync.RWMutex
	order  []string
	byName map[string]Tool
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byName: map[string]Tool{}}
}

// Register adds a tool. Re-registering a name overwrites it but keeps order.
func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byName[t.Name()]; !exists {
		r.order = append(r.order, t.Name())
	}
	r.byName[t.Name()] = t
}

// Unregister removes a tool by name. No-op if not present.
func (r *Registry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byName[name]; !ok {
		return
	}
	delete(r.byName, name)
	for i, n := range r.order {
		if n == name {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
}

// Has reports whether a tool is registered under name.
func (r *Registry) Has(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.byName[name]
	return ok
}

// Tools returns every registered tool in registration order.
func (r *Registry) Tools() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.byName[name])
	}
	return out
}

// Schemas returns the model-facing schema for every registered tool.
func (r *Registry) Schemas() []model.ToolSchema {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]model.ToolSchema, 0, len(r.order))
	for _, name := range r.order {
		t := r.byName[name]
		out = append(out, model.ToolSchema{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Parameters(),
		})
	}
	return out
}

// Execute runs the named tool, returning an error Result if it is unknown.
// Oversized outputs are spill-truncated centrally (head+tail inline, full
// output to a recoverable spill file) so no single result can flood the
// conversation context. A panicking tool becomes an error Result — tool
// calls run on agent goroutines, where an unrecovered panic kills the whole
// process (observed live: an inverted read range took down the TUI).
func (r *Registry) Execute(ctx context.Context, name, args string) (res Result) {
	r.mu.RLock()
	t, ok := r.byName[name]
	r.mu.RUnlock()
	if !ok {
		return Result{IsError: true, Output: fmt.Sprintf("unknown tool %q", name)}
	}
	defer func() {
		if rec := recover(); rec != nil {
			stack := debug.Stack()
			if len(stack) > 2048 {
				stack = stack[:2048]
			}
			res = Result{IsError: true, Output: fmt.Sprintf("tool %s panicked: %v\n%s", name, rec, stack)}
		}
	}()
	res = t.Execute(ctx, args)
	res.Output = SpillOutput(name, res.Output)
	return res
}

// Subset returns a new registry containing only the named tools. Unknown names
// are dropped and returned in the second return value.
//
// spawn_agent is NOT force-included (change 0034): a parent that omits it from
// the requested subset produces a child that structurally cannot spawn. The
// decision to (re-)wire a child's spawn tool lives in the child builder, which
// binds the tool to the child's own node before registering it — so a subset
// that names spawn_agent selects the parent's tool here and the builder replaces
// it with the child-wired one.
func (r *Registry) Subset(names []string) (*Registry, []string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := NewRegistry()
	var unknown []string
	for _, n := range names {
		if t, ok := r.byName[n]; ok {
			out.Register(t)
		} else {
			unknown = append(unknown, n)
		}
	}
	return out, unknown
}

// Clone returns a shallow copy of the registry with the same tool references.
func (r *Registry) Clone() *Registry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := &Registry{
		order:  make([]string, len(r.order)),
		byName: make(map[string]Tool, len(r.byName)),
	}
	copy(out.order, r.order)
	for k, v := range r.byName {
		out.byName[k] = v
	}
	return out
}

// DefaultTools returns the Phase 1 built-in tool set.
func DefaultTools() []Tool {
	return []Tool{
		NewBash(),
		NewReadFile(),
		NewWriteFile(),
		NewEditFile(),
		NewListDirectory(),
		NewGrep(),
		NewSegmentRead(currentSegmentsDir),
	}
}
