package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/tools"
)

// streamingConn is a fake conn that, during a tools/call, emits two $/stream
// delta chunks (routed through the manager) using the call's progress token,
// then returns a standard envelope. It models a server that streams then
// completes.
type streamingConn struct {
	m    *Manager
	tool *MCPTool
}

func (c *streamingConn) call(_ context.Context, _ string, params any) (json.RawMessage, error) {
	// Extract the injected progress token.
	b, _ := json.Marshal(params)
	var pm struct {
		Meta struct {
			ProgressToken string `json:"progressToken"`
		} `json:"_meta"`
	}
	_ = json.Unmarshal(b, &pm)
	token := pm.Meta.ProgressToken

	for _, d := range []string{"chunk-1", "chunk-2"} {
		np, _ := json.Marshal(map[string]any{"progressToken": token, "delta": d})
		c.m.dispatchNotification("srv", "$/stream", np)
	}
	// Standard envelope with some placeholder content (which the stream buffer
	// overrides on return).
	return json.RawMessage(`{"content":[{"type":"text","text":"IGNORED"}]}`), nil
}
func (c *streamingConn) notify(context.Context, string, any) error { return nil }
func (c *streamingConn) stop()                                     {}

// TestStreamChunksAssembleIntoBuffer: ordered $/stream delta chunks assemble in
// order; on call end the concatenated complete buffer is returned.
func TestStreamChunksAssembleIntoBuffer(t *testing.T) {
	m, _ := NewManager(nil, tools.NewRegistry())
	defer m.Close()

	token, end := m.beginCall("srv", "streamer")

	for _, d := range []string{"Hello, ", "streaming ", "world"} {
		params, _ := json.Marshal(map[string]any{"progressToken": token, "delta": d})
		m.dispatchNotification("srv", "$/stream", params)
	}

	got := end()
	if got != "Hello, streaming world" {
		t.Errorf("assembled = %q, want %q", got, "Hello, streaming world")
	}
}

// TestStreamRollingPartialSanitized: the rolling-partial accessor returns the
// latest content sanitized for a fixed-width TUI (control bytes stripped).
func TestStreamRollingPartialSanitized(t *testing.T) {
	m, _ := NewManager(nil, tools.NewRegistry())
	defer m.Close()

	token, end := m.beginCall("srv", "streamer")
	defer end()

	// A chunk carrying an ESC sequence and a carriage return.
	params, _ := json.Marshal(map[string]any{"progressToken": token, "delta": "a\x1b[31mb\rc"})
	m.dispatchNotification("srv", "$/stream", params)

	partial := m.StreamPartial(token)
	if strings.ContainsAny(partial, "\x1b\r") {
		t.Errorf("partial %q still carries control bytes", partial)
	}
	if !strings.Contains(partial, "a") || !strings.Contains(partial, "b") || !strings.Contains(partial, "c") {
		t.Errorf("partial %q lost visible characters", partial)
	}
}

// TestStreamRingOverflowKeepsHeadAndTail: when total delta exceeds the ring cap,
// the assembled buffer keeps the head and the tail with a truncation marker in
// between (consistent with internal/tools/spill.go).
func TestStreamRingOverflowKeepsHeadAndTail(t *testing.T) {
	m, _ := NewManager(nil, tools.NewRegistry())
	defer m.Close()

	token, end := m.beginCall("srv", "big")

	head := strings.Repeat("H", streamRingKeep)
	middle := strings.Repeat("M", streamRingKeep*4)
	tail := strings.Repeat("T", streamRingKeep)
	for _, d := range []string{head, middle, tail} {
		params, _ := json.Marshal(map[string]any{"progressToken": token, "delta": d})
		m.dispatchNotification("srv", "$/stream", params)
	}

	got := end()
	if !strings.HasPrefix(got, "H") {
		t.Errorf("assembled must start with the head bytes: %.20q", got)
	}
	if !strings.HasSuffix(got, "T") {
		t.Errorf("assembled must end with the tail bytes: ...%q", got[len(got)-20:])
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("assembled must carry a truncation marker, got %.80q...", got)
	}
	// The elided middle must NOT be fully present.
	if strings.Contains(got, middle) {
		t.Error("assembled must not contain the full elided middle")
	}
}

// TestExecuteReturnsStreamBufferAsResult: when $/stream chunks arrive for a
// streaming tools/call, Execute delivers the concatenated buffer as the result
// value (the loop contract: complete result, not partials).
func TestExecuteReturnsStreamBufferAsResult(t *testing.T) {
	m, _ := NewManager(nil, tools.NewRegistry())
	defer m.Close()

	// A conn that emits two $/stream chunks (via the manager) during the call,
	// then returns the standard envelope. The tool is wired to this manager.
	conn := &streamingConn{m: m}
	tool := &MCPTool{
		client:            conn,
		serverName:        "srv",
		toolName:          "streamer",
		supportsStreaming: true,
		tracker:           m,
	}
	conn.tool = tool

	res := tool.Execute(nil, `{}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if res.Output != "chunk-1chunk-2" {
		t.Errorf("output = %q, want the concatenated stream buffer %q", res.Output, "chunk-1chunk-2")
	}
}
