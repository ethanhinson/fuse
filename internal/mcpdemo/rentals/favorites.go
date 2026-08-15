package rentals

import (
	"sync"

	"github.com/ethanhinson/fuse/internal/event"
)

// PrincipalKey is the per-principal favorites-store key: the token identity
// (tenant + subject), NEVER a client-supplied argument. It is the confused-deputy
// boundary — a write always lands in the CALLING principal's set.
//
// The key is COMPOUND and must stay that way in every implementation. Subjects are only
// unique within a tenant, so a store keyed by Subject alone (or by any inner id alone)
// hands one tenant's caller another tenant's favorites. Key by the whole PrincipalKey,
// or re-assert both fields on every hit.
type PrincipalKey struct {
	Tenant  event.TenantID
	Subject string
}

// FavoritesStore is the favorites persistence seam. The default is NewMemoryFavorites
// (the CI-lane default); the durable filesystem store, NewFileFavorites
// (favorites_file.go), implements the same interface and is opted into by the runnable
// demo, never by the test lane.
//
// Implementations must be safe for concurrent use, must treat Add as idempotent per
// (principal, listing), and must partition strictly by the full PrincipalKey.
type FavoritesStore interface {
	// Add records listingID in pk's set. Adding the same listing twice is a no-op.
	Add(pk PrincipalKey, listingID string) error
	// List returns pk's listing ids in unspecified order, empty (never another
	// principal's set) for a principal that has favorited nothing.
	List(pk PrincipalKey) ([]string, error)
}

// memoryFavorites is the in-process store: a map keyed by the WHOLE compound
// PrincipalKey, guarded by its own mutex. It is the historical behaviour of the
// server's favorites map, lifted behind the seam unchanged.
type memoryFavorites struct {
	mu   sync.Mutex
	favs map[PrincipalKey]map[string]bool // principal -> set of listing ids
}

// NewMemoryFavorites returns an empty in-memory FavoritesStore (the CI-lane default:
// per-process, reset with the server, no durability).
func NewMemoryFavorites() FavoritesStore {
	return &memoryFavorites{favs: map[PrincipalKey]map[string]bool{}}
}

func (m *memoryFavorites) Add(pk PrincipalKey, listingID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	set := m.favs[pk]
	if set == nil {
		set = map[string]bool{}
		m.favs[pk] = set
	}
	set[listingID] = true // idempotent per principal
	return nil
}

func (m *memoryFavorites) List(pk PrincipalKey) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	set := m.favs[pk]
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	return out, nil
}
