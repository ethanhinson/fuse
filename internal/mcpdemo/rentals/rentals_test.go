package rentals

import (
	"context"
	"encoding/json"
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

const testAudience = "https://rentals.example"

// harness stands up a real rentals server + a real fuse MCP client (mcp.Manager over
// the wire) wired with a Broker/STS that mints per-principal delegation tokens under
// the given tenant keys. It returns the tool registry (to invoke tools) and the STS
// keys the server verifies with (shared out of band, as a real resource server does).
func harness(t *testing.T, tenantKeys map[event.TenantID][]byte) (*tools.Registry, *Server, func(subject string, tenant event.TenantID) context.Context) {
	t.Helper()
	srv := NewServer(Config{Audience: testAudience, TenantKeys: tenantKeys})
	t.Cleanup(srv.Close)

	sts, err := toolidentity.NewBuiltinSTS(toolidentity.BuiltinSTSConfig{Issuer: "fuse", TTL: time.Minute, TenantKeys: tenantKeys})
	if err != nil {
		t.Fatalf("NewBuiltinSTS: %v", err)
	}
	src := toolidentity.NewBroker(sts, nil, nil)

	reg := tools.NewRegistry()
	mgr, err := mcp.NewManager([]config.MCPServerConfig{{
		Name:      "rentals",
		Transport: "sse",
		URL:       srv.URL(),
		Audience:  testAudience,
		Auth:      config.MCPAuthConfig{Type: "identity"},
	}}, reg, mcp.WithCredentialSource(src))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(mgr.Close)

	ctxFor := func(subject string, tenant event.TenantID) context.Context {
		return toolidentity.WithPrincipal(context.Background(), loopauth.Principal{Tenant: tenant, Subject: subject})
	}
	return reg, srv, ctxFor
}

// TestSearchRentalsReturnsCannedListings: an authorized principal gets canned listings.
func TestSearchRentalsReturnsCannedListings(t *testing.T) {
	keys := map[event.TenantID][]byte{"acme": []byte("k-acme")}
	reg, _, ctxFor := harness(t, keys)

	ctx, cancel := context.WithTimeout(ctxFor("alice", "acme"), 5*time.Second)
	defer cancel()
	res := reg.Execute(ctx, "mcp:rentals/search_rentals", `{"query":""}`)
	if res.IsError {
		t.Fatalf("search_rentals errored: %s", res.Output)
	}
	if !strings.Contains(res.Output, "Beach Cottage") {
		t.Fatalf("expected canned listings, got %q", res.Output)
	}
}

// TestFavoritesArePerPrincipalIsolated is the deep confused-deputy assertion:
// alice's favorite_listing is invisible to bob's list_favorites and vice-versa. The
// store is keyed by TOKEN identity, never a client-supplied arg.
func TestFavoritesArePerPrincipalIsolated(t *testing.T) {
	keys := map[event.TenantID][]byte{"acme": []byte("k-acme"), "globex": []byte("k-globex")}
	reg, _, ctxFor := harness(t, keys)

	call := func(subject string, tenant event.TenantID, tool, args string) tools.Result {
		ctx, cancel := context.WithTimeout(ctxFor(subject, tenant), 5*time.Second)
		defer cancel()
		return reg.Execute(ctx, "mcp:rentals/"+tool, args)
	}

	// alice (acme) favorites L1.
	if res := call("alice", "acme", "favorite_listing", `{"listing_id":"L1"}`); res.IsError {
		t.Fatalf("alice favorite: %s", res.Output)
	}
	// bob (globex) favorites L2.
	if res := call("bob", "globex", "favorite_listing", `{"listing_id":"L2"}`); res.IsError {
		t.Fatalf("bob favorite: %s", res.Output)
	}

	aliceFavs := decodeFavs(t, call("alice", "acme", "list_favorites", `{}`))
	bobFavs := decodeFavs(t, call("bob", "globex", "list_favorites", `{}`))

	if !contains(aliceFavs, "L1") || contains(aliceFavs, "L2") {
		t.Fatalf("alice favorites = %v, want [L1] only (bob's L2 must be invisible)", aliceFavs)
	}
	if !contains(bobFavs, "L2") || contains(bobFavs, "L1") {
		t.Fatalf("bob favorites = %v, want [L2] only (alice's L1 must be invisible)", bobFavs)
	}
}

// TestUnauthorizedPrincipalGets403 (distinguishable tool error): a principal under a
// tenant the server has no key for is rejected — its token verifies under no key.
func TestUnauthorizedPrincipalGets403(t *testing.T) {
	// The client can MINT for "rogue" (its Broker has the key), but the SERVER has no
	// key for "rogue", so the token verifies under none → unauthorized.
	clientKeys := map[event.TenantID][]byte{"rogue": []byte("k-rogue")}
	srv := NewServer(Config{Audience: testAudience, TenantKeys: map[event.TenantID][]byte{"acme": []byte("k-acme")}})
	t.Cleanup(srv.Close)
	sts, _ := toolidentity.NewBuiltinSTS(toolidentity.BuiltinSTSConfig{TTL: time.Minute, TenantKeys: clientKeys})
	src := toolidentity.NewBroker(sts, nil, nil)
	reg := tools.NewRegistry()
	mgr, err := mcp.NewManager([]config.MCPServerConfig{{
		Name: "rentals", Transport: "sse", URL: srv.URL(), Audience: testAudience,
		Auth: config.MCPAuthConfig{Type: "identity"},
	}}, reg, mcp.WithCredentialSource(src))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(mgr.Close)

	ctx, cancel := context.WithTimeout(toolidentity.WithPrincipal(context.Background(), loopauth.Principal{Tenant: "rogue", Subject: "mallory"}), 5*time.Second)
	defer cancel()
	res := reg.Execute(ctx, "mcp:rentals/search_rentals", `{"query":""}`)
	if !res.IsError {
		t.Fatal("an unauthorized principal must surface as a tool error (403-equivalent)")
	}
	// Distinguishable: the server's -32001 code is surfaced, not a generic failure.
	if !strings.Contains(res.Output, "-32001") {
		t.Fatalf("expected a distinguishable authorization error code, got %q", res.Output)
	}
}

// TestWrongAudienceRejected: a token bound to the wrong audience is rejected by the
// server. The client declares a different audience than the server accepts.
func TestWrongAudienceRejected(t *testing.T) {
	keys := map[event.TenantID][]byte{"acme": []byte("k-acme")}
	srv := NewServer(Config{Audience: testAudience, TenantKeys: keys})
	t.Cleanup(srv.Close)
	sts, _ := toolidentity.NewBuiltinSTS(toolidentity.BuiltinSTSConfig{TTL: time.Minute, TenantKeys: keys})
	src := toolidentity.NewBroker(sts, nil, nil)
	reg := tools.NewRegistry()
	// The client's declared audience differs from the server's — the minted token is
	// bound to the wrong resource.
	mgr, err := mcp.NewManager([]config.MCPServerConfig{{
		Name: "rentals", Transport: "sse", URL: srv.URL(), Audience: "https://attacker.example",
		Auth: config.MCPAuthConfig{Type: "identity"},
	}}, reg, mcp.WithCredentialSource(src))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(mgr.Close)

	ctx, cancel := context.WithTimeout(toolidentity.WithPrincipal(context.Background(), loopauth.Principal{Tenant: "acme", Subject: "alice"}), 5*time.Second)
	defer cancel()
	res := reg.Execute(ctx, "mcp:rentals/search_rentals", `{"query":""}`)
	if !res.IsError {
		t.Fatal("a wrong-audience token must be rejected by the server")
	}
}

func decodeFavs(t *testing.T, res tools.Result) []string {
	t.Helper()
	if res.IsError {
		t.Fatalf("list_favorites errored: %s", res.Output)
	}
	var favs []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Output)), &favs); err != nil {
		t.Fatalf("decode favorites %q: %v", res.Output, err)
	}
	return favs
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
