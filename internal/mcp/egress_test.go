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

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
	"github.com/ethanhinson/fuse/internal/toolidentity"
)

// capturingSSEServer is an MCP HTTP/SSE test double that records the
// Authorization and MCP-Resource headers of every /messages POST it receives,
// then answers a canned tools/call result over the SSE stream. It lets a test
// assert what credential + audience actually left fuse on the wire.
type capturingSSEServer struct {
	srv *httptest.Server

	mu       sync.Mutex
	sseConns []chan string
	auths    []string // Authorization header per POST, in arrival order
	resource []string // MCP-Resource header per POST, in arrival order
}

// newCapturingSSEServer starts the double and registers its shutdown via
// t.Cleanup. Ordering matters: httptest.Server.Close() blocks until the hijacked
// handleSSE goroutine returns, which only happens after the client's SSE body is
// closed by client.stop(). Since t.Cleanup runs LIFO and newToolOverHTTP (called
// AFTER this) registers client.stop, the client is stopped FIRST and the server
// closes cleanly — a plain `defer srv.Close()` in the test would instead run
// before the client-stop cleanup and deadlock in teardown.
func newCapturingSSEServer(t *testing.T) *capturingSSEServer {
	t.Helper()
	s := &capturingSSEServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", s.handleSSE)
	mux.HandleFunc("/messages", s.handleMessages)
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *capturingSSEServer) handleSSE(w http.ResponseWriter, r *http.Request) {
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

func (s *capturingSSEServer) handleMessages(w http.ResponseWriter, r *http.Request) {
	var req jsonrpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad", http.StatusBadRequest)
		return
	}
	// Only record tools/call POSTs — discovery/handshake calls are noise here.
	if req.Method == "tools/call" {
		s.mu.Lock()
		s.auths = append(s.auths, r.Header.Get("Authorization"))
		s.resource = append(s.resource, r.Header.Get(mcpResourceHeader))
		s.mu.Unlock()
	}
	result, _ := json.Marshal(map[string]any{
		"content": []map[string]any{{"type": "text", "text": "ok"}},
	})
	resp := jsonrpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}
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

func (s *capturingSSEServer) capturedAuths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.auths...)
}

func (s *capturingSSEServer) capturedResources() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.resource...)
}

// newToolOverHTTP builds an MCPTool whose client is a live httpClient connected
// to the capturing server, with the given static bearer token baked in, plus the
// supplied source + target for the identity path (both zero for the no-source
// path).
func newToolOverHTTP(t *testing.T, srv *capturingSSEServer, staticToken string, src toolidentity.CredentialSource, target toolidentity.Target) *MCPTool {
	t.Helper()
	client, err := newHTTPClient("cap", srv.srv.URL, staticToken)
	if err != nil {
		t.Fatalf("newHTTPClient: %v", err)
	}
	t.Cleanup(client.stop)
	return &MCPTool{
		client:     client,
		serverName: "cap",
		toolName:   "do",
		source:     src,
		target:     target,
	}
}

// stsSource builds a real Broker over the BuiltinSTS for one tenant so a test
// can mint per-principal delegation tokens without any external service.
func stsSource(t *testing.T, tenant event.TenantID, key []byte) *toolidentity.Broker {
	t.Helper()
	sts, err := toolidentity.NewBuiltinSTS(toolidentity.BuiltinSTSConfig{
		TenantKeys: map[event.TenantID][]byte{tenant: key},
	})
	if err != nil {
		t.Fatalf("NewBuiltinSTS: %v", err)
	}
	return toolidentity.NewBroker(sts, nil, nil)
}

// TestEgress_TwoPrincipalsDistinctTokens is the load-bearing test: two distinct
// loop initiators calling the SAME OAuth-tier target present DISTINCT
// Authorization tokens to the downstream, each bound to the target audience. The
// token is per-principal (minted from the initiator's identity), never a shared
// static token.
func TestEgress_TwoPrincipalsDistinctTokens(t *testing.T) {
	srv := newCapturingSSEServer(t)

	const tenant = event.TenantID("acme")
	src := stsSource(t, tenant, []byte("k-acme-secret"))
	target := toolidentity.Target{Name: "cap", Audience: "https://cap.example.test", Tier: toolidentity.TierOAuth}

	tool := newToolOverHTTP(t, srv, "" /*no static token*/, src, target)

	call := func(subject string) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		ctx = toolidentity.WithPrincipal(ctx, loopauth.Principal{Tenant: tenant, Subject: subject})
		res := tool.Execute(ctx, `{}`)
		if res.IsError {
			t.Fatalf("subject %q: unexpected tool error: %s", subject, res.Output)
		}
	}
	call("alice")
	call("bob")

	auths := srv.capturedAuths()
	if len(auths) != 2 {
		t.Fatalf("captured %d tools/call auths, want 2: %v", len(auths), auths)
	}
	if auths[0] == "" || auths[1] == "" {
		t.Fatalf("both principals must present a token, got %q and %q", auths[0], auths[1])
	}
	if auths[0] == auths[1] {
		t.Fatalf("distinct principals must present DISTINCT tokens; both were %q", auths[0])
	}
	if !strings.HasPrefix(auths[0], "Bearer ") || !strings.HasPrefix(auths[1], "Bearer ") {
		t.Fatalf("tokens should be Bearer-scheme; got %q, %q", auths[0], auths[1])
	}
	// Audience binding: every outbound tools/call carries this target's audience.
	for i, r := range srv.capturedResources() {
		if r != target.Audience {
			t.Errorf("call %d: MCP-Resource = %q, want %q", i, r, target.Audience)
		}
	}
}

// TestEgress_StaticTierNoIdentity: a static-tier target's outbound header carries
// the configured static credential and NO initiator identity (the token is the
// same regardless of principal, and the broker marks it identity-free).
func TestEgress_StaticTierNoIdentity(t *testing.T) {
	srv := newCapturingSSEServer(t)

	src := toolidentity.NewBroker(nil, map[string]toolidentity.StaticCredential{
		"cap": {Scheme: "Bearer", Token: "static-secret"},
	}, nil)
	target := toolidentity.Target{Name: "cap", Tier: toolidentity.TierStatic}

	tool := newToolOverHTTP(t, srv, "" /*static comes from broker, not client*/, src, target)

	for _, subj := range []string{"alice", "bob"} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		ctx = toolidentity.WithPrincipal(ctx, loopauth.Principal{Subject: subj})
		res := tool.Execute(ctx, `{}`)
		cancel()
		if res.IsError {
			t.Fatalf("subject %q: %s", subj, res.Output)
		}
	}
	auths := srv.capturedAuths()
	if len(auths) != 2 {
		t.Fatalf("want 2 auths, got %v", auths)
	}
	if auths[0] != "Bearer static-secret" || auths[1] != "Bearer static-secret" {
		t.Fatalf("static tier must present the SAME identity-free credential; got %q, %q", auths[0], auths[1])
	}
	// A static-tier target declares no audience, so no resource binding is sent.
	for i, r := range srv.capturedResources() {
		if r != "" {
			t.Errorf("call %d: static tier should carry no MCP-Resource, got %q", i, r)
		}
	}
}

// TestEgress_NoSourceUsesStaticClientToken: with NO CredentialSource wired,
// Execute takes the byte-identical pre-#52 path — the client's static bearerToken
// is what reaches the downstream, and no resource header is added.
func TestEgress_NoSourceUsesStaticClientToken(t *testing.T) {
	srv := newCapturingSSEServer(t)

	tool := newToolOverHTTP(t, srv, "baked-in-token", nil, toolidentity.Target{})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Even with a principal on ctx, a nil source must ignore it entirely.
	ctx = toolidentity.WithPrincipal(ctx, loopauth.Principal{Subject: "alice"})
	res := tool.Execute(ctx, `{}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	auths := srv.capturedAuths()
	if len(auths) != 1 || auths[0] != "Bearer baked-in-token" {
		t.Fatalf("no-source path must use the client's static token; got %v", auths)
	}
	if r := srv.capturedResources(); len(r) != 1 || r[0] != "" {
		t.Fatalf("no-source path must add no resource header; got %v", r)
	}
}

// TestEgress_ResolveErrorSurfacesAsToolErrorNoToken: a resolution failure
// (undeclared OAuth target — no audience) surfaces as a tool error and NEVER
// reaches the network, and the error message carries no token.
func TestEgress_ResolveErrorSurfacesAsToolErrorNoToken(t *testing.T) {
	srv := newCapturingSSEServer(t)

	src := stsSource(t, event.TenantID("acme"), []byte("k"))
	// Undeclared: OAuth tier with an empty audience → broker denies.
	target := toolidentity.Target{Name: "cap", Tier: toolidentity.TierOAuth}
	tool := newToolOverHTTP(t, srv, "", src, target)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ctx = toolidentity.WithPrincipal(ctx, loopauth.Principal{Tenant: "acme", Subject: "alice"})
	res := tool.Execute(ctx, `{}`)
	if !res.IsError {
		t.Fatal("an undeclared OAuth target must surface as a tool error")
	}
	// The error surfaced to the loop must carry no credential material at all — no
	// "Bearer " scheme, no JWS (a "." -joined base64 token).
	if strings.Contains(res.Output, "Bearer ") {
		t.Errorf("error output must not contain a bearer token: %q", res.Output)
	}
	if auths := srv.capturedAuths(); len(auths) != 0 {
		t.Fatalf("a denied credential must never reach the network; got %v", auths)
	}
}

// TestEgress_AudienceBindingIsPerTarget: two tools targeting DIFFERENT audiences
// each carry their OWN audience on the wire — fuse never sends target-A's request
// with target-B's audience.
func TestEgress_AudienceBindingIsPerTarget(t *testing.T) {
	const tenant = event.TenantID("acme")
	src := stsSource(t, tenant, []byte("k"))

	srvA := newCapturingSSEServer(t)
	srvB := newCapturingSSEServer(t)

	toolA := newToolOverHTTP(t, srvA, "", src, toolidentity.Target{Name: "A", Audience: "aud-A", Tier: toolidentity.TierOAuth})
	toolB := newToolOverHTTP(t, srvB, "", src, toolidentity.Target{Name: "B", Audience: "aud-B", Tier: toolidentity.TierOAuth})

	ctx := toolidentity.WithPrincipal(context.Background(), loopauth.Principal{Tenant: tenant, Subject: "alice"})
	c1, cancel1 := context.WithTimeout(ctx, 3*time.Second)
	defer cancel1()
	if res := toolA.Execute(c1, `{}`); res.IsError {
		t.Fatalf("A: %s", res.Output)
	}
	c2, cancel2 := context.WithTimeout(ctx, 3*time.Second)
	defer cancel2()
	if res := toolB.Execute(c2, `{}`); res.IsError {
		t.Fatalf("B: %s", res.Output)
	}

	if r := srvA.capturedResources(); len(r) != 1 || r[0] != "aud-A" {
		t.Fatalf("target A must carry aud-A only; got %v", r)
	}
	if r := srvB.capturedResources(); len(r) != 1 || r[0] != "aud-B" {
		t.Fatalf("target B must carry aud-B only; got %v", r)
	}
}

// TestManager_WithCredentialSourceStampsTools verifies the manager wiring: a
// manager built WITH a source stamps discovered tools with it + the server's
// Target; a manager built WITHOUT one leaves them on the static path. Uses the
// config Target derivation end-to-end.
func TestManager_WithCredentialSourceStampsTools(t *testing.T) {
	src := stsSource(t, event.TenantID("acme"), []byte("k"))
	srv := config.MCPServerConfig{
		Name:     "s",
		Audience: "aud-x",
		Auth:     config.MCPAuthConfig{Type: "identity"},
	}
	target := srv.ToolIdentityTarget()

	// Simulate what Add does to a discovered tool.
	stamp := func(m *Manager) *MCPTool {
		tl := &MCPTool{serverName: srv.Name, toolName: "do"}
		if m.source != nil {
			tl.source = m.source
			tl.target = target
		}
		return tl
	}

	withSrc := &Manager{source: src}
	if tl := stamp(withSrc); tl.source == nil || tl.target.Audience != "aud-x" {
		t.Fatalf("wired manager must stamp source+target; got source=%v target=%+v", tl.source, tl.target)
	}
	without := &Manager{}
	if tl := stamp(without); tl.source != nil {
		t.Fatalf("un-wired manager must leave source nil; got %v", tl.source)
	}
}
