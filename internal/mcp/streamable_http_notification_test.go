package mcp

import (
	"encoding/json"
	"testing"
)

// TestStreamableDispatchFrameRoutesIDlessNotification verifies the streamable
// transport's server-frame seam routes id-less $/progress notifications to the
// registered router instead of dropping them, and that a matching id-keyed
// response is still returned to the caller (matched=true) and NOT routed.
func TestStreamableDispatchFrameRoutesIDlessNotification(t *testing.T) {
	router := &recordingRouter{}
	c := &StreamableHTTPClient{name: "stream-srv", router: router}

	// Id-less notification frame — routed, not matched.
	note := `{"jsonrpc":"2.0","method":"$/progress","params":{"progressToken":"tokS","progress":0.75}}`
	if _, _, matched := c.dispatchFrame(note, "1"); matched {
		t.Fatal("id-less notification must not match a pending id")
	}
	if router.count() != 1 {
		t.Fatalf("router dispatch count = %d, want 1", router.count())
	}
	router.mu.Lock()
	if router.server[0] != "stream-srv" || router.method[0] != "$/progress" {
		t.Errorf("routed (%q,%q), want (stream-srv,$/progress)", router.server[0], router.method[0])
	}
	router.mu.Unlock()

	// A matching response frame is returned to the caller, not routed.
	resp := `{"jsonrpc":"2.0","id":"1","result":{"ok":true}}`
	result, rpcErr, matched := c.dispatchFrame(resp, "1")
	if !matched {
		t.Fatal("response with matching id must match")
	}
	if rpcErr != nil {
		t.Fatalf("unexpected rpc error: %v", rpcErr)
	}
	var r map[string]bool
	if err := json.Unmarshal(result, &r); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if !r["ok"] {
		t.Errorf("result = %v, want ok=true", r)
	}
	if router.count() != 1 {
		t.Errorf("response frame must not be routed; count = %d, want 1", router.count())
	}
}
