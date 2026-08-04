package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ethanhinson/fuse/internal/tools"
)

// mcpToolDef is the shape of a single tool entry in the tools/list response.
type mcpToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// MCPTool wraps a single MCP server tool as a tools.Tool.
type MCPTool struct {
	client      *StdioClient
	serverName  string
	toolName    string
	description string
	inputSchema map[string]any
}

func (t *MCPTool) Name() string              { return "mcp:" + t.serverName + "/" + t.toolName }
func (t *MCPTool) Description() string       { return t.description }
func (t *MCPTool) Parameters() map[string]any { return t.inputSchema }

// Execute calls tools/call on the MCP server and wraps the result.
func (t *MCPTool) Execute(ctx context.Context, args string) tools.Result {
	var params any
	if err := json.Unmarshal([]byte(args), &params); err != nil {
		return tools.Result{IsError: true, Output: fmt.Sprintf("bad arguments: %v", err)}
	}

	raw, err := t.client.call(ctx, "tools/call", map[string]any{
		"name":      t.toolName,
		"arguments": params,
	})
	if err != nil {
		return tools.Result{IsError: true, Output: fmt.Sprintf("mcp %s/%s: %v", t.serverName, t.toolName, err)}
	}

	// MCP tools/call response: {"content": [{type, text}...], "isError": bool}
	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return tools.Result{Output: string(raw)}
	}

	var out string
	for _, c := range resp.Content {
		if c.Type == "text" {
			out += c.Text
		}
	}
	return tools.Result{Output: out, IsError: resp.IsError}
}

// compile-time interface check
var _ tools.Tool = (*MCPTool)(nil)
