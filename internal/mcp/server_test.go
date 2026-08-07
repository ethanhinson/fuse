package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/tools"
)

// newTestServer builds a Server whose gate auto-approves (mode "off"), so a
// tools/call for a registered tool runs through to a normal result.
func newTestServer(reg *tools.Registry) *Server {
	approve := func(context.Context, permissions.ApprovalRequest) (bool, bool, error) {
		return true, false, nil
	}
	gate := permissions.New(config.PermissionsConfig{Mode: "off"}, reg, approve)
	return &Server{reg: reg, gate: gate}
}

func callReq(t *testing.T, name string) serverReq {
	t.Helper()
	params, err := json.Marshal(callParams{Name: name, Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	return serverReq{JSONRPC: "2.0", ID: json.RawMessage(`"1"`), Method: "tools/call", Params: params}
}

func TestHandleCallUnknownToolReturnsToolNotFound(t *testing.T) {
	s := newTestServer(tools.NewRegistry()) // empty registry
	resp := s.dispatch(context.Background(), callReq(t, "nope"))
	if resp.Error == nil {
		t.Fatalf("expected an error frame, got result: %s", resp.Result)
	}
	if resp.Error.Code != ErrToolNotFound {
		t.Errorf("error code = %d, want %d (ErrToolNotFound)", resp.Error.Code, ErrToolNotFound)
	}
	if !strings.Contains(resp.Error.Message, "nope") {
		t.Errorf("error message %q should name the missing tool", resp.Error.Message)
	}
}

func TestHandleCallKnownToolStillReturnsResult(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(echoTool{})
	s := newTestServer(reg)
	resp := s.dispatch(context.Background(), callReq(t, "echo"))
	if resp.Error != nil {
		t.Fatalf("registered tool should not error: code=%d msg=%q", resp.Error.Code, resp.Error.Message)
	}
	if resp.Result == nil {
		t.Fatal("expected a result frame for a registered tool")
	}
}

func TestHandleCallDisabledToolIsNotToolNotFound(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(echoTool{})
	approve := func(context.Context, permissions.ApprovalRequest) (bool, bool, error) {
		return true, false, nil
	}
	// Tool is registered but disabled — the gate returns an isError result, and
	// the server must NOT reclassify that as -32900 (not-found).
	gate := permissions.New(config.PermissionsConfig{Mode: "smart", Disabled: []string{"echo"}}, reg, approve)
	s := &Server{reg: reg, gate: gate}
	resp := s.dispatch(context.Background(), callReq(t, "echo"))
	if resp.Error != nil {
		t.Fatalf("disabled tool should return an isError result, not a protocol error (code=%d)", resp.Error.Code)
	}
	var payload struct {
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(resp.Result, &payload); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if !payload.IsError {
		t.Error("disabled tool result should carry isError=true")
	}
}

// echoTool is a minimal registered tool for server tests.
type echoTool struct{}

func (echoTool) Name() string                                 { return "echo" }
func (echoTool) Description() string                          { return "echo" }
func (echoTool) Parameters() map[string]any                   { return map[string]any{"type": "object"} }
func (echoTool) Execute(context.Context, string) tools.Result { return tools.Result{Output: "ok"} }
