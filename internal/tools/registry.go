// Package tools provides the tool registry and the built-in tools every model
// can call.
package tools

import (
	"context"
	"fmt"

	"github.com/ethanhinson/fuse/internal/model"
)

// Result is the outcome of a tool execution.
type Result struct {
	Output  string
	IsError bool
}

// Tool is a single named, schema-described, executable capability.
type Tool interface {
	Name() string
	Description() string
	Parameters() map[string]any
	Execute(ctx context.Context, args string) Result
}

// Registry holds tools keyed by name in registration order.
type Registry struct {
	order  []string
	byName map[string]Tool
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byName: map[string]Tool{}}
}

// Register adds a tool. Re-registering a name overwrites it but keeps order.
func (r *Registry) Register(t Tool) {
	if _, exists := r.byName[t.Name()]; !exists {
		r.order = append(r.order, t.Name())
	}
	r.byName[t.Name()] = t
}

// Unregister removes a tool by name. No-op if not present.
func (r *Registry) Unregister(name string) {
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

// Schemas returns the model-facing schema for every registered tool.
func (r *Registry) Schemas() []model.ToolSchema {
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
func (r *Registry) Execute(ctx context.Context, name, args string) Result {
	t, ok := r.byName[name]
	if !ok {
		return Result{IsError: true, Output: fmt.Sprintf("unknown tool %q", name)}
	}
	return t.Execute(ctx, args)
}

// Subset returns a new registry containing only the named tools. The
// "spawn_agent" tool is always force-included even if not in names. Unknown
// names are dropped and returned in the second return value.
func (r *Registry) Subset(names []string) (*Registry, []string) {
	out := NewRegistry()
	// Force-include spawn_agent.
	if t, ok := r.byName["spawn_agent"]; ok {
		out.Register(t)
	}
	var unknown []string
	for _, n := range names {
		if n == "spawn_agent" {
			continue // already added
		}
		if t, ok := r.byName[n]; ok {
			out.Register(t)
		} else {
			unknown = append(unknown, n)
		}
	}
	return out, unknown
}

// DefaultTools returns the Phase 1 built-in tool set.
func DefaultTools() []Tool {
	return []Tool{
		NewBash(),
		NewReadFile(),
		NewWriteFile(),
		NewEditFile(),
		NewListDirectory(),
	}
}
