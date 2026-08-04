// Package tui provides line-based render helpers for one-shot and shell modes.
package tui

import (
	"fmt"
	"io"

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
