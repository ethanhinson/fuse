package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// fakeConn is an mcpConn whose call() returns a scripted result or error,
// letting MCPTool.Execute be tested without a real server process.
type fakeConn struct {
	raw json.RawMessage
	err error
}

func (f *fakeConn) call(context.Context, string, any) (json.RawMessage, error) {
	return f.raw, f.err
}
func (f *fakeConn) stop() {}

// TestMCPToolSurfacesErrorCode: when the server returns a JSON-RPC error, the
// wrapped *RPCError's code must appear in the tool result the model sees, not
// just the message.
func TestMCPToolSurfacesErrorCode(t *testing.T) {
	wrapped := fmt.Errorf("mcp %q: %w", "mock", &RPCError{Code: ErrResourceNotFound, Message: "resource gone"})
	tool := &MCPTool{client: &fakeConn{err: wrapped}, serverName: "mock", toolName: "explode"}

	res := tool.Execute(context.Background(), `{}`)
	if !res.IsError {
		t.Fatal("expected an error result")
	}
	if !strings.Contains(res.Output, "-32901") {
		t.Errorf("output %q should surface the numeric code -32901", res.Output)
	}
	if !strings.Contains(res.Output, "resource gone") {
		t.Errorf("output %q should still contain the message", res.Output)
	}
}

// TestMCPToolSuccessPath: a normal content result is unwrapped to text.
func TestMCPToolSuccessPath(t *testing.T) {
	raw := json.RawMessage(`{"content":[{"type":"text","text":"hello"}],"isError":false}`)
	tool := &MCPTool{client: &fakeConn{raw: raw}, serverName: "mock", toolName: "ok"}

	res := tool.Execute(context.Background(), `{}`)
	if res.IsError {
		t.Fatalf("expected success, got error: %s", res.Output)
	}
	if res.Output != "hello" {
		t.Errorf("output = %q, want %q", res.Output, "hello")
	}
}
