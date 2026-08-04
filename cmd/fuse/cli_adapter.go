package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"

	"github.com/ethanhinson/fuse/internal/hitl"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
)

// CLIAdapter implements agent.Completer by invoking the claude CLI subprocess.
//
// Fuse's tool registry is exposed to Claude via an MCP config that points to
// `fuse mcp-server` (this binary's own mcp-server subcommand). Tool approval
// requests from the subprocess are relayed back to the parent TUI over a Unix
// socket HITL bridge, giving the user full HITL control even though Claude
// runs in a child process.
//
// Each Complete() call:
//  1. Starts a per-call HITL relay server on a fresh Unix socket.
//  2. Writes a temp MCP config JSON pointing to `fuse mcp-server`.
//  3. Spawns `claude --print` with FUSE_HITL_SOCKET set in the environment.
//  4. The claude subprocess spawns `fuse mcp-server`, which connects to the
//     socket for each tool call needing approval.
//  5. Returns CompletionResp with Claude's final text (ToolCalls is always
//     nil; Claude handles multi-step tool use internally).
type CLIAdapter struct {
	fuseExe string
	approve permissions.ApprovalFunc
	counter atomic.Uint64
}

// newCLIAdapter returns a CLIAdapter. fuseExe is the path to this binary
// (from os.Executable); approve is the HITL gate for the current turn.
func newCLIAdapter(fuseExe string, approve permissions.ApprovalFunc) *CLIAdapter {
	return &CLIAdapter{fuseExe: fuseExe, approve: approve}
}

// cliJSONResult mirrors the --output-format json response from the claude CLI.
type cliJSONResult struct {
	Type         string  `json:"type"`
	Result       string  `json:"result"`
	CostUSD      float64 `json:"cost_usd"`
	InputTokens  int     `json:"total_input_tokens"`
	OutputTokens int     `json:"total_output_tokens"`
}

// Complete sends the full conversation to claude --print and parses the response.
func (a *CLIAdapter) Complete(ctx context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	// Fresh HITL relay server per subprocess invocation. The socket is unique
	// across concurrent calls by combining PID and a per-adapter counter.
	socketPath := fmt.Sprintf("%s/fuse-hitl-%d-%d.sock", os.TempDir(), os.Getpid(), a.counter.Add(1))
	srv, err := hitl.NewServer(socketPath, a.approve)
	if err != nil {
		return model.CompletionResp{}, fmt.Errorf("cli adapter: hitl server: %w", err)
	}
	defer srv.Close()

	mcpPath, cleanMCP, err := writeMCPConfig(a.fuseExe)
	if err != nil {
		return model.CompletionResp{}, err
	}
	defer cleanMCP()

	prompt := flattenMessages(req.Messages)

	cmd := exec.CommandContext(ctx, "claude",
		"--print",
		"--output-format", "json",
		"--no-session-persistence",
		"--mcp-config", mcpPath,
		"--strict-mcp-config",
		prompt,
	)
	// FUSE_HITL_SOCKET is inherited by the claude subprocess, which passes it
	// to `fuse mcp-server` children so they can dial back to this relay.
	cmd.Env = append(os.Environ(), "FUSE_HITL_SOCKET="+socketPath)

	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return model.CompletionResp{}, fmt.Errorf("claude: %w\n%s", err, exitErr.Stderr)
		}
		return model.CompletionResp{}, fmt.Errorf("claude: %w", err)
	}

	var result cliJSONResult
	if err := json.Unmarshal(out, &result); err != nil {
		return model.CompletionResp{}, fmt.Errorf("cli adapter: parse: %w\nraw: %.200s", err, out)
	}
	return model.CompletionResp{
		Content:      result.Result,
		InputTokens:  result.InputTokens,
		OutputTokens: result.OutputTokens,
	}, nil
}

// writeMCPConfig writes a temporary MCP config JSON that tells Claude to spawn
// `fuse mcp-server` as a stdio MCP server and returns the path + cleanup func.
func writeMCPConfig(fuseExe string) (path string, cleanup func(), err error) {
	raw, err := json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			"fuse": map[string]any{
				"command": fuseExe,
				"args":    []string{"mcp-server"},
			},
		},
	})
	if err != nil {
		return "", nil, fmt.Errorf("cli adapter: marshal mcp config: %w", err)
	}
	f, err := os.CreateTemp("", "fuse-mcp-*.json")
	if err != nil {
		return "", nil, fmt.Errorf("cli adapter: create mcp config: %w", err)
	}
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", nil, fmt.Errorf("cli adapter: write mcp config: %w", err)
	}
	_ = f.Close()
	return f.Name(), func() { _ = os.Remove(f.Name()) }, nil
}

// flattenMessages converts a message history into a single prompt string for
// claude --print. Prior tool calls and results are rendered as structured XML
// so Claude can reconstruct the conversational context.
func flattenMessages(msgs []model.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case "system":
			fmt.Fprintf(&b, "<system>\n%s\n</system>\n\n", m.Content)
		case "user":
			fmt.Fprintf(&b, "<user>\n%s\n</user>\n\n", m.Content)
		case "assistant":
			if m.Content != "" {
				fmt.Fprintf(&b, "<assistant>\n%s\n</assistant>\n\n", m.Content)
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "<tool_call name=%q>\n%s\n</tool_call>\n\n", tc.Name, tc.Arguments)
			}
		case "tool":
			fmt.Fprintf(&b, "<tool_result name=%q>\n%s\n</tool_result>\n\n", m.Name, m.Content)
		}
	}
	b.WriteString("Continue as the assistant. Do not include any prefix like \"Assistant:\".")
	return strings.TrimSpace(b.String())
}

// compile-time Completer check
var _ interface {
	Complete(context.Context, model.CompletionReq) (model.CompletionResp, error)
} = (*CLIAdapter)(nil)
