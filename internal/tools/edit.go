package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ethanhinson/fuse/internal/workspace"
)

type editTool struct {
	fsys workspace.FileSystem
}

// NewEditFile returns the edit_file tool: replace a unique old_string with
// new_string, erroring if old_string is absent or appears more than once.
// When fsys is nil the tool falls back to raw os.*.
func NewEditFile(fsys ...workspace.FileSystem) Tool {
	t := editTool{}
	if len(fsys) > 0 {
		t.fsys = fsys[0]
	}
	return t
}

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

func (t editTool) Execute(ctx context.Context, args string) Result {
	var a editArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("bad arguments: %v", err)}
	}
	p := stripWorkspacePrefix(a.Path)

	var data []byte
	if t.fsys != nil {
		f, err := t.fsys.Open(p)
		if err != nil {
			return Result{IsError: true, Output: err.Error()}
		}
		defer f.Close()
		data, err = io.ReadAll(f)
		if err != nil {
			return Result{IsError: true, Output: err.Error()}
		}
	} else {
		var err error
		data, err = os.ReadFile(p)
		if err != nil {
			return Result{IsError: true, Output: err.Error()}
		}
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
	if t.fsys != nil {
		if err := t.fsys.WriteFile(p, []byte(updated), 0o644); err != nil {
			return Result{IsError: true, Output: err.Error()}
		}
	} else {
		if err := os.WriteFile(p, []byte(updated), 0o644); err != nil {
			return Result{IsError: true, Output: err.Error()}
		}
	}
	return Result{Output: fmt.Sprintf("edited %s (1 replacement)", a.Path)}
}
