package rentals

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/ethanhinson/fuse/internal/event"
)

// fileFavoritesMode is the permission on both the per-principal files and their
// temp siblings: one principal's favorites are not world-readable.
const fileFavoritesMode os.FileMode = 0o600

// favoritesRecord is the on-disk shape of one principal's set. Tenant and Subject are
// stored ALONGSIDE the listings so a read can re-assert the full compound key it
// matched (see the compound-key note on PrincipalKey) rather than trusting the file
// name alone — a mismatch is treated as a read failure, never as a hit.
type favoritesRecord struct {
	Tenant   event.TenantID `json:"tenant"`
	Subject  string         `json:"subject"`
	Listings []string       `json:"listings"`
}

// fileFavorites is the durable FavoritesStore: one JSON file per PrincipalKey under a
// caller-supplied root, written atomically (temp file + rename) with a mutex around
// the whole read-modify-write. It is opted into by the runnable demo
// (cmd/rentals-mcp); the test lane keeps NewMemoryFavorites.
//
// Path safety: neither the tenant nor the subject is ever used as a path component.
// Both arrive from token claims and may contain "/", "..", or a leading "/"; the file
// name is the hex SHA-256 of an UNAMBIGUOUS (length-prefixed) encoding of the pair, so
// a hostile value can neither escape the root nor collide two distinct principals onto
// one file.
//
// The mutex serializes writers within one process. Two processes sharing a root is out
// of scope for the demo (last writer wins on the rename); each rentals-mcp instance
// owns its favorites directory.
type fileFavorites struct {
	root string

	mu sync.Mutex
}

// NewFileFavorites returns a durable FavoritesStore persisting under root, creating
// root if needed. Tenant values reaching the store are already normalized by the
// server (event.NormalizeTenant at key-registration time); the store keys on what it
// is handed and deliberately does NOT re-normalize, so it can never disagree with the
// in-memory store about which principal a key names.
func NewFileFavorites(root string) (FavoritesStore, error) {
	if root == "" {
		return nil, fmt.Errorf("rentals: favorites root directory is empty")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("rentals: create favorites root %q: %w", root, err)
	}
	return &fileFavorites{root: filepath.Clean(root)}, nil
}

// keyPath maps a PrincipalKey to its file INSIDE root. The digest input is
// length-prefixed so the tenant/subject boundary is unambiguous: {"a","b:c"} and
// {"a:b","c"} hash differently, where a naive join would fuse them. The result is a
// fixed 64-hex-char name, so no caller-controlled byte ever reaches the path.
func (f *fileFavorites) keyPath(pk PrincipalKey) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d:%s|%d:%s", len(pk.Tenant), pk.Tenant, len(pk.Subject), pk.Subject)
	return filepath.Join(f.root, hex.EncodeToString(h.Sum(nil))+".json")
}

// load reads pk's record. A missing file is an empty set, not an error. A record whose
// stored identity does not match pk is refused outright (fail closed): returning it
// would hand this caller some other principal's favorites.
func (f *fileFavorites) load(pk PrincipalKey) ([]string, error) {
	data, err := os.ReadFile(f.keyPath(pk))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("rentals: read favorites: %w", err)
	}
	var rec favoritesRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("rentals: decode favorites: %w", err)
	}
	// Re-assert the WHOLE compound key on every hit.
	if rec.Tenant != pk.Tenant || rec.Subject != pk.Subject {
		return nil, fmt.Errorf("rentals: favorites record identity mismatch (stored tenant/subject is not the caller's)")
	}
	return rec.Listings, nil
}

// store writes pk's record atomically: a temp file in the SAME directory (so the
// rename cannot cross filesystems), fsynced, then renamed over the target. A reader
// therefore sees either the previous complete set or the new one, never a torn write.
func (f *fileFavorites) store(pk PrincipalKey, listings []string) error {
	rec := favoritesRecord{Tenant: pk.Tenant, Subject: pk.Subject, Listings: listings}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("rentals: encode favorites: %w", err)
	}

	path := f.keyPath(pk)
	tmp, err := os.CreateTemp(f.root, ".favorites-*.tmp")
	if err != nil {
		return fmt.Errorf("rentals: create favorites temp file: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup on any failure before the rename claims the file.
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(fileFavoritesMode); err != nil {
		tmp.Close()
		return fmt.Errorf("rentals: chmod favorites temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("rentals: write favorites: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("rentals: sync favorites: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("rentals: close favorites temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rentals: commit favorites: %w", err)
	}
	tmpName = "" // renamed away; nothing to clean up
	return nil
}

// Add records listingID in pk's set, idempotently. The mutex spans the whole
// read-modify-write so concurrent Adds cannot lose each other's updates.
func (f *fileFavorites) Add(pk PrincipalKey, listingID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	listings, err := f.load(pk)
	if err != nil {
		return err
	}
	for _, id := range listings {
		if id == listingID {
			return nil // already present
		}
	}
	listings = append(listings, listingID)
	sort.Strings(listings) // deterministic on disk; callers must not depend on order
	return f.store(pk, listings)
}

// List returns pk's listing ids, empty for a principal that has favorited nothing.
// Order is sorted here, but the interface leaves it unspecified — do not build callers
// that depend on it.
func (f *fileFavorites) List(pk PrincipalKey) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	listings, err := f.load(pk)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(listings))
	copy(out, listings)
	return out, nil
}
