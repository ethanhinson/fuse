package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ethanhinson/fuse/internal/archive"
)

type readTool struct{}

// NewReadFile returns the read_file tool with optional 1-based line range.
func NewReadFile() Tool { return readTool{} }

func (readTool) Name() string { return "read_file" }

// readDefaultWindow bounds an unranged read. Whole-file dumps are the main
// driver of context ballooning; a windowed default with an explicit
// continuation footer teaches the model to page instead.
const readDefaultWindow = 1000

func (readTool) Description() string {
	return "Read a file's contents with 1-based inclusive line ranges. " +
		"Without a range, the first 1000 lines are returned; the footer says how to continue. " +
		"Prefer grep to locate, then read just the ranges you need."
}

func (readTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":       map[string]any{"type": "string", "description": "File path to read"},
			"start_line": map[string]any{"type": "integer", "description": "First line (1-based, inclusive)"},
			"end_line":   map[string]any{"type": "integer", "description": "Last line (1-based, inclusive)"},
		},
		"required": []string{"path"},
	}
}

type readArgs struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

func (readTool) Execute(ctx context.Context, args string) Result {
	var a readArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("bad arguments: %v", err)}
	}
	// Transparent decompression: a ".gz" (or a bare path whose file is now a
	// "<path>.gz" archive) is gunzipped so the model's generic read_file keeps
	// working on archived spill/log files. Plaintext files pass through unchanged.
	// A truncated/corrupt gzip returns an error, never a panic.
	data, err := archive.Open(a.Path)
	if err != nil {
		return Result{IsError: true, Output: err.Error()}
	}
	// Refuse binaries: raw executable bytes poison the model context and the
	// control characters corrupt terminal rendering. The check runs on the
	// DECOMPRESSED bytes so a gzipped binary is still refused.
	head := data
	if len(head) > 8192 {
		head = head[:8192]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return Result{IsError: true, Output: fmt.Sprintf(
			"%s appears to be a binary file (%d bytes) — not readable as text. Use bash (file, strings, otool, xxd) for binary inspection.",
			a.Path, len(data))}
	}
	lines := strings.Split(string(data), "\n")
	total := len(lines)

	start := a.StartLine
	if start < 1 {
		start = 1
	}
	end := a.EndLine
	windowed := a.StartLine == 0 && a.EndLine == 0 && total > readDefaultWindow
	if windowed {
		// Unranged read of a large file: return the first window with an
		// explicit continuation affordance instead of the whole file.
		end = readDefaultWindow
	} else if end == 0 || end > total {
		end = total
	}
	if start > total {
		return Result{IsError: true, Output: fmt.Sprintf("start_line %d beyond file length %d", start, total)}
	}
	// Inverted or negative ranges (end_line < start_line) would slice out of
	// bounds — tell the model instead of panicking.
	if end < start {
		return Result{IsError: true, Output: fmt.Sprintf(
			"invalid range: end_line %d is before start_line %d (file has %d lines)", a.EndLine, a.StartLine, total)}
	}

	out := strings.Join(lines[start-1:end], "\n")
	if windowed {
		out += fmt.Sprintf("\n(Showing lines %d-%d of %d. Use start_line=%d to continue.)", start, end, total, end+1)
	}
	return Result{Output: out}
}
