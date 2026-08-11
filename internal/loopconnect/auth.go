package loopconnect

import (
	"context"
	"strings"

	"connectrpc.com/connect"

	"github.com/ethanhinson/fuse/internal/loopauth"
)

// principalCtxKey is the private context key under which the auth interceptor
// stashes the verified Principal. It is unexported so only this package can set
// or read it — callers reach the Principal exclusively via PrincipalFrom.
type principalCtxKey struct{}

// PrincipalFrom returns the Principal the auth interceptor stashed on ctx, and
// ok=false when none is present (an un-intercepted or unauthenticated path). It
// is the only supported way to read the authenticated identity off a request
// context; the context key itself is deliberately private.
func PrincipalFrom(ctx context.Context) (loopauth.Principal, bool) {
	p, ok := ctx.Value(principalCtxKey{}).(loopauth.Principal)
	return p, ok
}

// withPrincipal returns a child context carrying p under the private key.
func withPrincipal(ctx context.Context, p loopauth.Principal) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, p)
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" header
// value, matching the "Bearer " scheme case-insensitively. It returns "" when
// the header is missing or is not a Bearer credential — the empty string flows
// into Verify, which maps it to loopauth.ErrInvalidToken (→ CodeUnauthenticated),
// so missing and malformed headers fail closed through the same path.
func bearerToken(authorization string) string {
	const prefix = "bearer "
	if len(authorization) < len(prefix) {
		return ""
	}
	if !strings.EqualFold(authorization[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(authorization[len(prefix):])
}

// authInterceptor is a connect.Interceptor that authenticates every unary and
// streaming request at the transport edge: it reads the bearer token, resolves
// it to a Principal via the Verifier, short-circuits with CodeUnauthenticated on
// any failure (never invoking the handler), and on success stashes the Principal
// on the context handed downstream.
type authInterceptor struct {
	v loopauth.Verifier
}

// NewAuthInterceptor builds a connect.Interceptor that verifies the Authorization
// bearer token on every unary and streaming-handler request against v, placing the
// resolved Principal on the downstream context (read via PrincipalFrom). Requests
// with a missing or invalid token are rejected with CodeUnauthenticated before the
// handler runs. As a server-side interceptor its streaming-client leg is a no-op.
func NewAuthInterceptor(v loopauth.Verifier) connect.Interceptor {
	return authInterceptor{v: v}
}

// WrapUnary authenticates a unary request, then calls next with the Principal on
// the context. A verification failure short-circuits before next runs.
func (i authInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		token := bearerToken(req.Header().Get("Authorization"))
		p, err := i.v.Verify(ctx, token)
		if err != nil {
			return nil, connect.NewError(connect.CodeUnauthenticated, err)
		}
		return next(withPrincipal(ctx, p), req)
	}
}

// WrapStreamingClient is a pass-through: this is a server-side interceptor, so it
// adds no behavior to outbound client streams.
func (i authInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler authenticates a streaming request from its request headers,
// then calls next with the Principal on the context. A verification failure short-
// circuits before next runs.
func (i authInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		token := bearerToken(conn.RequestHeader().Get("Authorization"))
		p, err := i.v.Verify(ctx, token)
		if err != nil {
			return connect.NewError(connect.CodeUnauthenticated, err)
		}
		return next(withPrincipal(ctx, p), conn)
	}
}
