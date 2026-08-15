package rentals

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
	"github.com/ethanhinson/fuse/internal/mcp"
	"github.com/ethanhinson/fuse/internal/toolidentity"
	"github.com/ethanhinson/fuse/internal/tools"
)

// favoritesStoreContract is the behaviour every FavoritesStore implementation must
// satisfy. It is a table over the INTERFACE (not the memory type) so the durable
// implementation can be run through the identical cases.
//
// The third case is the compound-key regression: a store keyed by the inner subject
// alone would hand tenant B's caller tenant A's favorites.
func favoritesStoreContract(t *testing.T, newStore func(t *testing.T) FavoritesStore) {
	t.Helper()

	alice := PrincipalKey{Tenant: "acme", Subject: "alice"}
	bob := PrincipalKey{Tenant: "acme", Subject: "bob"}
	aliceElsewhere := PrincipalKey{Tenant: "globex", Subject: "alice"} // SAME subject, other tenant

	type add struct {
		pk        PrincipalKey
		listingID string
	}
	cases := []struct {
		name string
		adds []add
		want map[PrincipalKey][]string
	}{
		{
			name: "add is idempotent per principal",
			adds: []add{{alice, "L1"}, {alice, "L1"}, {alice, "L2"}, {alice, "L1"}},
			want: map[PrincipalKey][]string{alice: {"L1", "L2"}},
		},
		{
			name: "two subjects in one tenant are isolated",
			adds: []add{{alice, "L1"}, {bob, "L2"}},
			want: map[PrincipalKey][]string{alice: {"L1"}, bob: {"L2"}},
		},
		{
			name: "same subject under two tenants is isolated (compound key)",
			adds: []add{{alice, "L1"}, {aliceElsewhere, "L2"}},
			want: map[PrincipalKey][]string{alice: {"L1"}, aliceElsewhere: {"L2"}},
		},
		{
			name: "unknown principal reads empty, never another principal's set",
			adds: []add{{alice, "L1"}},
			want: map[PrincipalKey][]string{bob: {}, aliceElsewhere: {}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			for _, a := range tc.adds {
				if err := store.Add(a.pk, a.listingID); err != nil {
					t.Fatalf("Add(%+v, %q): %v", a.pk, a.listingID, err)
				}
			}
			for pk, want := range tc.want {
				got, err := store.List(pk)
				if err != nil {
					t.Fatalf("List(%+v): %v", pk, err)
				}
				sort.Strings(got)
				if len(got) != len(want) {
					t.Fatalf("List(%+v) = %v, want %v", pk, got, want)
				}
				for i := range want {
					if got[i] != want[i] {
						t.Fatalf("List(%+v) = %v, want %v", pk, got, want)
					}
				}
			}
		})
	}
}

func TestMemoryFavoritesContract(t *testing.T) {
	favoritesStoreContract(t, func(t *testing.T) FavoritesStore { return NewMemoryFavorites() })
}

// TestConfigFavoritesSeamIsUsedAndKeyedByToken drives the real MCP wire and asserts the
// seeded Config.Favorites store is the one the tool handlers write through, keyed by the
// VERIFIED TOKEN's {tenant, subject} — never by a client-supplied argument. Two callers
// share the subject string "alice" under different tenants; their sets must stay disjoint.
func TestConfigFavoritesSeamIsUsedAndKeyedByToken(t *testing.T) {
	store := NewMemoryFavorites()
	keys := map[event.TenantID][]byte{"acme": []byte("k-acme"), "globex": []byte("k-globex")}
	reg, _, ctxFor := harnessWithConfig(t, Config{
		Audience:   testAudience,
		TenantKeys: keys,
		Favorites:  store,
	})

	call := func(subject string, tenant event.TenantID, tool, args string) tools.Result {
		t.Helper()
		ctx, cancel := context.WithTimeout(ctxFor(subject, tenant), 5*time.Second)
		defer cancel()
		return reg.Execute(ctx, "mcp:rentals/"+tool, args)
	}

	if res := call("alice", "acme", "favorite_listing", `{"listing_id":"L1"}`); res.IsError {
		t.Fatalf("acme/alice favorite: %s", res.Output)
	}
	if res := call("alice", "globex", "favorite_listing", `{"listing_id":"L2"}`); res.IsError {
		t.Fatalf("globex/alice favorite: %s", res.Output)
	}

	// The injected store saw the writes, keyed by the compound token identity.
	acme, err := store.List(PrincipalKey{Tenant: "acme", Subject: "alice"})
	if err != nil {
		t.Fatalf("List acme/alice: %v", err)
	}
	globex, err := store.List(PrincipalKey{Tenant: "globex", Subject: "alice"})
	if err != nil {
		t.Fatalf("List globex/alice: %v", err)
	}
	if !contains(acme, "L1") || contains(acme, "L2") {
		t.Fatalf("acme/alice = %v, want [L1] only", acme)
	}
	if !contains(globex, "L2") || contains(globex, "L1") {
		t.Fatalf("globex/alice = %v, want [L2] only", globex)
	}

	// And the wire read agrees: same subject string, different tenant, disjoint sets.
	if got := decodeFavs(t, call("alice", "acme", "list_favorites", `{}`)); !contains(got, "L1") || contains(got, "L2") {
		t.Fatalf("acme/alice list_favorites = %v, want [L1] only", got)
	}
	if got := decodeFavs(t, call("alice", "globex", "list_favorites", `{}`)); !contains(got, "L2") || contains(got, "L1") {
		t.Fatalf("globex/alice list_favorites = %v, want [L2] only", got)
	}
}

// TestNilConfigFavoritesDefaultsToMemory pins the CI default: an unset Config.Favorites
// yields the in-memory store, so #59's acceptance lane needs no wiring.
func TestNilConfigFavoritesDefaultsToMemory(t *testing.T) {
	s, _ := NewHandler(Config{Audience: testAudience})
	pk := PrincipalKey{Tenant: "acme", Subject: "alice"}
	if err := s.addFavorite(pk, "L1"); err != nil {
		t.Fatalf("addFavorite: %v", err)
	}
	got, err := s.listFavorites(pk)
	if err != nil {
		t.Fatalf("listFavorites: %v", err)
	}
	if len(got) != 1 || got[0] != "L1" {
		t.Fatalf("favorites = %v, want [L1]", got)
	}
}

// harnessWithConfig stands up a rentals server from an arbitrary Config plus a real fuse
// MCP client over the wire. Teardown note (see the package doc): handleSSE blocks on
// r.Context().Done(), so srv.Close must be registered BEFORE the client's Close — LIFO
// then stops the client's read pump first. Never `defer srv.Close()`.
func harnessWithConfig(t *testing.T, cfg Config) (*tools.Registry, *Server, func(subject string, tenant event.TenantID) context.Context) {
	t.Helper()
	srv := NewServer(cfg)
	t.Cleanup(srv.Close)

	sts, err := toolidentity.NewBuiltinSTS(toolidentity.BuiltinSTSConfig{Issuer: "fuse", TTL: time.Minute, TenantKeys: cfg.TenantKeys})
	if err != nil {
		t.Fatalf("NewBuiltinSTS: %v", err)
	}
	src := toolidentity.NewBroker(sts, nil, nil)

	reg := tools.NewRegistry()
	mgr, err := mcp.NewManager([]config.MCPServerConfig{{
		Name:      "rentals",
		Transport: "sse",
		URL:       srv.URL(),
		Audience:  cfg.Audience,
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
