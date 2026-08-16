package rentals

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
	"github.com/ethanhinson/fuse/internal/mcp"
	"github.com/ethanhinson/fuse/internal/toolidentity"
	"github.com/ethanhinson/fuse/internal/tools"
)

// TestNewHandlerServesCompleteMCPSurface proves the exported handler is a complete,
// listener-free MCP surface: a CALLER-owned httptest.Server hosts it (NewHandler binds
// nothing itself), and a real fuse MCP client drives tools/list + an authorized
// search_rentals over it. This is the shape cmd/rentals-mcp needs — the routed mux
// without NewServer's self-hosting.
//
// Teardown discipline (learning httptest-defer-close-before-tcleanup-deadlock): handleSSE
// loops on r.Context().Done(), so Close() blocks until the client read-pump disconnects.
// Both teardowns go through t.Cleanup — server registered FIRST, client SECOND, so LIFO
// stops the client before the server. NEVER `defer httpSrv.Close()` here.
func TestNewHandlerServesCompleteMCPSurface(t *testing.T) {
	keys := map[event.TenantID][]byte{"acme": []byte("k-acme")}

	s, h := NewHandler(Config{Audience: testAudience, TenantKeys: keys})
	if s == nil || h == nil {
		t.Fatal("NewHandler must return both the server and its routed handler")
	}

	httpSrv := httptest.NewServer(h)
	t.Cleanup(httpSrv.Close)

	sts, err := toolidentity.NewBuiltinSTS(toolidentity.BuiltinSTSConfig{Issuer: "fuse", TTL: time.Minute, TenantKeys: keys})
	if err != nil {
		t.Fatalf("NewBuiltinSTS: %v", err)
	}
	reg := tools.NewRegistry()
	mgr, err := mcp.NewManager([]config.MCPServerConfig{{
		Name:      "rentals",
		Transport: "sse",
		URL:       httpSrv.URL,
		Audience:  testAudience,
		Auth:      config.MCPAuthConfig{Type: "identity"},
	}}, reg, mcp.WithCredentialSource(toolidentity.NewBroker(sts, nil, nil)))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(mgr.Close)

	// tools/list happened over the caller-owned listener: the full advertised surface
	// landed in the registry.
	for _, name := range []string{"search_rentals", "favorite_listing", "list_favorites"} {
		if !reg.Has("mcp:rentals/" + name) {
			t.Fatalf("tools/list over the caller-hosted handler did not register %q", name)
		}
	}

	// One authorized tools/call over the same listener.
	ctx, cancel := context.WithTimeout(
		toolidentity.WithPrincipal(context.Background(), loopauth.Principal{Tenant: "acme", Subject: "alice"}),
		5*time.Second)
	defer cancel()
	res := reg.Execute(ctx, "mcp:rentals/search_rentals", `{"query":"tahoe"}`)
	if res.IsError {
		t.Fatalf("search_rentals over the caller-hosted handler errored: %s", res.Output)
	}
	if !strings.Contains(res.Output, "Mountain Cabin") {
		t.Fatalf("expected the Tahoe listing, got %q", res.Output)
	}

	// The call really went through this Server instance's adjudication path.
	if auths := s.CapturedAuths(); len(auths) != 1 || !strings.HasPrefix(auths[0], "Bearer ") {
		t.Fatalf("expected one Bearer-credentialed tools/call on the wire, got %v", auths)
	}
}
