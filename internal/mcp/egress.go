package mcp

import (
	"context"

	"github.com/ethanhinson/fuse/internal/toolidentity"
)

// callAuth is the per-call credential + target carried on the request context
// from MCPTool.Execute (the resolution site) down to the outbound transport. It
// holds BOTH the resolved Credential (whose token is set on the Authorization
// header via Header() only) AND the declared Target, so the transport can bind
// the outbound request to the target's RFC 8707 audience — never sending a token
// whose audience is a different target.
//
// Design choice (audience / resource carriage): the MCP tools/call JSON-RPC body
// shape is fixed by the spec ({"name","arguments"}), so threading a `resource`
// param into the body is not clean. Instead the target audience rides as an
// HTTP header (mcpResourceHeader) set next to Authorization on the same outbound
// request. This keeps the JSON-RPC envelope untouched and makes the binding a
// transport-level property: the request that carries target A's token also
// carries target A's audience, and no other.
type callAuth struct {
	cred   toolidentity.Credential
	target toolidentity.Target
}

// callAuthCtxKey is the unexported context key under which callAuth is threaded.
// Unexported so only this package can set or read it — the credential can never
// be injected by untrusted code through a foreign string key.
type callAuthCtxKey struct{}

// mcpResourceHeader carries the RFC 8707 resource (target audience) the token is
// bound to, alongside Authorization on the outbound request. Chosen over a
// JSON-RPC body `resource` param because the tools/call body shape is fixed.
const mcpResourceHeader = "MCP-Resource"

// WithCallCredential returns a child context carrying cred as the per-call
// downstream credential for the next outbound MCP request. It sets no target
// audience — use WithCallAuth when the resolved Target is available so the
// request is audience-bound. Present so callers with only a Credential still
// have a supported entry point.
func WithCallCredential(ctx context.Context, cred toolidentity.Credential) context.Context {
	return context.WithValue(ctx, callAuthCtxKey{}, callAuth{cred: cred})
}

// WithCallAuth returns a child context carrying both the resolved credential and
// the declared target for the next outbound MCP request. The transport reads the
// credential for the Authorization header and the target's Audience for the
// RFC 8707 resource binding.
func WithCallAuth(ctx context.Context, cred toolidentity.Credential, target toolidentity.Target) context.Context {
	return context.WithValue(ctx, callAuthCtxKey{}, callAuth{cred: cred, target: target})
}

// callCredentialFrom returns the per-call credential threaded on ctx and
// ok=false when none is present. When ok is false the transport falls back to
// its static bearerToken field exactly as before (back-compat).
func callCredentialFrom(ctx context.Context) (toolidentity.Credential, bool) {
	ca, ok := ctx.Value(callAuthCtxKey{}).(callAuth)
	if !ok {
		return toolidentity.Credential{}, false
	}
	return ca.cred, true
}

// callAuthFrom returns the full per-call auth (credential + target) on ctx.
func callAuthFrom(ctx context.Context) (callAuth, bool) {
	ca, ok := ctx.Value(callAuthCtxKey{}).(callAuth)
	return ca, ok
}

// applyCallAuth sets the Authorization header from a per-call credential when one
// is present on ctx, plus the RFC 8707 resource header when the target declares
// an audience. When NO per-call credential is present it invokes fallback (the
// transport's existing static-token header logic) so existing servers behave
// byte-identically. A per-call credential whose Header() is empty sends no
// Authorization header at all (rather than an empty "Bearer ").
func applyCallAuth(ctx context.Context, setHeader func(key, value string), fallback func()) {
	ca, ok := callAuthFrom(ctx)
	if !ok {
		fallback()
		return
	}
	if h := ca.cred.Header(); h != "" {
		setHeader("Authorization", h)
	}
	// Bind the outbound request to the target's RFC 8707 resource so fuse never
	// sends a token whose audience is a different target. Only when the target
	// declared an audience (a static-tier target need not).
	if ca.target.Audience != "" {
		setHeader(mcpResourceHeader, ca.target.Audience)
	}
}
