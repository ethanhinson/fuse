package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type editTool struct{}

// NewEditFile returns the edit_file tool: replace a unique old_string with
// new_string, erroring if old_string is absent or appears more than once.
func NewEditFile() Tool { return editTool{} }

func (editTool) Name() string { return "edit_file" }

func (editTool) Description() string {
	return "Replace old_string with new_string in a file. old_string must appear exactly once."
}

func (editTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":       map[string]any{"type": "string", "description": "File path to edit"},
			"old_string": map[string]any{"type": "string", "description": "Exact text to replace (must be unique)"},
			"new_string": map[string]any{"type": "string", "description": "Replacement text"},
		},
		"required": []string{"path", "old_string", "new_string"},
	}
}

type editArgs struct {
	Path      string `json:"path"`
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

func (editTool) Execute(ctx context.Context, args string) Result {
	var a editArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("bad arguments: %v", err)}
	}

	// Check staleness: if this agent has state for the file, verify the file
	// has not changed on disk since the agent last read or wrote it.
	canonical, err := resolvePath(a.Path)
	if err != nil {
		return Result{IsError: true, Output: err.Error()}
	}
	if ft := FileTrackerFrom(ctx); ft != nil && ft.HasState(canonical) {
		if fi, statErr := os.Stat(canonical); statErr == nil {
			if ft.IsStale(canonical, fi.ModTime(), fi.Size()) {
				return Result{
					IsError: true,
					Output:  fmt.Sprintf("refusing to edit %s because it changed on disk since this agent last read or wrote it — read_file it again first", a.Path),
				}
			}
		}
	}

	data, err := os.ReadFile(a.Path)
	if err != nil {
		return Result{IsError: true, Output: err.Error()}
	}
	content := string(data)
	n := strings.Count(content, a.OldString)
	if n == 0 {
		return Result{IsError: true, Output: "old_string not found in file"}
	}
	if n > 1 {
		return Result{IsError: true, Output: fmt.Sprintf("old_string is not unique (%d matches)", n)}
	}
	updated := strings.Replace(content, a.OldString, a.NewString, 1)
	if err := os.WriteFile(a.Path, []byte(updated), 0o644); err != nil {
		return Result{IsError: true, Output: err.Error()}
	}

	// Record the write in the file tracker.
	if ft := FileTrackerFrom(ctx); ft != nil {
		if fi, statErr := os.Stat(canonical); statErr == nil {
			ft.MarkWritten(canonical, fi.ModTime(), fi.Size())
		}
	}

	return Result{Output: fmt.Sprintf("edited %s (1 replacement)", a.Path)}
}
