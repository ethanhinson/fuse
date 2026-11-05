package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ethanhinson/fuse/internal/workspace"
)

type listTool struct {
	fsys workspace.FileSystem
}

// NewListDirectory returns the list_directory tool with optional glob filter.
// When fsys is nil the tool falls back to raw os.*.
func NewListDirectory(fsys ...workspace.FileSystem) Tool {
	t := listTool{}
	if len(fsys) > 0 {
		t.fsys = fsys[0]
	}
	return t
}

func (listTool) Name() string { return "list_directory" }

func (listTool) Description() string {
	return "List entries in a directory, optionally filtered by a shell glob pattern. " +
		"path may also be an art:// prefix (art://<tenant>/<loop> or art://<tenant>/<loop>/<kind>) to list this loop's artifacts."
}

func (listTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "Directory or art:// artifact prefix to list (default: current)"},
			"glob": map[string]any{"type": "string", "description": "Optional glob to filter entries, e.g. *.go"},
		},
	}
}

type listArgs struct {
	Path string `json:"path"`
	Glob string `json:"glob"`
}

func (t listTool) Execute(ctx context.Context, args string) Result {
	var a listArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("bad arguments: %v", err)}
	}
	if out, handled, err := listArtifacts(ctx, a.Path); handled {
		if err != nil {
			return Result{IsError: true, Output: err.Error()}
		}
		return Result{Output: out}
	}
	p := stripWorkspacePrefix(a.Path)
	if p == "" {
		p = "."
	}

	var entries []string
	if t.fsys != nil {
		des, err := t.fsys.ReadDir(p)
		if err != nil {
			return Result{IsError: true, Output: err.Error()}
		}
		for _, e := range des {
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
			entries = append(entries, name)
		}
	} else {
		p2 := a.Path
		if p2 == "" {
			p2 = "."
		}
		des, err := os.ReadDir(p2)
		if err != nil {
			return Result{IsError: true, Output: err.Error()}
		}
		for _, e := range des {
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
			entries = append(entries, name)
		}
	}
	sort.Strings(entries)
	return Result{Output: strings.Join(entries, "\n")}
}
