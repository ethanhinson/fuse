package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type writeTool struct{}

// NewWriteFile returns the write_file tool: create or overwrite a file,
// creating parent directories as needed.
func NewWriteFile() Tool { return writeTool{} }

func (writeTool) Name() string { return "write_file" }

func (writeTool) Description() string {
	return "Create or overwrite a file with the given content. Parent directories are created automatically."
}

func (writeTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":    map[string]any{"type": "string", "description": "File path to write"},
			"content": map[string]any{"type": "string", "description": "Full file content"},
		},
		"required": []string{"path", "content"},
	}
}

type writeArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (writeTool) Execute(ctx context.Context, args string) Result {
	var a writeArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("bad arguments: %v", err)}
	}
	if a.Path == "" {
		return Result{IsError: true, Output: "path is required"}
	}

	// Use resolvePath to canonicalize so two spellings of one path share
	// tracker state. MkdirAll and WriteFile still use the original path so
	// relative-path inputs work; the canonical form is only for tracking.
	canonical, err := resolvePath(a.Path)
	if err != nil {
		return Result{IsError: true, Output: err.Error()}
	}
	if fi, statErr := os.Stat(canonical); statErr == nil {
		// Refuse to overwrite an existing file unless this agent has read it in
		// full or last wrote it, and the file has not changed since.
		if ft := FileTrackerFrom(ctx); ft != nil {
			if !ft.AuthorizeOverwrite(canonical) {
				return Result{
					IsError: true,
					Output:  fmt.Sprintf("refusing to overwrite %s without reading it first — read_file the file in full before write_file", a.Path),
				}
			}
			if ft.IsStale(canonical, fi.ModTime(), fi.Size()) {
				return Result{
					IsError: true,
					Output:  fmt.Sprintf("refusing to overwrite %s because it changed on disk since this agent last read or wrote it — read_file it again before write_file", a.Path),
				}
			}
		}
	}

	if err := os.MkdirAll(filepath.Dir(a.Path), 0o755); err != nil {
		return Result{IsError: true, Output: err.Error()}
	}
	if err := writeFileAtomic(a.Path, []byte(a.Content)); err != nil {
		return Result{IsError: true, Output: err.Error()}
	}

	// Record the write in the file tracker.
	if ft := FileTrackerFrom(ctx); ft != nil {
		if fi, statErr := os.Stat(canonical); statErr == nil {
			ft.MarkWritten(canonical, fi.ModTime(), fi.Size())
		}
	}

	lines := 1
	for _, c := range a.Content {
		if c == '\n' {
			lines++
		}
	}
	return Result{Output: fmt.Sprintf("wrote %s (%d bytes, %d lines)", a.Path, len(a.Content), lines)}
}
