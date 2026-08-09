package mcp

import (
	"context"
	"encoding/json"
	"testing"
)

// capturingConn records the params of the last tools/call, so a test can assert
// exactly what MCPTool.Execute sent on the wire.
type capturingConn struct {
	raw        json.RawMessage
	lastMethod string
	lastParams any
}

func (c *capturingConn) call(_ context.Context, method string, params any) (json.RawMessage, error) {
	c.lastMethod = method
	c.lastParams = params
	if c.raw == nil {
		return json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`), nil
	}
	return c.raw, nil
}
func (c *capturingConn) notify(context.Context, string, any) error { return nil }
func (c *capturingConn) stop()                                     {}

// paramsMap marshals the recorded params and unmarshals into a generic map for
// structural assertions.
func paramsMap(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	return m
}

// fakeTracker is a callTracker double: it mints a fixed token and records
// begin/end calls so the test can assert lifecycle.
type fakeTracker struct {
	token   string
	begins  int
	ends    int
	drains  int
	server  string
	toolNm  string
}

func (f *fakeTracker) beginCall(server, tool string) (string, func() string) {
	f.begins++
	f.server = server
	f.toolNm = tool
	return f.token, func() string { f.ends++; return "" }
}

func (f *fakeTracker) drainNotifications() { f.drains++ }

// TestExecuteInjectsProgressTokenWhenStreaming: with streaming supported, the
// tools/call params carry _meta.progressToken == the minted token, and the
// tracker's begin/end lifecycle is observed.
func TestExecuteInjectsProgressTokenWhenStreaming(t *testing.T) {
	conn := &capturingConn{}
	tr := &fakeTracker{token: "tok-123"}
	tool := &MCPTool{
		client:            conn,
		serverName:        "srv",
		toolName:          "do",
		supportsStreaming: true,
		tracker:           tr,
	}

	res := tool.Execute(context.Background(), `{"a":1}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}

	if conn.lastMethod != "tools/call" {
		t.Fatalf("method = %q, want tools/call", conn.lastMethod)
	}
	m := paramsMap(t, conn.lastParams)
	meta, ok := m["_meta"].(map[string]any)
	if !ok {
		t.Fatalf("_meta missing or wrong type: %v", m["_meta"])
	}
	if meta["progressToken"] != "tok-123" {
		t.Errorf("progressToken = %v, want tok-123", meta["progressToken"])
	}
	// arguments preserved
	args, ok := m["arguments"].(map[string]any)
	if !ok || args["a"] != float64(1) {
		t.Errorf("arguments not preserved: %v", m["arguments"])
	}
	if tr.begins != 1 || tr.ends != 1 {
		t.Errorf("tracker lifecycle begins=%d ends=%d, want 1/1", tr.begins, tr.ends)
	}
	if tr.server != "srv" || tr.toolNm != "do" {
		t.Errorf("tracker got (%q,%q), want (srv,do)", tr.server, tr.toolNm)
	}
}

// TestExecuteNoMetaWhenNotStreaming is the blocking-behavior golden: without
// streaming, the params are byte-identical to the pre-#0020 path — NO _meta key,
// and the tracker is never touched.
func TestExecuteNoMetaWhenNotStreaming(t *testing.T) {
	conn := &capturingConn{}
	tr := &fakeTracker{token: "should-not-appear"}
	tool := &MCPTool{
		client:            conn,
		serverName:        "srv",
		toolName:          "do",
		supportsStreaming: false,
		tracker:           tr,
	}

	if res := tool.Execute(context.Background(), `{"a":1}`); res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}

	m := paramsMap(t, conn.lastParams)
	if _, present := m["_meta"]; present {
		t.Errorf("non-streaming call must NOT carry _meta: %v", m)
	}
	// The golden params: exactly name + arguments.
	if len(m) != 2 {
		t.Errorf("params keys = %v, want exactly name+arguments", m)
	}
	if m["name"] != "do" {
		t.Errorf("name = %v, want do", m["name"])
	}
	if tr.begins != 0 || tr.ends != 0 {
		t.Errorf("tracker must not be used when not streaming: begins=%d ends=%d", tr.begins, tr.ends)
	}
}
