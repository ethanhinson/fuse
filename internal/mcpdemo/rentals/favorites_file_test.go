package rentals

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/event"
)

// newFileStore builds a durable store rooted at a fresh temp dir, failing the test
// on any construction error.
func newFileStore(t *testing.T, root string) FavoritesStore {
	t.Helper()
	store, err := NewFileFavorites(root)
	if err != nil {
		t.Fatalf("NewFileFavorites(%q): %v", root, err)
	}
	return store
}

// TestFileFavoritesContract runs the durable store through the SAME table the
// in-memory store satisfies — so compound-key isolation and add-idempotence coverage
// comes for free rather than being re-derived (and weakened) here.
func TestFileFavoritesContract(t *testing.T) {
	favoritesStoreContract(t, func(t *testing.T) FavoritesStore {
		return newFileStore(t, t.TempDir())
	})
}

// TestFileFavoritesSurvivesRestart is D4's pinned contract: favorites outlive the
// process. A SECOND store instance over the same root (the restart) must read back
// both principals' sets exactly, and they must stay disjoint across the restart.
func TestFileFavoritesSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	alice := PrincipalKey{Tenant: "acme", Subject: "alice"}
	bob := PrincipalKey{Tenant: "globex", Subject: "bob"}

	before := newFileStore(t, root)
	for _, id := range []string{"L1", "L2"} {
		if err := before.Add(alice, id); err != nil {
			t.Fatalf("Add(alice, %s): %v", id, err)
		}
	}
	if err := before.Add(bob, "L3"); err != nil {
		t.Fatalf("Add(bob, L3): %v", err)
	}

	// The restart: a brand-new instance, same root, no shared in-process state.
	after := newFileStore(t, root)
	assertFavorites(t, after, alice, "L1", "L2")
	assertFavorites(t, after, bob, "L3")

	// And a principal that favorited nothing still reads empty after the restart —
	// never a neighbour's set.
	assertFavorites(t, after, PrincipalKey{Tenant: "acme", Subject: "carol"})

	// Writes through the restarted instance accumulate onto the persisted set.
	if err := after.Add(alice, "L4"); err != nil {
		t.Fatalf("Add(alice, L4) after restart: %v", err)
	}
	assertFavorites(t, newFileStore(t, root), alice, "L1", "L2", "L4")
	assertFavorites(t, newFileStore(t, root), bob, "L3")
}

// TestFileFavoritesHostileKeysStayInsideRoot is the path-traversal guard. Tenants and
// subjects arrive from token claims, but the store must not trust them as path
// components: no hostile value may write outside the root, and no two distinct
// PrincipalKeys may collide onto one file.
func TestFileFavoritesHostileKeysStayInsideRoot(t *testing.T) {
	outer := t.TempDir()
	root := filepath.Join(outer, "favs")
	store := newFileStore(t, root)

	hostile := []struct {
		name    string
		pk      PrincipalKey
		listing string
	}{
		{"subject escapes upward", PrincipalKey{Tenant: "acme", Subject: "../../../../etc/passwd"}, "L1"},
		{"subject with separators", PrincipalKey{Tenant: "acme", Subject: "a/b/c"}, "L2"},
		{"subject is dotdot", PrincipalKey{Tenant: "acme", Subject: ".."}, "L3"},
		{"tenant escapes upward", PrincipalKey{Tenant: event.TenantID("../../etc"), Subject: "passwd"}, "L4"},
		{"tenant with separators", PrincipalKey{Tenant: event.TenantID("acme/../globex"), Subject: "alice"}, "L5"},
		{"absolute-looking subject", PrincipalKey{Tenant: "acme", Subject: "/etc/shadow"}, "L6"},
		{"empty key", PrincipalKey{}, "L7"},
		// Boundary-ambiguity pair: a naive tenant+separator+subject encoding would
		// hand these two DISTINCT principals the same file.
		{"split point A", PrincipalKey{Tenant: "a", Subject: "b:c"}, "L8"},
		{"split point B", PrincipalKey{Tenant: event.TenantID("a:b"), Subject: "c"}, "L9"},
	}

	for _, h := range hostile {
		if err := store.Add(h.pk, h.listing); err != nil {
			t.Fatalf("%s: Add(%+v): %v", h.name, h.pk, err)
		}
	}

	// Each hostile principal sees exactly its own listing — no collisions.
	for _, h := range hostile {
		assertFavorites(t, store, h.pk, h.listing)
	}
	// The benign principal a hostile tenant tried to impersonate is untouched.
	assertFavorites(t, store, PrincipalKey{Tenant: "globex", Subject: "alice"})
	assertFavorites(t, store, PrincipalKey{Tenant: "acme", Subject: "passwd"})

	// Nothing was written outside the root: the only entry under outer/ is root
	// itself, and every file under root is a flat, opaque, fixed-shape name.
	entries, err := readDirNames(outer)
	if err != nil {
		t.Fatalf("read outer: %v", err)
	}
	if len(entries) != 1 || entries[0] != "favs" {
		t.Fatalf("entries outside the root: %v, want [favs] only", entries)
	}
	opaque := regexp.MustCompile(`^[0-9a-f]{64}\.json$`)
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		if d.IsDir() {
			t.Errorf("unexpected subdirectory under root: %s", path)
			return nil
		}
		if !opaque.MatchString(d.Name()) {
			t.Errorf("non-opaque favorites file name %q (hostile key leaked into the path)", d.Name())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk root: %v", err)
	}

	// A restart still reads every hostile principal's set back correctly.
	restarted := newFileStore(t, root)
	for _, h := range hostile {
		assertFavorites(t, restarted, h.pk, h.listing)
	}
}

// TestFileFavoritesConcurrentAddsDoNotLoseWrites exercises the read-modify-write
// under the mutex: concurrent Adds for the same principal must all land (no
// lost update), and concurrent Adds across principals must not bleed.
func TestFileFavoritesConcurrentAddsDoNotLoseWrites(t *testing.T) {
	root := t.TempDir()
	store := newFileStore(t, root)
	alice := PrincipalKey{Tenant: "acme", Subject: "alice"}
	bob := PrincipalKey{Tenant: "acme", Subject: "bob"}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			if err := store.Add(alice, "A"+strconv.Itoa(i)); err != nil {
				t.Errorf("concurrent Add(alice): %v", err)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			if err := store.Add(bob, "B"+strconv.Itoa(i)); err != nil {
				t.Errorf("concurrent Add(bob): %v", err)
			}
		}(i)
		// Concurrent readers race the writers.
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := store.List(alice); err != nil {
				t.Errorf("concurrent List(alice): %v", err)
			}
		}()
	}
	wg.Wait()

	wantAlice := make([]string, 0, 8)
	wantBob := make([]string, 0, 8)
	for i := 0; i < 8; i++ {
		wantAlice = append(wantAlice, "A"+strconv.Itoa(i))
		wantBob = append(wantBob, "B"+strconv.Itoa(i))
	}
	assertFavorites(t, newFileStore(t, root), alice, wantAlice...)
	assertFavorites(t, newFileStore(t, root), bob, wantBob...)
}

// readDirNames lists every entry in dir, dotfiles included — a leaked temp file must
// not be able to hide from the containment assertion.
func readDirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// assertFavorites asserts store.List(pk) equals want as a set (order-independent:
// List's order is deliberately unspecified).
func assertFavorites(t *testing.T, store FavoritesStore, pk PrincipalKey, want ...string) {
	t.Helper()
	got, err := store.List(pk)
	if err != nil {
		t.Fatalf("List(%+v): %v", pk, err)
	}
	gotSorted := append([]string(nil), got...)
	sort.Strings(gotSorted)
	wantSorted := append([]string(nil), want...)
	sort.Strings(wantSorted)
	if strings.Join(gotSorted, ",") != strings.Join(wantSorted, ",") {
		t.Fatalf("List(%+v) = %v, want %v", pk, gotSorted, wantSorted)
	}
}
