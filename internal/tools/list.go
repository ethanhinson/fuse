package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type listTool struct{}

// NewListDirectory returns the list_directory tool with optional glob filter.
func NewListDirectory() Tool { return listTool{} }

func (listTool) Name() string { return "list_directory" }

func (listTool) Description() string {
	return "List entries in a directory, optionally filtered by a shell glob pattern."
}

func (listTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "Directory to list (default: current)"},
			"glob": map[string]any{"type": "string", "description": "Optional glob to filter entries, e.g. *.go"},
		},
	}
}

type listArgs struct {
	Path string `json:"path"`
	Glob string `json:"glob"`
}

func (listTool) Execute(ctx context.Context, args string) Result {
	var a listArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("bad arguments: %v", err)}
	}
	if a.Path == "" {
		a.Path = "."
	}
	entries, err := os.ReadDir(a.Path)
	if err != nil {
		return Result{IsError: true, Output: err.Error()}
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		if a.Glob != "" {
			ok, err := filepath.Match(a.Glob, e.Name())
			if err != nil {
				return Result{IsError: true, Output: fmt.Sprintf("bad glob: %v", err)}
			}
			if !ok {
				continue
			}
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return Result{Output: strings.Join(names, "\n")}
}
