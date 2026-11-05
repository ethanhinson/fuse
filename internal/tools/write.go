package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ethanhinson/fuse/internal/workspace"
)

type writeTool struct {
	fsys workspace.FileSystem
}

// NewWriteFile returns the write_file tool: create or overwrite a file,
// creating parent directories as needed.
// When fsys is nil the tool falls back to raw os.*.
func NewWriteFile(fsys ...workspace.FileSystem) Tool {
	t := writeTool{}
	if len(fsys) > 0 {
		t.fsys = fsys[0]
	}
	return t
}

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

func (t writeTool) Execute(ctx context.Context, args string) Result {
	var a writeArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("bad arguments: %v", err)}
	}
	if a.Path == "" {
		return Result{IsError: true, Output: "path is required"}
	}
	p := stripWorkspacePrefix(a.Path)
	if t.fsys != nil {
		if err := t.fsys.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return Result{IsError: true, Output: err.Error()}
		}
		if err := t.fsys.WriteFile(p, []byte(a.Content), 0o644); err != nil {
			return Result{IsError: true, Output: err.Error()}
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return Result{IsError: true, Output: err.Error()}
		}
		if err := os.WriteFile(p, []byte(a.Content), 0o644); err != nil {
			return Result{IsError: true, Output: err.Error()}
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
