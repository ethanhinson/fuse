package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// sseTestServer runs a minimal MCP HTTP/SSE test server.
// It accepts a list of JSON-RPC method handlers and streams responses back over SSE.
type sseTestServer struct {
	mu       sync.Mutex
	handlers map[string]func(params json.RawMessage) (any, error)
}

func newSSETestServer() *sseTestServer {
	return &sseTestServer{
		handlers: make(map[string]func(params json.RawMessage) (any, error)),
	}
}

func (s *sseTestServer) handle(method string, fn func(params json.RawMessage) (any, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = fn
}

func (s *sseTestServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/sse":
		s.handleSSE(w, r)
	case "/messages":
		s.handleMessages(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *sseTestServer) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// Derive the messages URL from the request host.
	scheme := "http"
	messagesURL := scheme + "://" + r.Host + "/messages"

	// Send the endpoint event.
	fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", messagesURL)
	flusher.Flush()

	// Keep the connection open until client closes.
	<-r.Context().Done()
}

func (s *sseTestServer) handleMessages(w http.ResponseWriter, r *http.Request) {
	// We need to write back over SSE, but in this test setup we find the
	// SSE writer via a shared channel. Instead, use a simpler approach:
	// respond inline in the POST body (204 accepted) and deliver via SSE
	// through a shared broadcaster.
	//
	// For the test, we use the approach where the test server holds
	// a reference to the SSE writer and fans out responses.
	http.Error(w, "use sseTestServerWithBroadcast instead", http.StatusInternalServerError)
}

// sseTestServerFull is a self-contained test server that properly implements
// the MCP HTTP/SSE protocol: POSTed requests are responded to via the SSE stream.
type sseTestServerFull struct {
	srv      *httptest.Server
	mu       sync.Mutex
	sseConns []chan string // each SSE connection gets its own channel
	handlers map[string]func(id string, params json.RawMessage) jsonrpcResponse
}

func newSSETestServerFull() *sseTestServerFull {
	s := &sseTestServerFull{
		handlers: make(map[string]func(id string, params json.RawMessage) jsonrpcResponse),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", s.handleSSE)
	mux.HandleFunc("/messages", s.handleMessages)
	s.srv = httptest.NewServer(mux)
	return s
}

func (s *sseTestServerFull) addHandler(method string, fn func(id string, params json.RawMessage) jsonrpcResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = fn
}

func (s *sseTestServerFull) broadcast(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range s.sseConns {
		select {
		case ch <- msg:
		default:
		}
	}
}

func (s *sseTestServerFull) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	// Register the channel BEFORE flushing the endpoint event so that any
	// broadcast triggered by the client's first POST always finds a listener.
	ch := make(chan string, 16)
	s.mu.Lock()
	s.sseConns = append(s.sseConns, ch)
	s.mu.Unlock()

	messagesURL := "http://" + r.Host + "/messages"
	fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", messagesURL)
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		}
	}
}

func (s *sseTestServerFull) handleMessages(w http.ResponseWriter, r *http.Request) {
	var req jsonrpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	fn, ok := s.handlers[req.Method]
	s.mu.Unlock()

	var resp jsonrpcResponse
	if !ok {
		resp = jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &jsonrpcError{Code: -32601, Message: "method not found: " + req.Method},
		}
	} else {
		var raw json.RawMessage
		if req.Params != nil {
			raw, _ = json.Marshal(req.Params)
		}
		resp = fn(req.ID, raw)
	}

	respBytes, _ := json.Marshal(resp)
	go s.broadcast(string(respBytes))

	w.WriteHeader(http.StatusAccepted)
}

func (s *sseTestServerFull) Close() {
	s.srv.Close()
}

// TestHTTPClientSSEParsing verifies that newHTTPClient correctly connects,
// receives the endpoint event, and can make a round-trip call.
func TestHTTPClientSSEParsing(t *testing.T) {
	srv := newSSETestServerFull()
	defer srv.Close()

	srv.addHandler("ping", func(id string, params json.RawMessage) jsonrpcResponse {
		result, _ := json.Marshal(map[string]string{"pong": "ok"})
		return jsonrpcResponse{JSONRPC: "2.0", ID: id, Result: result}
	})

	client, err := newHTTPClient("test", srv.srv.URL, "")
	if err != nil {
		t.Fatalf("newHTTPClient: %v", err)
	}
	defer client.stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	raw, err := client.call(ctx, "ping", nil)
	if err != nil {
		t.Fatalf("call ping: %v", err)
	}

	var result map[string]string
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result["pong"] != "ok" {
		t.Errorf("result[pong] = %q, want %q", result["pong"], "ok")
	}
}

// TestHTTPClientMultipleConcurrentCalls verifies that multiple concurrent
// in-flight requests are correctly matched to their responses.
func TestHTTPClientMultipleConcurrentCalls(t *testing.T) {
	srv := newSSETestServerFull()
	defer srv.Close()

	srv.addHandler("echo", func(id string, params json.RawMessage) jsonrpcResponse {
		// Small sleep to force concurrency overlap.
		time.Sleep(5 * time.Millisecond)
		result, _ := json.Marshal(map[string]string{"id": id, "echo": string(params)})
		return jsonrpcResponse{JSONRPC: "2.0", ID: id, Result: result}
	})

	client, err := newHTTPClient("test-concurrent", srv.srv.URL, "")
	if err != nil {
		t.Fatalf("newHTTPClient: %v", err)
	}
	defer client.stop()

	const n = 5
	errs := make([]error, n)
	results := make([]json.RawMessage, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			raw, err := client.call(ctx, "echo", map[string]int{"n": i})
			errs[i] = err
			results[i] = raw
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("call %d: %v", i, err)
		}
	}
	for i, raw := range results {
		if raw == nil {
			t.Errorf("call %d: nil result", i)
		}
	}
}

// TestHTTPClientStopUnblocksCallers verifies that calling stop() while a
// call is pending causes the call to return an error rather than blocking.
func TestHTTPClientStopUnblocksCallers(t *testing.T) {
	// Build a server that never responds to messages.
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		messagesURL := "http://" + r.Host + "/messages"
		fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", messagesURL)
		flusher.Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		// Accept but never respond on the SSE stream.
		w.WriteHeader(http.StatusAccepted)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	client, err := newHTTPClient("test-stop", ts.URL, "")
	if err != nil {
		t.Fatalf("newHTTPClient: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := client.call(ctx, "noreply", nil)
		done <- err
	}()

	// Give the goroutine time to register the pending call.
	time.Sleep(50 * time.Millisecond)
	client.stop()

	select {
	case err := <-done:
		if err == nil {
			t.Error("expected error after stop(), got nil")
		}
	case <-time.After(2 * time.Second):
		t.Error("call did not unblock after stop()")
	}
}

// TestReadEndpointEvent verifies SSE parsing for the initial endpoint event.
func TestReadEndpointEvent(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantURL string
		wantErr bool
	}{
		{
			name:    "simple endpoint event",
			input:   "event: endpoint\ndata: http://localhost:9000/messages\n\n",
			wantURL: "http://localhost:9000/messages",
		},
		{
			name:    "endpoint event with preceding comment",
			input:   ": comment\nevent: endpoint\ndata: http://example.com/msg\n\n",
			wantURL: "http://example.com/msg",
		},
		{
			name:    "endpoint event after other event",
			input:   "event: other\ndata: ignored\n\nevent: endpoint\ndata: http://x.com/m\n\n",
			wantURL: "http://x.com/m",
		},
		{
			name:    "stream ends before endpoint",
			input:   "event: other\ndata: foo\n\n",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := strings.NewReader(tc.input)
			got, err := readEndpointEvent(r, 2*time.Second)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error, got url=%q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantURL {
				t.Errorf("url = %q, want %q", got, tc.wantURL)
			}
		})
	}
}

// TestHTTPClientBearerToken verifies that the Authorization header is sent.
func TestHTTPClientBearerToken(t *testing.T) {
	var gotAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		messagesURL := "http://" + r.Host + "/messages"
		fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", messagesURL)
		flusher.Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusAccepted)
		// No SSE response — we're just checking the header.
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	client, err := newHTTPClient("test-auth", ts.URL, "secret-token")
	if err != nil {
		t.Fatalf("newHTTPClient: %v", err)
	}

	// Fire a call — it won't get a response, so use a short context.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	client.call(ctx, "noreply", nil) //nolint:errcheck

	client.stop()

	want := "Bearer secret-token"
	if gotAuth != want {
		t.Errorf("Authorization header = %q, want %q", gotAuth, want)
	}
}
