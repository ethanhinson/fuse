package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

// codeindexBin resolves the codeindex binary: CODEINDEX_BIN env if set,
// otherwise "codeindex" (looked up on PATH by exec).
func codeindexBin() string {
	if v := os.Getenv("CODEINDEX_BIN"); v != "" {
		return v
	}
	return "codeindex"
}

// codeindexArgs is the shared argument shape for the codeindex tools.
type codeindexArgs struct {
	Symbol string `json:"symbol"`
}

// runCodeindex executes `codeindex <subcommand> <symbol>` and returns its
// combined output.
func runCodeindex(ctx context.Context, subcommand, args string) Result {
	var a codeindexArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("bad arguments: %v", err)}
	}
	if a.Symbol == "" {
		return Result{IsError: true, Output: "symbol is required"}
	}
	cmd := exec.CommandContext(ctx, codeindexBin(), subcommand, a.Symbol)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("%s\ncodeindex %s failed: %v", out, subcommand, err)}
	}
	return Result{Output: string(out)}
}

type codeindexImpactTool struct{}

// NewCodeindexImpact returns the codeindex_impact tool: callers + callees
// blast-radius for a symbol.
func NewCodeindexImpact() Tool { return codeindexImpactTool{} }

func (codeindexImpactTool) Name() string { return "codeindex_impact" }

func (codeindexImpactTool) Description() string {
	return "Blast-radius (callers + callees) for a symbol. Run before modifying, renaming, or deleting it."
}

func (codeindexImpactTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"symbol": map[string]any{"type": "string", "description": "Symbol name to analyze"},
		},
		"required": []string{"symbol"},
	}
}

func (codeindexImpactTool) Execute(ctx context.Context, args string) Result {
	return runCodeindex(ctx, "impact", args)
}

type codeindexCallersTool struct{}

// NewCodeindexCallers returns the codeindex_callers tool: direct callers of a
// symbol.
func NewCodeindexCallers() Tool { return codeindexCallersTool{} }

func (codeindexCallersTool) Name() string { return "codeindex_callers" }

func (codeindexCallersTool) Description() string {
	return "List the direct callers of a symbol."
}

func (codeindexCallersTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"symbol": map[string]any{"type": "string", "description": "Symbol name to analyze"},
		},
		"required": []string{"symbol"},
	}
}

func (codeindexCallersTool) Execute(ctx context.Context, args string) Result {
	return runCodeindex(ctx, "callers", args)
}

// CodeindexTools returns the Phase 1 codeindex tool set.
func CodeindexTools() []Tool {
	return []Tool{NewCodeindexImpact(), NewCodeindexCallers()}
}
