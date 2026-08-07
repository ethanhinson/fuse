package tui

// End-to-end TUI test: a real ShellModel, driven through a live bubbletea
// program (teatest), runs an agent turn whose model (scripted completer) calls
// two MCP tools. The tools are backed by a REAL mcp.Manager talking to a REAL
// MCP server subprocess (this test binary re-execed as a server), so the whole
// client path — MCPTool.Execute → client.call → RPCError → surfaced code — runs
// inside the actual TUI. Proves both success and error-code surfacing reach the
// agent through the live program, which the PTY harness could not drive.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/mcp"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/tools"
)

// regExec adapts a tools.Registry to agent.ToolExecutor, routing Execute through
// the registry exactly as a real session does (→ MCPTool.Execute → client.call).
type regExec struct{ reg *tools.Registry }

func (e regExec) Schemas() []model.ToolSchema { return e.reg.Schemas() }
func (e regExec) Execute(ctx context.Context, name, args string) tools.Result {
	return e.reg.Execute(ctx, name, args)
}

func TestTUI_MCPToolRoundTrip(t *testing.T) {
	reg := tools.NewRegistry()
	mgr, err := mcp.NewManager([]config.MCPServerConfig{{
		Name:      "mock",
		Transport: "stdio",
		Command:   []string{os.Args[0], "-test.run=TestTUIMockMCPHelper"},
		Env:       map[string]string{"FUSE_TUI_MOCK_MCP": "1"},
	}}, reg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()
	if !reg.Has("mcp:mock/ok_echo") || !reg.Has("mcp:mock/explode") {
		t.Fatalf("mock MCP tools not registered (discovery failed)")
	}

	// Turn 1: the model calls both MCP tools. Turn 2: it replies and stops.
	cmp := &scriptedCompleter{responses: []model.CompletionResp{
		{ToolCalls: []model.ToolCall{
			{ID: "1", Name: "mcp:mock/ok_echo", Arguments: `{"text":"hello world"}`},
			{ID: "2", Name: "mcp:mock/explode", Arguments: `{}`},
		}},
		{Content: "mcp-turn-complete"},
	}}

	exec := regExec{reg: reg}
	build := func(_ string, r agent.Renderer, _ permissions.ApprovalFunc) (*agent.Agent, error) {
		return agent.New(cmp, exec, r, "test/model", "", 25, 0), nil
	}
	m := NewShellModel("alpha", false, "", testRegistry(), nil, build, permissions.NewSessionMode(permissions.ModeSmart), true)

	bridgeCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(80, 24))
	StartBridges(bridgeCtx, tm.GetProgram(), m.Channel(), nil, nil)
	t.Cleanup(func() { tm.Quit() })

	for _, r := range "use the mock tools" {
		tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})

	// The live program must complete the two-turn loop.
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(stripANSI(b), []byte("mcp-turn-complete"))
	}, teatest.WithDuration(15*time.Second), teatest.WithCheckInterval(20*time.Millisecond))

	// The agent's SECOND request carries the tool results fed back after turn 1
	// — success text and the surfaced MCP error code — proving the real
	// client→server round trip ran inside the live TUI.
	if len(cmp.requests) < 2 {
		t.Fatalf("expected >=2 completer requests (tool results never fed back), got %d", len(cmp.requests))
	}
	sent := renderRequest(cmp.requests[1])
	if !strings.Contains(sent, "echoed: hello world") {
		t.Errorf("SUCCESS tool result missing from agent request:\n%s", sent)
	}
	if !strings.Contains(sent, "-32901") || !strings.Contains(sent, "resource gone") {
		t.Errorf("surfaced MCP error code missing from agent request:\n%s", sent)
	}
}

// TestTUIMockMCPHelper is not a real test: re-execed with FUSE_TUI_MOCK_MCP=1 it
// becomes a minimal stdio MCP server, then exits. Under a normal run it no-ops.
func TestTUIMockMCPHelper(t *testing.T) {
	if os.Getenv("FUSE_TUI_MOCK_MCP") != "1" {
		return
	}
	dec := json.NewDecoder(bufio.NewReader(os.Stdin))
	enc := json.NewEncoder(os.Stdout)
	send := func(id json.RawMessage, result any, rpcErr map[string]any) {
		msg := map[string]any{"jsonrpc": "2.0", "id": id}
		if rpcErr != nil {
			msg["error"] = rpcErr
		} else {
			msg["result"] = result
		}
		_ = enc.Encode(msg)
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
			os.Exit(0)
		}
		if len(req.ID) == 0 {
			continue
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
				{"name": "ok_echo", "description": "echo the text argument", "inputSchema": map[string]any{"type": "object"}},
				{"name": "explode", "description": "fails with an MCP error code", "inputSchema": map[string]any{"type": "object"}},
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
				send(req.ID, nil, map[string]any{"code": mcp.ErrResourceNotFound, "message": "resource gone (mock)"})
			default:
				send(req.ID, nil, map[string]any{"code": mcp.ErrToolNotFound, "message": "tool not found: " + req.Params.Name})
			}
		default:
			send(req.ID, nil, map[string]any{"code": mcp.ErrMethodNotFound, "message": fmt.Sprintf("method not found: %s", req.Method)})
		}
	}
}
