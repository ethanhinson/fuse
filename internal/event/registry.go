package event

import (
	"context"
	"errors"
	"time"
)

// This file holds the durable loop-registry contract (change 0047): the seam that
// records a loop's *very existence*, ownership, and liveness so a fresh process —
// with an empty in-memory map — can discover and reattach to a loop started by a
// prior process or another instance. It is types + the shared error only; no
// backend logic lives here (fsstore and pgstore each implement LoopRegistry).

// ErrLoopUnknown is returned by LoopRegistry.Resolve (and surfaced by durable
// Replay/Observe paths) when a (tenant, loop) has no registry record. It is a
// distinguishable, cross-process sentinel: a caller distinguishes "loop never
// existed / wrong tenant" from a transport or backend error.
var ErrLoopUnknown = errors.New("event: loop not found in registry")

// LoopRecord is the durable existence/ownership/liveness record for one loop. It
// is what a cold instance reads to decide whether a loop exists, who owns it, and
// whether it is still live (accepting Sends) versus finished (replay-only).
type LoopRecord struct {
	Key         StreamKey
	OwnerNodeID string
	Live        bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// LoopRegistry is the durable, tenant-scoped registry of loop existence and
// liveness. It reconciles the in-process loop map (a per-instance cache) with a
// shared source of truth so cross-process/cross-instance resolution works. When a
// backend also implements DurableStore they may be the same value sharing storage.
type LoopRegistry interface {
	// Register durably records a loop's existence (idempotent upsert keyed by
	// rec.Key). CreatedAt is set on first registration; UpdatedAt advances.
	Register(ctx context.Context, rec LoopRecord) error

	// SetLive flips the loop's liveness and records the owning node. A live loop
	// accepts Sends on its owner; a not-live loop is replay-only.
	SetLive(ctx context.Context, key StreamKey, live bool, ownerNodeID string) error

	// Resolve returns the loop's record, or ErrLoopUnknown if no record exists for
	// the (tenant, loop) — including a resolve under the wrong tenant.
	Resolve(ctx context.Context, key StreamKey) (LoopRecord, error)

	// List returns all loop records for a tenant (tenant-scoped; never another
	// tenant's loops).
	List(ctx context.Context, tenant TenantID) ([]LoopRecord, error)
}
