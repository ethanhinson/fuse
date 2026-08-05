// Package tui provides line-based render helpers for one-shot and shell modes.
package tui

import (
	"fmt"
	"io"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/tools"
)

// Renderer writes agent events as plain text lines.
type Renderer struct {
	w       io.Writer
	verbose bool
}

// NewRenderer builds a Renderer. When verbose is false, tool call args and
// results are truncated for scannability.
func NewRenderer(w io.Writer, verbose bool) *Renderer {
	return &Renderer{w: w, verbose: verbose}
}

const previewLimit = 120

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Assistant prints model prose.
func (r *Renderer) Assistant(text string) {
	fmt.Fprintf(r.w, "%s\n", text)
}

// ToolCall prints a tool invocation line.
func (r *Renderer) ToolCall(name, args string) {
	if r.verbose {
		fmt.Fprintf(r.w, "→ %s(%s)\n", name, args)
		return
	}
	fmt.Fprintf(r.w, "→ %s(%s)\n", name, truncate(args, previewLimit))
}

// ToolResult prints a tool result line.
func (r *Renderer) ToolResult(name string, res tools.Result) {
	prefix := "←"
	out := res.Output
	if res.IsError {
		prefix = "✗"
	}
	if !r.verbose {
		out = truncate(out, previewLimit)
	}
	fmt.Fprintf(r.w, "%s %s\n", prefix, out)
}

// Errorf prints an error line.
func (r *Renderer) Errorf(format string, a ...any) {
	fmt.Fprintf(r.w, "! "+format+"\n", a...)
}

// Tokens is a no-op for one-shot mode.
func (r *Renderer) Tokens(_, _ int) {}

// ─── NodeRenderer ─────────────────────────────────────────────────────────────

// NodeRenderer records agent loop events onto an AgentNode in the tree.
// It implements agent.Renderer so child agents can emit events into the tree.
type NodeRenderer struct {
	node *agent.AgentNode
	tree *agent.AgentTree
}

// NewNodeRenderer creates a NodeRenderer targeting the given node.
func NewNodeRenderer(node *agent.AgentNode, tree *agent.AgentTree) *NodeRenderer {
	return &NodeRenderer{node: node, tree: tree}
}

func (r *NodeRenderer) Assistant(text string) {
	r.node.AddEvent(agent.AgentEvent{
		Kind:    agent.KindAssistant,
		Name:    "assistant",
		Payload: map[string]any{"text": text},
		TS:      time.Now(),
	})
	r.tree.Emit(agent.TreeUpdate{NodeID: r.node.ID})
}

func (r *NodeRenderer) ToolCall(name, args string) {
	r.node.AddEvent(agent.AgentEvent{
		Kind:    agent.KindToolCall,
		Name:    name,
		Payload: map[string]any{"args": args},
		TS:      time.Now(),
	})
	r.tree.Emit(agent.TreeUpdate{NodeID: r.node.ID})
}

func (r *NodeRenderer) ToolResult(name string, res tools.Result) {
	r.node.AddEvent(agent.AgentEvent{
		Kind:    agent.KindToolResult,
		Name:    name,
		Payload: map[string]any{"output": res.Output, "error": res.IsError},
		TS:      time.Now(),
	})
	r.tree.Emit(agent.TreeUpdate{NodeID: r.node.ID})
}

func (r *NodeRenderer) Errorf(format string, a ...any) {
	r.node.AddEvent(agent.AgentEvent{
		Kind:    agent.KindError,
		Name:    "error",
		Payload: map[string]any{"error": fmt.Sprintf(format, a...)},
		TS:      time.Now(),
	})
	r.tree.Emit(agent.TreeUpdate{NodeID: r.node.ID})
}

func (r *NodeRenderer) Tokens(input, output int) {
	r.node.UpdateTokens(input, output)
	r.node.AddEvent(agent.AgentEvent{
		Kind:    agent.KindTokens,
		Payload: map[string]any{"in": input, "out": output},
		TS:      time.Now(),
	})
	r.tree.Emit(agent.TreeUpdate{NodeID: r.node.ID})
}

var _ agent.Renderer = (*NodeRenderer)(nil)

// ─── MultiRenderer ────────────────────────────────────────────────────────────

// MultiRenderer fans out agent.Renderer calls to multiple implementations.
type MultiRenderer struct {
	renderers []agent.Renderer
}

// NewMultiRenderer creates a MultiRenderer.
func NewMultiRenderer(renderers ...agent.Renderer) *MultiRenderer {
	return &MultiRenderer{renderers: renderers}
}

func (r *MultiRenderer) Assistant(text string) {
	for _, rr := range r.renderers {
		rr.Assistant(text)
	}
}

func (r *MultiRenderer) ToolCall(name, args string) {
	for _, rr := range r.renderers {
		rr.ToolCall(name, args)
	}
}

func (r *MultiRenderer) ToolResult(name string, res tools.Result) {
	for _, rr := range r.renderers {
		rr.ToolResult(name, res)
	}
}

func (r *MultiRenderer) Errorf(format string, a ...any) {
	for _, rr := range r.renderers {
		rr.Errorf(format, a...)
	}
}

func (r *MultiRenderer) Tokens(input, output int) {
	for _, rr := range r.renderers {
		rr.Tokens(input, output)
	}
}

var _ agent.Renderer = (*MultiRenderer)(nil)
