package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
)

// recordConn is an mcpConn that records the ordered sequence of call/notify
// methods and returns scripted per-method results, for testing the handshake.
type recordConn struct {
	seq     []string
	results map[string]json.RawMessage
	errs    map[string]error
	stopped bool
}

func newRecordConn() *recordConn {
	return &recordConn{results: map[string]json.RawMessage{}, errs: map[string]error{}}
}

func (c *recordConn) call(_ context.Context, method string, _ any) (json.RawMessage, error) {
	c.seq = append(c.seq, "call:"+method)
	if err := c.errs[method]; err != nil {
		return nil, err
	}
	return c.results[method], nil
}
func (c *recordConn) notify(_ context.Context, method string, _ any) error {
	c.seq = append(c.seq, "notify:"+method)
	return c.errs[method]
}
func (c *recordConn) stop() { c.stopped = true }

func TestHandshakeOrdering(t *testing.T) {
	c := newRecordConn()
	c.results["initialize"] = json.RawMessage(`{"protocolVersion":"2025-03-26","capabilities":{"resources":{"subscribe":true}}}`)
	c.results["tools/list"] = json.RawMessage(`{"tools":[{"name":"alpha","description":"a"}]}`)

	tools, caps, ver, err := handshakeAndDiscover(context.Background(), c, "srv")
	if err != nil {
		t.Fatalf("handshakeAndDiscover: %v", err)
	}

	want := []string{"call:initialize", "notify:notifications/initialized", "call:tools/list"}
	if fmt.Sprint(c.seq) != fmt.Sprint(want) {
		t.Errorf("method sequence = %v, want %v", c.seq, want)
	}
	if ver != "2025-03-26" {
		t.Errorf("protoVer = %q, want 2025-03-26", ver)
	}
	if !caps.Supports("resources.subscribe") {
		t.Error("expected resources.subscribe negotiated")
	}
	if len(tools) != 1 || tools[0].toolName != "alpha" {
		t.Errorf("tools = %+v, want one 'alpha'", tools)
	}
}

func TestHandshakeInitErrorHardFails(t *testing.T) {
	c := newRecordConn()
	c.errs["initialize"] = fmt.Errorf("boom")
	c.results["tools/list"] = json.RawMessage(`{"tools":[{"name":"alpha"}]}`)

	_, _, _, err := handshakeAndDiscover(context.Background(), c, "srv")
	if err == nil {
		t.Fatal("expected initialize error to hard-fail the handshake")
	}
	// tools/list must never be reached after a failed initialize.
	for _, s := range c.seq {
		if s == "call:tools/list" {
			t.Error("tools/list must not be called after initialize fails")
		}
	}
}

func TestHandshakeInitializedNotifyErrorIsNonFatal(t *testing.T) {
	c := newRecordConn()
	c.results["initialize"] = json.RawMessage(`{"protocolVersion":"2025-03-26","capabilities":{}}`)
	c.errs["notifications/initialized"] = fmt.Errorf("notify failed")
	c.results["tools/list"] = json.RawMessage(`{"tools":[]}`)

	if _, _, _, err := handshakeAndDiscover(context.Background(), c, "srv"); err != nil {
		t.Fatalf("a failed initialized notification must not fail the handshake: %v", err)
	}
}

func TestManagerStatusSurfacesCapabilities(t *testing.T) {
	caps, ver := parseInitializeResult(json.RawMessage(`{"protocolVersion":"2025-03-26","capabilities":{"resources":{"subscribe":true},"logging":{}}}`))
	m := &Manager{servers: map[string]*managedServer{
		"srv": {cfg: config.MCPServerConfig{Name: "srv"}, conn: newRecordConn(), caps: caps, protoVer: ver},
	}}
	st := m.Status()
	if len(st) != 1 {
		t.Fatalf("Status() len = %d, want 1", len(st))
	}
	if st[0].ProtocolVersion != "2025-03-26" {
		t.Errorf("ProtocolVersion = %q, want 2025-03-26", st[0].ProtocolVersion)
	}
	if !contains(st[0].Capabilities, "resources.subscribe") || !contains(st[0].Capabilities, "logging") {
		t.Errorf("Capabilities = %v, want to include resources.subscribe and logging", st[0].Capabilities)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
