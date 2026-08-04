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
	if err := os.MkdirAll(filepath.Dir(a.Path), 0o755); err != nil {
		return Result{IsError: true, Output: err.Error()}
	}
	if err := os.WriteFile(a.Path, []byte(a.Content), 0o644); err != nil {
		return Result{IsError: true, Output: err.Error()}
	}
	lines := 1
	for _, c := range a.Content {
		if c == '\n' {
			lines++
		}
	}
	return Result{Output: fmt.Sprintf("wrote %s (%d bytes, %d lines)", a.Path, len(a.Content), lines)}
}
