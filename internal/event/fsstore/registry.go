package fsstore

// registry.go adds the filesystem-backed event.LoopRegistry to FSDurableStore
// (change 0047). A loop's existence/ownership/liveness persists as a small JSON
// sidecar at <baseDir>/<tenant>/<loop>/loop.json alongside its events.jsonl, so a
// FRESH process (empty in-memory map) can Resolve a loop a prior process started —
// the durable-registry half of the cold-reattach fix. Writes are atomic
// (temp+rename) under the store mutex; List enumerates a tenant's loop dirs and is
// robust to non-loop.json entries and symlinks (dirent-isdir-skips-symlinks guard:
// stat the sidecar rather than trusting the dirent type).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
)

// compile-time assertion that FSDurableStore is a durable loop registry.
var _ event.LoopRegistry = (*FSDurableStore)(nil)

// loopSidecar is the on-disk shape of loop.json. Times are RFC3339 via the standard
// time JSON encoding.
type loopSidecar struct {
	OwnerNodeID string    `json:"owner_node_id"`
	Owner       string    `json:"owner"`
	Live        bool      `json:"live"`
	LeaseExpiry time.Time `json:"lease_expiry"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (s *FSDurableStore) sidecarPath(k event.StreamKey) string {
	return filepath.Join(s.streamDir(k), "loop.json")
}

// readSidecar reads and decodes a loop.json (caller need not hold the mutex for a
// pure read, but registry mutations take s.mu to serialize read-modify-write).
// Returns (rec, false, nil) when the sidecar is absent.
func readSidecar(path string) (loopSidecar, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return loopSidecar{}, false, nil
		}
		return loopSidecar{}, false, err
	}
	var sc loopSidecar
	if err := json.Unmarshal(b, &sc); err != nil {
		return loopSidecar{}, false, err
	}
	return sc, true, nil
}

// writeSidecar atomically writes sc to path (temp file in the same dir + rename).
func writeSidecar(path string, sc loopSidecar) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(sc)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "loop-*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// Register durably records a loop's existence (idempotent upsert keyed by rec.Key).
// CreatedAt is preserved from the first registration; UpdatedAt always advances.
func (s *FSDurableStore) Register(ctx context.Context, rec event.LoopRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.sidecarPath(rec.Key)
	now := nowUTC()
	sc := loopSidecar{
		OwnerNodeID: rec.OwnerNodeID,
		Owner:       rec.Owner,
		Live:        rec.Live,
		LeaseExpiry: rec.LeaseExpiry,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if prev, ok, err := readSidecar(path); err != nil {
		return err
	} else if ok {
		sc.CreatedAt = prev.CreatedAt // preserve original creation time on upsert
	}
	return writeSidecar(path, sc)
}

// SetLive flips the loop's liveness and records the owning node, preserving
// CreatedAt. It is a no-op error (ErrLoopUnknown) if the loop was never registered.
func (s *FSDurableStore) SetLive(ctx context.Context, key event.StreamKey, live bool, ownerNodeID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.sidecarPath(key)
	prev, ok, err := readSidecar(path)
	if err != nil {
		return err
	}
	if !ok {
		return event.ErrLoopUnknown
	}
	prev.Live = live
	prev.OwnerNodeID = ownerNodeID
	prev.UpdatedAt = nowUTC()
	return writeSidecar(path, prev)
}

// Resolve returns the loop's record, or ErrLoopUnknown if no sidecar exists for the
// (tenant, loop) — including a resolve under the wrong tenant.
func (s *FSDurableStore) Resolve(ctx context.Context, key event.StreamKey) (event.LoopRecord, error) {
	if err := ctx.Err(); err != nil {
		return event.LoopRecord{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, ok, err := readSidecar(s.sidecarPath(key))
	if err != nil {
		return event.LoopRecord{}, err
	}
	if !ok {
		return event.LoopRecord{}, event.ErrLoopUnknown
	}
	return event.LoopRecord{
		Key:         normalizeKey(key),
		OwnerNodeID: sc.OwnerNodeID,
		Owner:       sc.Owner,
		Live:        sc.Live,
		LeaseExpiry: sc.LeaseExpiry,
		CreatedAt:   sc.CreatedAt,
		UpdatedAt:   sc.UpdatedAt,
	}, nil
}

// Heartbeat renews the owner-liveness lease for a registered loop: it advances
// LeaseExpiry to expiry and UpdatedAt to now, recording the renewing node, while
// leaving Owner and Live untouched. It returns ErrLoopUnknown if no sidecar exists.
func (s *FSDurableStore) Heartbeat(ctx context.Context, key event.StreamKey, ownerNodeID string, expiry time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.sidecarPath(key)
	prev, ok, err := readSidecar(path)
	if err != nil {
		return err
	}
	if !ok {
		return event.ErrLoopUnknown
	}
	prev.OwnerNodeID = ownerNodeID
	prev.LeaseExpiry = expiry
	prev.UpdatedAt = nowUTC()
	return writeSidecar(path, prev)
}

// List returns all loop records for a tenant by enumerating
// <baseDir>/<tenant>/*/loop.json. It is tenant-scoped and robust to non-directory
// and symlink entries (dirent-isdir-skips-symlinks guard: stat the sidecar rather
// than trusting the dirent type).
func (s *FSDurableStore) List(ctx context.Context, tenant event.TenantID) ([]event.LoopRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	nt := event.NormalizeTenant(tenant)
	tenantDir := filepath.Join(s.baseDir, string(nt))
	entries, err := os.ReadDir(tenantDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []event.LoopRecord
	for _, ent := range entries {
		// Don't trust ent.IsDir(): a symlink to a dir reports as non-dir. Probe the
		// sidecar directly and skip entries without one.
		loopName := ent.Name()
		path := filepath.Join(tenantDir, loopName, "loop.json")
		sc, ok, rerr := readSidecar(path)
		if rerr != nil {
			return out, rerr
		}
		if !ok {
			continue
		}
		out = append(out, event.LoopRecord{
			Key:         event.StreamKey{Tenant: nt, Loop: event.LoopID(loopName)},
			OwnerNodeID: sc.OwnerNodeID,
			Owner:       sc.Owner,
			Live:        sc.Live,
			LeaseExpiry: sc.LeaseExpiry,
			CreatedAt:   sc.CreatedAt,
			UpdatedAt:   sc.UpdatedAt,
		})
	}
	return out, nil
}
