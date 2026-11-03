// Package artifact is the tenant-scoped store seam for loop artifacts: spilled
// tool output and compaction segments. Artifacts are addressed by opaque
// art://<tenant>/<loop>/<kind>/<id> refs that never name a filesystem path, so a
// transcript stays portable across backends and processes.
//
// The package is agent-free on purpose: its only fuse dependency is
// internal/event, for TenantID and LoopID.
package artifact

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
)

// Kind classifies an artifact. Only the kinds declared here are valid.
type Kind string

const (
	KindSpill        Kind = "spill"
	KindSegment      Kind = "segment"
	KindSegmentIndex Kind = "segment-index"
)

// Known reports whether k is one of the declared kinds.
func (k Kind) Known() bool {
	switch k {
	case KindSpill, KindSegment, KindSegmentIndex:
		return true
	}
	return false
}

// Scheme prefixes every artifact ref's string form.
const Scheme = "art://"

// IsRef reports whether s is shaped like an artifact ref (it may still be invalid).
func IsRef(s string) bool { return strings.HasPrefix(s, Scheme) }

// MaxArtifactBytes is the per-artifact size cap every store enforces in Put.
const MaxArtifactBytes = 16 << 20

var (
	ErrNotFound      = errors.New("artifact: not found")
	ErrInvalidRef    = errors.New("artifact: invalid ref")
	ErrTooLarge      = errors.New("artifact: exceeds size cap")
	ErrQuotaExceeded = errors.New("artifact: loop quota exceeded")
)

// Ref is an opaque, tenant-scoped artifact address. Its String form is what a
// transcript carries; it never names a filesystem path.
type Ref struct {
	Tenant event.TenantID
	Loop   event.LoopID
	Kind   Kind
	ID     string
}

// String renders the ref as art://<tenant>/<loop>/<kind>/<id>.
func (r Ref) String() string {
	return Scheme + string(r.Tenant) + "/" + string(r.Loop) + "/" + string(r.Kind) + "/" + r.ID
}

// Validate checks a complete ref: every segment is a safe name and the kind is
// known. It does not normalize the tenant; stores do that before validating.
func (r Ref) Validate() error { return r.validate(false) }

// ValidateForPut is Validate, except that an empty ID is allowed because Put
// allocates one.
func (r Ref) ValidateForPut() error { return r.validate(true) }

func (r Ref) validate(allowEmptyID bool) error {
	if err := validSegment("tenant", string(r.Tenant)); err != nil {
		return err
	}
	if err := validSegment("loop", string(r.Loop)); err != nil {
		return err
	}
	if !r.Kind.Known() {
		return fmt.Errorf("%w: unknown kind %q", ErrInvalidRef, r.Kind)
	}
	if r.ID == "" && allowEmptyID {
		return nil
	}
	return validSegment("id", r.ID)
}

// ValidateScope checks a (tenant, loop, kind) listing scope.
func ValidateScope(tenant event.TenantID, loop event.LoopID, kind Kind) error {
	return Ref{Tenant: tenant, Loop: loop, Kind: kind}.ValidateForPut()
}

// validSegment enforces ^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$ and additionally
// refuses any ".." so a segment can never climb a directory.
func validSegment(what, s string) error {
	if s == "" {
		return fmt.Errorf("%w: empty %s", ErrInvalidRef, what)
	}
	if len(s) > 128 {
		return fmt.Errorf("%w: %s longer than 128 bytes", ErrInvalidRef, what)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_':
		case (c == '.' || c == '-') && i > 0:
		default:
			return fmt.Errorf("%w: %s %q has a disallowed character", ErrInvalidRef, what, s)
		}
	}
	if strings.Contains(s, "..") {
		return fmt.Errorf("%w: %s %q contains ..", ErrInvalidRef, what, s)
	}
	return nil
}

// ParseRef parses a complete art://<tenant>/<loop>/<kind>/<id> ref.
func ParseRef(s string) (Ref, error) {
	rest, ok := strings.CutPrefix(s, Scheme)
	if !ok {
		return Ref{}, fmt.Errorf("%w: missing %s scheme", ErrInvalidRef, Scheme)
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 4 {
		return Ref{}, fmt.Errorf("%w: want 4 segments, got %d", ErrInvalidRef, len(parts))
	}
	r := Ref{Tenant: event.TenantID(parts[0]), Loop: event.LoopID(parts[1]), Kind: Kind(parts[2]), ID: parts[3]}
	if err := r.Validate(); err != nil {
		return Ref{}, err
	}
	return r, nil
}

// ParsePrefix parses a ref prefix: tenant and loop are required, kind and id are
// optional (art://t/l, art://t/l/spill/, art://t/l/spill/x1). One trailing slash
// is tolerated. It serves directory-style listing of a loop's artifacts.
func ParsePrefix(s string) (Ref, error) {
	rest, ok := strings.CutPrefix(s, Scheme)
	if !ok {
		return Ref{}, fmt.Errorf("%w: missing %s scheme", ErrInvalidRef, Scheme)
	}
	rest = strings.TrimSuffix(rest, "/")
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || len(parts) > 4 {
		return Ref{}, fmt.Errorf("%w: want 2-4 segments, got %d", ErrInvalidRef, len(parts))
	}
	var r Ref
	r.Tenant = event.TenantID(parts[0])
	r.Loop = event.LoopID(parts[1])
	if err := validSegment("tenant", string(r.Tenant)); err != nil {
		return Ref{}, err
	}
	if err := validSegment("loop", string(r.Loop)); err != nil {
		return Ref{}, err
	}
	if len(parts) >= 3 {
		r.Kind = Kind(parts[2])
		if !r.Kind.Known() {
			return Ref{}, fmt.Errorf("%w: unknown kind %q", ErrInvalidRef, r.Kind)
		}
	}
	if len(parts) == 4 {
		r.ID = parts[3]
		if err := validSegment("id", r.ID); err != nil {
			return Ref{}, err
		}
	}
	return r, nil
}

// Meta describes a stored artifact.
type Meta struct {
	Ref       Ref               `json:"ref"`
	Size      int64             `json:"size"`       // uncompressed bytes
	CreatedAt time.Time         `json:"created_at"` // set by the store on every Put
	Tool      string            `json:"tool,omitempty"`
	Turn      int               `json:"turn,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// Store is blob-shaped on purpose so an object-storage backend can follow.
type Store interface {
	// Put writes r under ref. ref.ID == "" makes the store allocate a unique id (NewID).
	// A non-empty ID REPLACES any existing artifact under that ref. Bodies larger than
	// MaxArtifactBytes are refused with ErrTooLarge and nothing is written.
	Put(ctx context.Context, ref Ref, r io.Reader, meta Meta) (Meta, error)
	// Open returns the uncompressed body. A missing ref, or one under another tenant, is ErrNotFound.
	Open(ctx context.Context, ref Ref) (io.ReadCloser, Meta, error)
	// List returns the (tenant, loop, kind) artifacts ordered by CreatedAt, then ID.
	List(ctx context.Context, tenant event.TenantID, loop event.LoopID, kind Kind) ([]Meta, error)
}

// ReadCapped reads r fully, refusing with ErrTooLarge once it exceeds
// MaxArtifactBytes. Stores use it so an oversized body is never written.
func ReadCapped(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, MaxArtifactBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > MaxArtifactBytes {
		return nil, ErrTooLarge
	}
	return body, nil
}

var (
	idMu   sync.Mutex
	idLast int64
)

// NewID returns a lowercase, unique id that sorts ascending across successive
// calls in one process: a 16-hex-digit monotonic timestamp, then 8 random bytes.
func NewID() string {
	idMu.Lock()
	ts := time.Now().UnixNano()
	if ts <= idLast {
		ts = idLast + 1
	}
	idLast = ts
	idMu.Unlock()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("artifact: crypto/rand: " + err.Error())
	}
	return fmt.Sprintf("%016x-%s", ts, hex.EncodeToString(b[:]))
}
