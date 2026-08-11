package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// stubMCPServer is a minimal REAL MCP HTTP/SSE server for the loop-server tests: it
// completes the MCP init handshake, advertises one tool per tools/list, and answers
// tools/call over the SSE stream. It is deliberately generic (Task 4 isolation
// proof); the domain-specific rentals server with per-principal state is a separate
// harness (Task 6). fuse's real client talks to it over the wire, so a loop that can
// list + invoke its tool proves the per-loop manager attach is real, not mocked.
type stubMCPServer struct {
	srv      *httptest.Server
	toolName string

	mu       sync.Mutex
	sseConns []chan string
	calls    int // tools/call count (proves the wire round-trip happened)
}

// newStubMCPServer starts the stub advertising a single tool named toolName.
func newStubMCPServer(t *testing.T, toolName string) *stubMCPServer {
	t.Helper()
	s := &stubMCPServer{toolName: toolName}
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", s.handleSSE)
	mux.HandleFunc("/messages", s.handleMessages)
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stubMCPServer) URL() string { return s.srv.URL }

func (s *stubMCPServer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *stubMCPServer) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flush", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	ch := make(chan string, 16)
	s.mu.Lock()
	s.sseConns = append(s.sseConns, ch)
	s.mu.Unlock()
	fmt.Fprintf(w, "event: endpoint\ndata: http://%s/messages\n\n", r.Host)
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

func (s *stubMCPServer) handleMessages(w http.ResponseWriter, r *http.Request) {
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad", http.StatusBadRequest)
		return
	}
	var result any
	switch req.Method {
	case "initialize":
		result = map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{},
			"serverInfo":      map[string]any{"name": "stub", "version": "1.0.0"},
		}
	case "tools/list":
		result = map[string]any{
			"tools": []map[string]any{{
				"name":        s.toolName,
				"description": "stub tool",
				"inputSchema": map[string]any{"type": "object"},
			}},
		}
	case "tools/call":
		s.mu.Lock()
		s.calls++
		s.mu.Unlock()
		result = map[string]any{
			"content": []map[string]any{{"type": "text", "text": "stub-ok"}},
		}
	default:
		// notifications/initialized and any other notification: 202, no reply frame.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	resp := map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}
	b, _ := json.Marshal(resp)
	s.mu.Lock()
	conns := append([]chan string(nil), s.sseConns...)
	s.mu.Unlock()
	for _, c := range conns {
		select {
		case c <- string(b):
		default:
		}
	}
	w.WriteHeader(http.StatusAccepted)
}
