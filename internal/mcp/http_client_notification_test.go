package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestSSEPumpRoutesIDlessNotification pushes an id-less $/progress frame over the
// SSE stream and asserts it reaches the client's notification router; a normal
// id-keyed response still resolves its pending call.
func TestSSEPumpRoutesIDlessNotification(t *testing.T) {
	srv := newSSETestServerFull()
	defer srv.Close()

	router := &recordingRouter{}

	// A "slow" handler: it first broadcasts an id-less $/progress notification,
	// then responds normally. Because the SSE pump is a single goroutine, the
	// notification is observed before the response resolves the call.
	srv.addHandler("work", func(id string, params json.RawMessage) jsonrpcResponse {
		note := jsonrpcNotification{JSONRPC: "2.0", Method: "$/progress", Params: map[string]any{
			"progressToken": "tokX", "progress": 0.25,
		}}
		b, _ := json.Marshal(note)
		srv.broadcast(string(b))
		result, _ := json.Marshal(map[string]string{"done": "yes"})
		return jsonrpcResponse{JSONRPC: "2.0", ID: id, Result: result}
	})

	client, err := newHTTPClient("http-srv", srv.srv.URL, "")
	if err != nil {
		t.Fatalf("newHTTPClient: %v", err)
	}
	client.router = router
	defer client.stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	raw, err := client.call(ctx, "work", nil)
	if err != nil {
		t.Fatalf("call work: %v", err)
	}
	var res map[string]string
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if res["done"] != "yes" {
		t.Errorf("call result = %v, want done=yes", res)
	}

	// The notification should have been routed.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && router.count() == 0 {
		time.Sleep(2 * time.Millisecond)
	}
	if router.count() != 1 {
		t.Fatalf("router dispatch count = %d, want 1", router.count())
	}
	router.mu.Lock()
	defer router.mu.Unlock()
	if router.server[0] != "http-srv" {
		t.Errorf("server = %q, want http-srv", router.server[0])
	}
	if router.method[0] != "$/progress" {
		t.Errorf("method = %q, want $/progress", router.method[0])
	}
	var p struct {
		ProgressToken string  `json:"progressToken"`
		Progress      float64 `json:"progress"`
	}
	if err := json.Unmarshal(router.params[0], &p); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if p.ProgressToken != "tokX" || p.Progress != 0.25 {
		t.Errorf("params = %+v, want tokX/0.25", p)
	}
}
