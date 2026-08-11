package toolidentity

import (
	"context"

	"github.com/ethanhinson/fuse/internal/loopauth"
)

// principalCtxKey is the private context key under which the authenticated loop
// initiator's Principal is threaded from loop-start to the tool-call egress. It
// is unexported so ONLY this package can set or read it — the identity can never
// be forged by untrusted code (model output, a foreign string key), which is the
// root-of-trust constraint (the principal originates from the authenticated
// loop-start context, never from anything the model emits).
//
// This carrier is distinct from internal/loopconnect's edge-only principal key:
// this one is readable by the MCP egress (internal/mcp) without importing the
// Connect edge, and it imports only internal/loopauth — keeping internal/runtime
// out of the auth import graph (ADR-0030).
type principalCtxKey struct{}

// WithPrincipal returns a child context carrying p, threaded from the
// authenticated loop-start context down to the tool-call egress. The composition
// root (cmd/fuse) seeds it once per loop from the Principal the Connect edge
// resolved (or the single explicit local principal on the CLI paths).
func WithPrincipal(ctx context.Context, p loopauth.Principal) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, p)
}

// PrincipalFrom returns the Principal WithPrincipal stashed on ctx, and ok=false
// when none is present. It is the only supported way to read the propagated
// identity at the egress seam; the context key itself is deliberately private.
// A bare context yields the zero Principal and ok=false — the egress seam treats
// that as "no identity" and fails closed for an OAuth target rather than minting
// under an empty/spoofable identity.
func PrincipalFrom(ctx context.Context) (loopauth.Principal, bool) {
	p, ok := ctx.Value(principalCtxKey{}).(loopauth.Principal)
	return p, ok
}
