// Package loopauth is the pluggable identity seam for the networked loop binding
// (change #49). It answers one question — "which principal does this bearer token
// name?" — behind a single Verifier interface, deliberately independent of how a
// loop runs or how a request arrives on the wire.
//
// The default implementation is StaticVerifier, a config-driven token→Principal
// map suitable for local development and small deployments. Richer verifiers
// (OIDC/JWT, mTLS, a secrets-backed token store) slot in behind the same Verifier
// interface without a re-cut of the callers.
//
// The seam is intentionally dependency-light: it imports ONLY internal/event and
// the standard library. Both the Connect edge (internal/loopconnect) and the
// composition root (cmd/fuse) may import it; internal/runtime never does. Keeping
// runtime and cmd/fuse out of this package's import graph is what lets the edge
// carry identity without leaking auth into the policy-free runtime seam (ADR-0030).
package loopauth

import (
	"context"
	"errors"

	"github.com/ethanhinson/fuse/internal/event"
)

// Principal is the authenticated identity a Verifier resolves a token to: the
// tenant isolation boundary the caller acts within and the authorization subject
// (the owner recorded on a loop). It carries no credentials — only identity.
type Principal struct {
	// Tenant is the isolation boundary every registry/store op is keyed by. The
	// empty tenant normalizes to event.DefaultTenant at the storage layer.
	Tenant event.TenantID
	// Subject is the authorization subject — the stable identity recorded as a
	// loop's Owner and compared on subsequent Send/Observe.
	Subject string
	// ObservabilityOperator authorizes process-global observability control
	// operations. Tenant principals do not receive it implicitly; trusted verifier
	// configuration must grant it.
	ObservabilityOperator bool
}

// Verifier resolves a bearer token to the Principal it names. It is the pluggable
// identity seam: the static map, OIDC/JWT, and mTLS impls all satisfy this one
// interface. Implementations must be safe for concurrent use.
type Verifier interface {
	// Verify returns the Principal named by token, or an error. An unknown or
	// empty token returns ErrInvalidToken (via errors.Is).
	Verify(ctx context.Context, token string) (Principal, error)
}

// ErrInvalidToken is returned by a Verifier for an unknown or missing/empty token.
// Callers match it with errors.Is; the Connect edge maps it to
// connect.CodeUnauthenticated.
var ErrInvalidToken = errors.New("loopauth: invalid or missing token")

// StaticVerifier resolves tokens against a fixed token→Principal map supplied at
// construction. It is the default Verifier for local development and small
// deployments. The map is read-only after construction, so Verify performs a plain
// (never-mutating) map read and is safe for concurrent use.
type StaticVerifier struct {
	tokens map[string]Principal
}

// NewStaticVerifier builds a StaticVerifier from a token→Principal map. The map is
// treated as read-only after this call; callers should not mutate it afterward.
func NewStaticVerifier(tokens map[string]Principal) *StaticVerifier {
	return &StaticVerifier{tokens: tokens}
}

// Verify returns the Principal mapped to token, or ErrInvalidToken for an unknown
// or empty token. It never mutates the underlying map.
func (v *StaticVerifier) Verify(_ context.Context, token string) (Principal, error) {
	if token == "" {
		return Principal{}, ErrInvalidToken
	}
	p, ok := v.tokens[token]
	if !ok {
		return Principal{}, ErrInvalidToken
	}
	return p, nil
}
