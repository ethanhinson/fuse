package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type readTool struct{}

// NewReadFile returns the read_file tool with optional 1-based line range.
func NewReadFile() Tool { return readTool{} }

func (readTool) Name() string { return "read_file" }

func (readTool) Description() string {
	return "Read a file's contents. Optionally restrict to a 1-based inclusive line range."
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
	data, err := os.ReadFile(a.Path)
	if err != nil {
		return Result{IsError: true, Output: err.Error()}
	}
	if a.StartLine == 0 && a.EndLine == 0 {
		return Result{Output: string(data)}
	}
	lines := strings.Split(string(data), "\n")
	start := a.StartLine
	if start < 1 {
		start = 1
	}
	end := a.EndLine
	if end == 0 || end > len(lines) {
		end = len(lines)
	}
	if start > len(lines) {
		return Result{IsError: true, Output: fmt.Sprintf("start_line %d beyond file length %d", start, len(lines))}
	}
	return Result{Output: strings.Join(lines[start-1:end], "\n")}
}
