package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/tools"
)

// TestMCPClientPathAgainstRealServer drives the REAL client path — Manager →
// registered MCPTool → client.call → RPCError → surfaced code — against a real
// MCP server subprocess, through the same permission gate + registry the agent
// loop uses (the wiring cmd/fuse/shell.go builds via tui.NewMCPProvider).
//
// The "server" is this test binary re-executed with FUSE_MOCK_MCP=1 (Go's
// helper-process idiom), so the test is hermetic — no external script, no docker.
func TestMCPClientPathAgainstRealServer(t *testing.T) {
	reg := tools.NewRegistry()
	mgr, err := NewManager([]config.MCPServerConfig{{
		Name:      "mock",
		Transport: "stdio",
		Command:   []string{os.Args[0], "-test.run=TestMockMCPServerHelper"},
		Env:       map[string]string{"FUSE_MOCK_MCP": "1"},
	}}, reg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	// Discovery: the client's tools/list must have succeeded and registered both.
	if !reg.Has("mcp:mock/ok_echo") || !reg.Has("mcp:mock/explode") {
		var got []string
		for _, s := range reg.Schemas() {
			got = append(got, s.Name)
		}
		t.Fatalf("expected mock tools registered, got: %v", got)
	}

	// Drive tool calls through the SAME gate the agent uses (mode off = auto).
	gate := permissions.New(config.PermissionsConfig{Mode: "off"}, reg, permissions.AlwaysApprove)
	ctx := context.Background()

	// Success path.
	ok := gate.Execute(ctx, "mcp:mock/ok_echo", `{"text":"hello world"}`)
	if ok.IsError {
		t.Errorf("ok_echo returned error: %s", ok.Output)
	}
	if !strings.Contains(ok.Output, "echoed: hello world") {
		t.Errorf("ok_echo output = %q, want it to contain %q", ok.Output, "echoed: hello world")
	}

	// Error path: server returns JSON-RPC error code -32901; the client must
	// preserve it and MCPTool.Execute must surface the numeric code.
	boom := gate.Execute(ctx, "mcp:mock/explode", `{}`)
	if !boom.IsError {
		t.Errorf("explode should have returned an error result")
	}
	if !strings.Contains(boom.Output, "-32901") {
		t.Errorf("explode output %q must surface the error code -32901", boom.Output)
	}
	if !strings.Contains(boom.Output, "resource gone") {
		t.Errorf("explode output %q must retain the server message", boom.Output)
	}
}

// TestMockMCPServerHelper is not a real test: when the process is re-executed
// with FUSE_MOCK_MCP=1 it becomes a minimal MCP server speaking JSON-RPC 2.0 on
// stdio, then exits. Under a normal `go test` run (env unset) it returns
// immediately.
func TestMockMCPServerHelper(t *testing.T) {
	if os.Getenv("FUSE_MOCK_MCP") != "1" {
		return
	}
	runMockMCPStdioServer(os.Stdin, os.Stdout)
	os.Exit(0)
}

func runMockMCPStdioServer(rd *os.File, wr *os.File) {
	dec := json.NewDecoder(bufio.NewReader(rd))
	enc := json.NewEncoder(wr)
	send := func(id json.RawMessage, result any, rpcErr map[string]any) {
		m := map[string]any{"jsonrpc": "2.0", "id": id}
		if rpcErr != nil {
			m["error"] = rpcErr
		} else {
			m["result"] = result
		}
		_ = enc.Encode(m)
	}
	for {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		if err := dec.Decode(&req); err != nil {
			return
		}
		if len(req.ID) == 0 {
			continue // notification
		}
		switch req.Method {
		case "initialize":
			send(req.ID, map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "mock", "version": "0.1.0"},
			}, nil)
		case "tools/list":
			send(req.ID, map[string]any{"tools": []map[string]any{
				{"name": "ok_echo", "description": "echo the text argument",
					"inputSchema": map[string]any{"type": "object"}},
				{"name": "explode", "description": "always fails with an MCP error code",
					"inputSchema": map[string]any{"type": "object"}},
			}}, nil)
		case "tools/call":
			switch req.Params.Name {
			case "ok_echo":
				var a struct {
					Text string `json:"text"`
				}
				_ = json.Unmarshal(req.Params.Arguments, &a)
				send(req.ID, map[string]any{
					"content": []map[string]any{{"type": "text", "text": "echoed: " + a.Text}},
					"isError": false,
				}, nil)
			case "explode":
				send(req.ID, nil, map[string]any{"code": ErrResourceNotFound, "message": "resource gone (mock)"})
			default:
				send(req.ID, nil, map[string]any{"code": ErrToolNotFound, "message": "tool not found: " + req.Params.Name})
			}
		default:
			send(req.ID, nil, map[string]any{"code": -32601, "message": fmt.Sprintf("method not found: %s", req.Method)})
		}
	}
}
