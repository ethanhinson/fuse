// Package toolidentity is fuse's tool/resource identity-propagation egress seam
// (change #52, Plane 2). It is the single choke point where the authenticated
// loop initiator's identity becomes a short-lived, audience-bound downstream
// credential for an outbound tool/MCP call — so the downstream, not fuse,
// adjudicates authorization (per-call RFC 8693 delegation, MCP spec rev
// 2025-11-25: no token passthrough, audience binding via RFC 8707).
//
// The package is deliberately auth-light: it imports only internal/loopauth (for
// the Principal type #49 resolves at the Connect edge) and the standard library.
// The Connect edge (internal/loopconnect) and the composition root (cmd/fuse) may
// import it; internal/runtime NEVER does, keeping the runtime seam policy-free
// (ADR-0030). The identity travels to the MCP egress as a request-context value
// (see context.go), never reconstructed from model output.
package toolidentity

import (
	"context"

	"github.com/ethanhinson/fuse/internal/loopauth"
)

// Tier selects how a downstream target is authenticated. The zero value is
// TierOAuth: an unset tier defaults to the identity-propagating exchange path,
// never silently to the weaker static tier.
type Tier int

const (
	// TierOAuth is the identity-propagation path: an RFC 8693 delegation token,
	// audience-bound to the target, minted per call. The default (zero value).
	TierOAuth Tier = iota
	// TierStatic is the explicit legacy fallback: a per-server static credential
	// that carries NO initiator identity (fuse-as-client, not delegation). It is
	// never a silent default — a server must be explicitly configured static.
	TierStatic
)

// Target names a downstream system a tool call may reach. It is declared by the
// TOOL DEFINITION (never by the model): its Audience is the RFC 8707 resource
// identifier the minted token is bound to, and Scopes are the scopes the tool
// requires. A model-supplied scope or target never enters here.
type Target struct {
	// Name is the human/config identifier of the downstream (e.g. the MCP server
	// name). Used for audit and static-credential lookup.
	Name string
	// Audience is the RFC 8707 resource identifier the credential is bound to.
	// Required for TierOAuth; a TierOAuth target with an empty Audience is
	// "undeclared" and denied.
	Audience string
	// Scopes are the required downstream scopes, sourced from the tool declaration.
	Scopes []string
	// Tier selects the auth path. Zero value TierOAuth.
	Tier Tier
}

// Valid reports whether the target is declared well enough to reach. A TierOAuth
// target MUST carry an Audience (else it is undeclared — the egress seam and the
// mediation gate deny it). A TierStatic target only needs a Name.
func (t Target) Valid() bool {
	switch t.Tier {
	case TierStatic:
		return t.Name != ""
	default: // TierOAuth
		return t.Audience != ""
	}
}

// Credential is a resolved, short-lived downstream credential (or an explicit
// static one). Its Token is set on the outbound transport header ONLY — String
// and GoString redact it so a stray %v, log line, or error wrap can never leak it
// (D6). The token is reachable solely through Header, which the transport
// injector calls.
type Credential struct {
	// Scheme is the auth scheme, e.g. "Bearer".
	Scheme string
	// Token is the secret credential value. Read it ONLY via Header — never format
	// this struct with a token in a log/event/error.
	Token string
	// carriesIdentity records whether this credential carries the loop initiator's
	// identity (true for a delegated OAuth token, false for a static credential).
	carriesIdentity bool
}

// NewCredential builds a Credential, making carriesIdentity explicit at the call
// site (the broker sets true for a delegated token, false for a static one).
func NewCredential(scheme, token string, carriesIdentity bool) Credential {
	return Credential{Scheme: scheme, Token: token, carriesIdentity: carriesIdentity}
}

// Header returns the full Authorization header value ("<Scheme> <Token>"), or the
// empty string when the token is empty (so the caller sends no header rather than
// an empty "Bearer "). This is the ONLY method that exposes the token.
func (c Credential) Header() string {
	if c.Token == "" {
		return ""
	}
	scheme := c.Scheme
	if scheme == "" {
		scheme = "Bearer"
	}
	return scheme + " " + c.Token
}

// CarriesIdentity reports whether the credential carries the loop initiator's
// identity (a delegated token) versus being an identity-free static credential.
func (c Credential) CarriesIdentity() bool { return c.carriesIdentity }

// String redacts the token — see the D6 redaction constraint.
func (c Credential) String() string {
	return "toolidentity.Credential{Scheme:" + c.Scheme + " Token:<redacted> carriesIdentity:" + boolStr(c.carriesIdentity) + "}"
}

// GoString redacts the token for the %#v verb too.
func (c Credential) GoString() string { return c.String() }

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// CredentialSource is the egress seam: the single choke point that turns an
// authenticated Principal + a declared Target into a downstream Credential. The
// broker (broker.go) is the production implementation; a fake backs tests.
type CredentialSource interface {
	// CredentialFor resolves the credential for principal p to reach target t. It
	// returns an error (never a wrong-audience or cross-tenant credential) when the
	// target is undeclared, a static target has no configured credential, or the
	// exchange fails.
	CredentialFor(ctx context.Context, p loopauth.Principal, t Target) (Credential, error)
}
