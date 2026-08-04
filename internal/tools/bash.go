package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

type bashTool struct{}

// NewBash returns the bash tool: runs a shell command with an optional
// per-call timeout and working directory.
func NewBash() Tool { return bashTool{} }

func (bashTool) Name() string { return "bash" }

func (bashTool) Description() string {
	return "Execute a shell command via /bin/sh. Supports an optional timeout (seconds) and working directory."
}

func (bashTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command":         map[string]any{"type": "string", "description": "Shell command to run"},
			"timeout_seconds": map[string]any{"type": "integer", "description": "Kill the command after this many seconds (default 120)"},
			"working_dir":     map[string]any{"type": "string", "description": "Directory to run in (default: current)"},
		},
		"required": []string{"command"},
	}
}

type bashArgs struct {
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	WorkingDir     string `json:"working_dir"`
}

func (bashTool) Execute(ctx context.Context, args string) Result {
	var a bashArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("bad arguments: %v", err)}
	}
	if a.Command == "" {
		return Result{IsError: true, Output: "command is required"}
	}
	if err := ctx.Err(); err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("context: %v", err)}
	}
	timeout := time.Duration(a.TimeoutSeconds) * time.Second
	if a.TimeoutSeconds == 0 {
		timeout = 120 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "/bin/sh", "-c", a.Command)
	if a.WorkingDir != "" {
		cmd.Dir = a.WorkingDir
	}
	out, err := cmd.CombinedOutput()
	if runCtx.Err() == context.DeadlineExceeded {
		return Result{IsError: true, Output: fmt.Sprintf("command timed out after %s\n%s", timeout, out)}
	}
	if err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("%s\nerror: %v", out, err)}
	}
	return Result{Output: string(out)}
}
