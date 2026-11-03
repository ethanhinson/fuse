package artifact

import (
	"bytes"
	"context"
	"io"
	"maps"
	"sort"
	"sync"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
)

// DefaultMemLoopCap is the per-loop byte budget the runtime gives its default
// in-memory store.
const DefaultMemLoopCap = 64 << 20

// Option configures a MemStore.
type Option func(*MemStore)

// WithPerLoopCap caps the total uncompressed bytes one (tenant, loop) may hold.
// A Put that would exceed it fails with ErrQuotaExceeded. n <= 0 means no cap.
func WithPerLoopCap(n int64) Option { return func(s *MemStore) { s.loopCap = n } }

type memEntry struct {
	body []byte
	meta Meta
}

type loopKey struct {
	tenant event.TenantID
	loop   event.LoopID
}

// MemStore is an in-process Store. Bodies are held uncompressed.
type MemStore struct {
	mu      sync.Mutex
	entries map[Ref]memEntry
	used    map[loopKey]int64
	loopCap int64
}

// NewMemStore returns an empty in-memory store.
func NewMemStore(opts ...Option) *MemStore {
	s := &MemStore{entries: map[Ref]memEntry{}, used: map[loopKey]int64{}}
	for _, o := range opts {
		o(s)
	}
	return s
}

var _ Store = (*MemStore)(nil)

func (s *MemStore) Put(ctx context.Context, ref Ref, r io.Reader, meta Meta) (Meta, error) {
	ref.Tenant = event.NormalizeTenant(ref.Tenant)
	if err := ref.ValidateForPut(); err != nil {
		return Meta{}, err
	}
	if err := ctx.Err(); err != nil {
		return Meta{}, err
	}
	body, err := ReadCapped(r)
	if err != nil {
		return Meta{}, err
	}
	if ref.ID == "" {
		ref.ID = NewID()
	}
	meta.Ref = ref
	meta.Size = int64(len(body))
	meta.CreatedAt = time.Now()
	meta.Labels = maps.Clone(meta.Labels)

	s.mu.Lock()
	defer s.mu.Unlock()
	k := loopKey{ref.Tenant, ref.Loop}
	used := s.used[k]
	if old, ok := s.entries[ref]; ok {
		used -= old.meta.Size
	}
	if s.loopCap > 0 && used+meta.Size > s.loopCap {
		return Meta{}, ErrQuotaExceeded
	}
	s.used[k] = used + meta.Size
	s.entries[ref] = memEntry{body: body, meta: meta}
	return cloneMeta(meta), nil
}

func (s *MemStore) Open(ctx context.Context, ref Ref) (io.ReadCloser, Meta, error) {
	ref.Tenant = event.NormalizeTenant(ref.Tenant)
	if err := ref.Validate(); err != nil {
		return nil, Meta{}, err
	}
	s.mu.Lock()
	e, ok := s.entries[ref]
	s.mu.Unlock()
	if !ok {
		return nil, Meta{}, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(e.body)), cloneMeta(e.meta), nil
}

func (s *MemStore) List(ctx context.Context, tenant event.TenantID, loop event.LoopID, kind Kind) ([]Meta, error) {
	tenant = event.NormalizeTenant(tenant)
	if err := ValidateScope(tenant, loop, kind); err != nil {
		return nil, err
	}
	s.mu.Lock()
	var out []Meta
	for ref, e := range s.entries {
		if ref.Tenant == tenant && ref.Loop == loop && ref.Kind == kind {
			out = append(out, cloneMeta(e.meta))
		}
	}
	s.mu.Unlock()
	SortMetas(out)
	return out, nil
}

// SortMetas orders metas by CreatedAt, then ID — the order List promises.
func SortMetas(ms []Meta) {
	sort.Slice(ms, func(i, j int) bool {
		if !ms[i].CreatedAt.Equal(ms[j].CreatedAt) {
			return ms[i].CreatedAt.Before(ms[j].CreatedAt)
		}
		return ms[i].Ref.ID < ms[j].Ref.ID
	})
}

func cloneMeta(m Meta) Meta {
	m.Labels = maps.Clone(m.Labels)
	return m
}
