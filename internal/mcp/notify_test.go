package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// TestStdioNotifyFrameHasNoID verifies StdioClient.notify writes a JSON-RPC
// notification frame with no "id" key and does not block for a response.
func TestStdioNotifyFrameHasNoID(t *testing.T) {
	var buf bytes.Buffer
	c := &StdioClient{
		name:    "t",
		enc:     json.NewEncoder(&buf),
		pending: map[string]chan jsonrpcResponse{},
		done:    make(chan struct{}),
	}
	if err := c.notify(context.Background(), "notifications/initialized", nil); err != nil {
		t.Fatalf("notify: %v", err)
	}
	var frame map[string]any
	if err := json.Unmarshal(buf.Bytes(), &frame); err != nil {
		t.Fatalf("unmarshal frame %q: %v", buf.String(), err)
	}
	if _, ok := frame["id"]; ok {
		t.Errorf("notification frame must not carry an id: %s", buf.String())
	}
	if frame["method"] != "notifications/initialized" {
		t.Errorf("method = %v, want notifications/initialized", frame["method"])
	}
}

// TestStdioNotifyAfterCloseErrors verifies notify fails once the conn is closed.
func TestStdioNotifyAfterCloseErrors(t *testing.T) {
	var buf bytes.Buffer
	c := &StdioClient{name: "t", enc: json.NewEncoder(&buf), pending: map[string]chan jsonrpcResponse{}, done: make(chan struct{})}
	close(c.done)
	if err := c.notify(context.Background(), "x", nil); err == nil {
		t.Error("expected error notifying a closed conn")
	}
}

// TestHTTPNotifyPostsIDLessFrame verifies httpClient.notify POSTs a frame with
// no "id" key to the messages URL.
func TestHTTPNotifyPostsIDLessFrame(t *testing.T) {
	var (
		mu   sync.Mutex
		body []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = b
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := &httpClient{
		name:        "t",
		http:        srv.Client(),
		messagesURL: srv.URL,
		pending:     map[string]chan jsonrpcResponse{},
		done:        make(chan struct{}),
	}
	if err := c.notify(context.Background(), "notifications/initialized", nil); err != nil {
		t.Fatalf("notify: %v", err)
	}
	mu.Lock()
	got := body
	mu.Unlock()
	var frame map[string]any
	if err := json.Unmarshal(got, &frame); err != nil {
		t.Fatalf("unmarshal posted body %q: %v", string(got), err)
	}
	if _, ok := frame["id"]; ok {
		t.Errorf("posted notification must not carry an id: %s", string(got))
	}
	if frame["method"] != "notifications/initialized" {
		t.Errorf("method = %v, want notifications/initialized", frame["method"])
	}
}
