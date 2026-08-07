package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/config"
)

// --- test recorder (mutex-protected: the httptest handler runs on server
// goroutines; see the mutex-test-double-concurrent-provider learning) ---

type recordedReq struct {
	httpMethod string
	rpcMethod  string
	sessionID  string
	protoVer   string
	auth       string
	accept     string
	bodyLen    int
	body       string
	lastEvent  string
}

type reqRecord struct {
	mu   sync.Mutex
	reqs []recordedReq
}

func (r *reqRecord) add(rec recordedReq) {
	r.mu.Lock()
	r.reqs = append(r.reqs, rec)
	r.mu.Unlock()
}

func (r *reqRecord) all() []recordedReq {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedReq(nil), r.reqs...)
}

// observe reads an incoming request, records it, and returns the parsed JSON-RPC
// method + id and the raw body.
func observe(rec *reqRecord, r *http.Request) (rpcMethod, id string, body []byte) {
	body, _ = io.ReadAll(r.Body)
	var parsed struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	_ = json.Unmarshal(body, &parsed)
	id = strings.Trim(string(parsed.ID), `"`)
	rec.add(recordedReq{
		httpMethod: r.Method,
		rpcMethod:  parsed.Method,
		sessionID:  r.Header.Get(hdrSessionID),
		protoVer:   r.Header.Get(hdrProtocol),
		auth:       r.Header.Get("Authorization"),
		accept:     r.Header.Get("Accept"),
		bodyLen:    len(body),
		body:       string(body),
		lastEvent:  r.Header.Get(hdrLastEvent),
	})
	return parsed.Method, id, body
}

func rpcResult(id string, result any) string {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	return string(b)
}

// jsonInitResult / toolsListResult are the canonical happy-path payloads.
func writeJSON(w http.ResponseWriter, s string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, s)
}

const echoToolsList = `{"tools":[{"name":"echo","description":"echoes input"}]}`

// --- Task 1: dial routing ---

func TestStreamableDialRouting(t *testing.T) {
	c, err := dial(config.MCPServerConfig{Name: "s", Transport: "streamable-http", URL: "https://example.com/mcp"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, ok := c.(*StreamableHTTPClient); !ok {
		t.Fatalf("dial returned %T, want *StreamableHTTPClient", c)
	}

	if _, err := dial(config.MCPServerConfig{Name: "s", Transport: "streamable-http"}); err == nil {
		t.Fatal("expected error for streamable-http with no url")
	} else if !strings.Contains(err.Error(), "requires a url") {
		t.Fatalf("error = %v, want 'requires a url'", err)
	}
}

// --- Task 2: synchronous application/json + full handshake + session/proto echo ---

func TestStreamableSyncHandshake(t *testing.T) {
	rec := &reqRecord{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, id, _ := observe(rec, r)
		switch method {
		case "initialize":
			w.Header().Set(hdrSessionID, "sess-1")
			writeJSON(w, rpcResult(id, map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}}))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			writeJSON(w, rpcResult(id, json.RawMessage(echoToolsList)))
		default:
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	client, _ := newStreamableHTTPClient("s", srv.URL, "", config.MCPAuthConfig{})
	tools, _, ver, err := handshakeAndDiscover(context.Background(), client, "s")
	if err != nil {
		t.Fatalf("handshakeAndDiscover: %v", err)
	}
	if ver != "2025-03-26" {
		t.Errorf("protoVer = %q, want 2025-03-26", ver)
	}
	if len(tools) != 1 || tools[0].toolName != "echo" {
		t.Fatalf("tools = %+v, want one 'echo'", tools)
	}

	// Every request after initialize must echo the captured session id and the
	// protocol-version header.
	all := rec.all()
	if len(all) < 3 {
		t.Fatalf("expected >=3 requests, got %d", len(all))
	}
	for _, rq := range all {
		if rq.protoVer != "2025-03-26" {
			t.Errorf("%s: MCP-Protocol-Version = %q, want 2025-03-26", rq.rpcMethod, rq.protoVer)
		}
		if !strings.Contains(rq.accept, "text/event-stream") || !strings.Contains(rq.accept, "application/json") {
			t.Errorf("%s: Accept = %q, want both json and event-stream", rq.rpcMethod, rq.accept)
		}
	}
	for _, rq := range all[1:] { // all except the initialize that established it
		if rq.sessionID != "sess-1" {
			t.Errorf("%s: session = %q, want sess-1 echoed", rq.rpcMethod, rq.sessionID)
		}
	}
}

// --- Task 3: notify carries no id ---

func TestStreamableNotify(t *testing.T) {
	rec := &reqRecord{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observe(rec, r)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	client, _ := newStreamableHTTPClient("s", srv.URL, "", config.MCPAuthConfig{})
	if err := client.notify(context.Background(), "notifications/initialized", nil); err != nil {
		t.Fatalf("notify: %v", err)
	}
	all := rec.all()
	if len(all) != 1 {
		t.Fatalf("want 1 request, got %d", len(all))
	}
	if all[0].rpcMethod != "notifications/initialized" {
		t.Errorf("rpcMethod = %q", all[0].rpcMethod)
	}
	if all[0].bodyLen == 0 {
		t.Error("notify sent an empty body")
	}
	// A notification frame must not carry an id.
	if strings.Contains(all[0].body, `"id"`) {
		t.Errorf("notification carried an id: %s", all[0].body)
	}
}

// --- Task 4: streaming text/event-stream response + notification seam ---

func TestStreamableStreamingResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, id, _ := observe(&reqRecord{}, r)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		// An interleaved id-less server notification (must be ignored/routed to
		// the seam, not mistaken for the response).
		io.WriteString(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"$/progress\",\"params\":{}}\n\n")
		fl.Flush()
		// The actual response for this call.
		fmt.Fprintf(w, "id: e1\ndata: %s\n\n", rpcResult(id, map[string]any{"ok": true}))
		fl.Flush()
	}))
	defer srv.Close()

	client, _ := newStreamableHTTPClient("s", srv.URL, "", config.MCPAuthConfig{})
	raw, err := client.call(context.Background(), "tools/call", map[string]any{"name": "echo"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !strings.Contains(string(raw), "\"ok\":true") {
		t.Fatalf("result = %s, want ok:true", raw)
	}
}

// --- Task 5: session lifecycle — echo on every request + DELETE on stop ---

func TestStreamableStopDeletesSession(t *testing.T) {
	rec := &reqRecord{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, id, _ := observe(rec, r)
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusOK)
			return
		}
		if method == "initialize" {
			w.Header().Set(hdrSessionID, "sess-x")
		}
		writeJSON(w, rpcResult(id, map[string]any{}))
	}))
	defer srv.Close()

	client, _ := newStreamableHTTPClient("s", srv.URL, "", config.MCPAuthConfig{})
	if _, err := client.call(context.Background(), "initialize", initializeParams()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	client.stop()

	var sawDelete bool
	for _, rq := range rec.all() {
		if rq.httpMethod == http.MethodDelete {
			sawDelete = true
			if rq.sessionID != "sess-x" {
				t.Errorf("DELETE session = %q, want sess-x", rq.sessionID)
			}
		}
	}
	if !sawDelete {
		t.Error("stop() did not fire a DELETE for the session")
	}
}

func TestStreamableStatelessNoDelete(t *testing.T) {
	rec := &reqRecord{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, id, _ := observe(rec, r)
		writeJSON(w, rpcResult(id, map[string]any{})) // never sets Mcp-Session-Id
	}))
	defer srv.Close()

	client, _ := newStreamableHTTPClient("s", srv.URL, "", config.MCPAuthConfig{})
	if _, err := client.call(context.Background(), "initialize", initializeParams()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	client.stop()

	for _, rq := range rec.all() {
		if rq.httpMethod == http.MethodDelete {
			t.Error("stateless server: stop() should not DELETE")
		}
		if rq.sessionID != "" {
			t.Errorf("stateless server: request carried session %q", rq.sessionID)
		}
	}
}

// --- Task 6: 401 refresh + body rewind (the regression guard) ---

func TestStreamable401RefreshRewindsBody(t *testing.T) {
	rec := &reqRecord{}
	var n int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, id, _ := observe(rec, r)
		mu.Lock()
		n++
		attempt := n
		mu.Unlock()
		if attempt == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, rpcResult(id, map[string]any{"ok": true}))
	}))
	defer srv.Close()

	// Initial token stale; refresh (bearer) yields the new token.
	client, _ := newStreamableHTTPClient("s", srv.URL, "stale", config.MCPAuthConfig{Type: "bearer", ClientSecret: "refreshed"})
	raw, err := client.call(context.Background(), "tools/call", map[string]any{"name": "echo"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !strings.Contains(string(raw), "ok") {
		t.Fatalf("result = %s", raw)
	}

	all := rec.all()
	if len(all) != 2 {
		t.Fatalf("want 2 attempts, got %d", len(all))
	}
	// The retried attempt must carry the refreshed token AND a full (non-empty)
	// body — the rewind-request-body-on-manual-retry regression.
	if all[1].bodyLen == 0 {
		t.Error("retried request sent an EMPTY body (body was not rewound)")
	}
	if all[1].bodyLen != all[0].bodyLen {
		t.Errorf("retried body len = %d, want %d (same payload)", all[1].bodyLen, all[0].bodyLen)
	}
	if all[1].auth != "Bearer refreshed" {
		t.Errorf("retried auth = %q, want 'Bearer refreshed'", all[1].auth)
	}
}

// --- Task 7: 404 session expiry -> inline reinitialize + retry ---

func TestStreamable404Reinitializes(t *testing.T) {
	rec := &reqRecord{}
	var mu sync.Mutex
	var initCount, callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, id, _ := observe(rec, r)
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "initialize":
			initCount++
			w.Header().Set(hdrSessionID, fmt.Sprintf("sess-%d", initCount))
			writeJSON(w, rpcResult(id, map[string]any{"protocolVersion": "2025-03-26"}))
		case "tools/call":
			callCount++
			if callCount == 1 {
				w.WriteHeader(http.StatusNotFound) // session expired
				return
			}
			writeJSON(w, rpcResult(id, map[string]any{"ok": true}))
		default:
			writeJSON(w, rpcResult(id, map[string]any{}))
		}
	}))
	defer srv.Close()

	client, _ := newStreamableHTTPClient("s", srv.URL, "", config.MCPAuthConfig{})
	// Establish the first session.
	if _, err := client.call(context.Background(), "initialize", initializeParams()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	// This call 404s, triggers reinit -> sess-2, then retries successfully.
	if _, err := client.call(context.Background(), "tools/call", map[string]any{"name": "echo"}); err != nil {
		t.Fatalf("tools/call: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if initCount != 2 {
		t.Errorf("initCount = %d, want 2 (original + reinit)", initCount)
	}
	// The final (successful) tools/call must carry the refreshed session.
	all := rec.all()
	last := all[len(all)-1]
	if last.rpcMethod != "tools/call" || last.sessionID != "sess-2" {
		t.Errorf("final call session = %q, want sess-2", last.sessionID)
	}
}

// --- Task 8: resumability via Last-Event-Id ---

func TestStreamableResumeAfterDisconnect(t *testing.T) {
	var mu sync.Mutex
	var origID string // the id from the initial POST; the resume must reuse it
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, id, _ := observe(&reqRecord{}, r)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		if r.Method == http.MethodGet && r.Header.Get(hdrLastEvent) == "evt-1" {
			// Resume: replay the real response, carrying the ORIGINAL id (the GET
			// itself has no body — resumption correlates by Last-Event-Id).
			mu.Lock()
			resumeID := origID
			mu.Unlock()
			fmt.Fprintf(w, "id: evt-2\ndata: %s\n\n", rpcResult(resumeID, map[string]any{"resumed": true}))
			fl.Flush()
			return
		}
		// Initial POST stream: record the id, emit an SSE id, then disconnect
		// before the response frame arrives.
		mu.Lock()
		origID = id
		mu.Unlock()
		io.WriteString(w, "id: evt-1\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"$/progress\"}\n\n")
		fl.Flush()
		// Handler returns -> body EOF -> client sees errStreamClosed with evt-1.
	}))
	defer srv.Close()

	client, _ := newStreamableHTTPClient("s", srv.URL, "", config.MCPAuthConfig{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := client.call(ctx, "tools/call", map[string]any{"name": "slow"})
	if err != nil {
		t.Fatalf("call (with resume): %v", err)
	}
	if !strings.Contains(string(raw), "\"resumed\":true") {
		t.Fatalf("result = %s, want resumed:true", raw)
	}
}

// --- error mapping: a JSON-RPC error surfaces as *RPCError ---

func TestStreamableRPCErrorMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, id, _ := observe(&reqRecord{}, r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":"%s","error":{"code":-32900,"message":"tool not found"}}`, id)
	}))
	defer srv.Close()

	client, _ := newStreamableHTTPClient("s", srv.URL, "", config.MCPAuthConfig{})
	_, err := client.call(context.Background(), "tools/call", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32900 {
		t.Fatalf("err = %v, want *RPCError code -32900", err)
	}
}
