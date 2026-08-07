package mcp

import (
	"errors"
	"fmt"
	"testing"
)

// TestRPCErrorPreservesCode verifies that a client call path wrapping an
// *RPCError with %w keeps the numeric code recoverable via errors.As, and that
// the flattened message (via Error()) is unchanged from the pre-typed-error
// format so existing message-substring behavior holds.
func TestRPCErrorPreservesCode(t *testing.T) {
	rpc := &RPCError{Code: ErrToolNotFound, Message: "tool not found: nope"}
	wrapped := fmt.Errorf("mcp %q: %w", "srv", rpc)

	var got *RPCError
	if !errors.As(wrapped, &got) {
		t.Fatal("errors.As failed to recover *RPCError from wrapped error")
	}
	if got.Code != ErrToolNotFound {
		t.Errorf("recovered code = %d, want %d", got.Code, ErrToolNotFound)
	}
	if !got.IsMCPError() {
		t.Errorf("IsMCPError() = false for code %d, want true", got.Code)
	}
	if want := `mcp "srv": tool not found: nope`; wrapped.Error() != want {
		t.Errorf("wrapped.Error() = %q, want %q", wrapped.Error(), want)
	}
}

func TestRPCErrorStandardCodeNotMCP(t *testing.T) {
	rpc := &RPCError{Code: -32601, Message: "method not found"}
	if rpc.IsMCPError() {
		t.Error("a standard JSON-RPC code (-32601) must not report as an MCP error")
	}
	if rpc.Error() != "method not found" {
		t.Errorf("Error() = %q, want %q", rpc.Error(), "method not found")
	}
}
