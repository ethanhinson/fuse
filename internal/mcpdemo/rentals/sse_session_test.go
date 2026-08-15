package rentals

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
)

// This file guards the SSE fan-out boundary: a JSON-RPC response belongs to exactly ONE
// SSE stream — the one whose session id the POST carried. It was written against a real
// defect: every response was broadcast to every connected client and connections were
// never pruned, so once two loops (two per-loop MCP managers, e.g. after a Wander user
// switch) held streams against one rentals server, one principal's list_favorites frame
// was written into the other principal's stream. Because internal/mcp/http_client.go
// numbers request ids PER CLIENT, the two id spaces collide by construction, so a stray
// frame can resolve the other principal's pending call — defeating the per-principal
// isolation this server exists to demonstrate.
//
// Teardown discipline (see the package doc): handleSSE loops on r.Context().Done(), so
// Close() blocks until connected clients disconnect. EVERY teardown goes through
// t.Cleanup with the server registered FIRST and each client SECOND, so LIFO stops the
// clients before the server. NEVER `defer srv.Close()` here.

// rawSSEClient is a minimal, hand-rolled MCP-over-SSE client. It is deliberately NOT the
// fuse client: the defect under test is a frame arriving on the WRONG stream, and the
// fuse client silently drops frames it has no pending id for, so only a raw reader can
// observe the leak on the wire.
type rawSSEClient struct {
	endpoint string      // the POST URL the server advertised on the endpoint event
	frames   chan string // every non-endpoint data: frame, in arrival order
	body     io.ReadCloser
}

// dialRawSSE opens an SSE stream against base, reads the advertised endpoint event, and
// pumps subsequent frames into c.frames. Its disconnect is registered with t.Cleanup, so
// callers must have registered the server's Close BEFORE calling this.
func dialRawSSE(t *testing.T, base string) *rawSSEClient {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(base, "/")+"/sse", nil)
	if err != nil {
		t.Fatalf("build sse request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /sse: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("GET /sse status = %d, want 200", resp.StatusCode)
	}
	c := &rawSSEClient{frames: make(chan string, 16), body: resp.Body}
	// Registered AFTER the server's Close, so LIFO closes this stream first and the
	// server's handleSSE loop can return.
	t.Cleanup(func() { _ = resp.Body.Close() })

	endpointCh := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		var evName string
		var data []string
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event:"):
				evName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			case line == "":
				if len(data) > 0 {
					joined := strings.Join(data, "\n")
					if evName == "endpoint" {
						select {
						case endpointCh <- joined:
						default:
						}
					} else {
						select {
						case c.frames <- joined:
						default:
						}
					}
				}
				evName, data = "", nil
			}
		}
	}()

	select {
	case c.endpoint = <-endpointCh:
	case <-time.After(5 * time.Second):
		t.Fatal("never received the endpoint SSE event")
	}
	return c
}

// post sends one JSON-RPC frame to the endpoint URL this client was handed.
func (c *rawSSEClient) post(t *testing.T, token, audience, body string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, c.endpoint, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("build POST: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("MCP-Resource", audience)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", c.endpoint, err)
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST returned HTTP %d", resp.StatusCode)
	}
}

// waitFrame returns the next frame, or fails if none arrives in time.
func (c *rawSSEClient) waitFrame(t *testing.T, d time.Duration) string {
	t.Helper()
	select {
	case f := <-c.frames:
		return f
	case <-time.After(d):
		t.Fatalf("no SSE frame arrived within %s", d)
		return ""
	}
}

// mintTestToken builds the compact HS256 JWS the server's own verify() accepts: the same
// shape fuse's built-in STS mints, so this exercises the real adjudication path.
func mintTestToken(t *testing.T, key []byte, subject, audience string) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal token part: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signingInput := enc(map[string]any{"alg": "HS256", "typ": "JWT"}) +
		"." + enc(map[string]any{"sub": subject, "aud": audience})
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// TestSSEResponseGoesOnlyToTheRequestingSession is the isolation assertion on the wire:
// with TWO concurrent SSE clients on ONE rentals server, alice's list_favorites response
// must land on alice's stream and MUST NOT appear on bob's. Both clients deliberately use
// JSON-RPC id "1" — exactly the collision internal/mcp/http_client.go's per-client
// counter produces — so a leaked frame is one the other client would wrongly resolve.
func TestSSEResponseGoesOnlyToTheRequestingSession(t *testing.T) {
	keys := map[event.TenantID][]byte{"acme": []byte("k-acme"), "globex": []byte("k-globex")}
	srv := NewServer(Config{Audience: testAudience, TenantKeys: keys})
	t.Cleanup(srv.Close) // registered FIRST; client disconnects (below) run before it

	// Seed a favorite so the leaked frame would carry a concrete, attributable secret.
	if err := srv.favorites.Add(PrincipalKey{Tenant: "acme", Subject: "alice"}, "L1-alice-secret"); err != nil {
		t.Fatalf("seed alice favorite: %v", err)
	}

	alice := dialRawSSE(t, srv.URL())
	bob := dialRawSSE(t, srv.URL())

	aliceTok := mintTestToken(t, keys["acme"], "alice", testAudience)
	alice.post(t, aliceTok, testAudience,
		`{"jsonrpc":"2.0","id":"1","method":"tools/call","params":{"name":"list_favorites","arguments":{}}}`)

	got := alice.waitFrame(t, 5*time.Second)
	if !strings.Contains(got, "L1-alice-secret") {
		t.Fatalf("alice's own stream did not carry her favorites: %q", got)
	}

	// The fan-out is synchronous inside the POST handler: if bob's stream were going to
	// receive this frame, it already holds it now that alice's has arrived. A short grace
	// window covers scheduling, not a race.
	select {
	case leaked := <-bob.frames:
		t.Fatalf("bob's SSE stream received a frame addressed to alice's session (id collision would resolve it as bob's): %q", leaked)
	case <-time.After(500 * time.Millisecond):
	}
}

// TestSSEEndpointEventAdvertisesADistinctSession pins the mechanism the routing rests on:
// each stream is told a DIFFERENT POST URL, so the server can correlate a POST back to
// the stream that owns it. Without this the server has nothing to route by.
func TestSSEEndpointEventAdvertisesADistinctSession(t *testing.T) {
	srv := NewServer(Config{Audience: testAudience, TenantKeys: map[event.TenantID][]byte{"acme": []byte("k-acme")}})
	t.Cleanup(srv.Close)

	a := dialRawSSE(t, srv.URL())
	b := dialRawSSE(t, srv.URL())

	if a.endpoint == b.endpoint {
		t.Fatalf("both streams were advertised the same endpoint %q; the server cannot correlate a POST to its stream", a.endpoint)
	}
	for _, ep := range []string{a.endpoint, b.endpoint} {
		u, err := http.NewRequest(http.MethodPost, ep, nil)
		if err != nil {
			t.Fatalf("advertised endpoint %q is not a usable URL: %v", ep, err)
		}
		if u.URL.Query().Get("sessionId") == "" {
			t.Fatalf("advertised endpoint %q carries no sessionId", ep)
		}
	}
}

// TestSSEConnectionsArePrunedOnDisconnect: a disconnecting client's channel must be
// removed from the registry. Without pruning, a long-lived rentals process (cmd/rentals-mcp
// serving a per-loop MCP manager per loop) accumulates dead channels forever and keeps
// fanning frames at departed principals.
func TestSSEConnectionsArePrunedOnDisconnect(t *testing.T) {
	srv := NewServer(Config{Audience: testAudience, TenantKeys: map[event.TenantID][]byte{"acme": []byte("k-acme")}})
	t.Cleanup(srv.Close)

	const n = 3
	var bodies []io.ReadCloser
	for i := 0; i < n; i++ {
		c := dialRawSSE(t, srv.URL())
		bodies = append(bodies, c.body)
	}
	if got := srv.sseConnCount(); got != n {
		t.Fatalf("connected clients = %d, want %d", got, n)
	}

	for _, b := range bodies {
		_ = b.Close()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := srv.sseConnCount()
		if got == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after all %d clients disconnected, %d SSE connections are still registered; the registry grows unboundedly", n, got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
