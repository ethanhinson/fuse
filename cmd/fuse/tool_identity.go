package main

import (
	"fmt"
	"io"
	"time"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
	"github.com/ethanhinson/fuse/internal/toolidentity"
)

// buildToolIdentitySource is the change #52 composition root: it builds the
// egress CredentialSource (a Broker over the built-in STS plus any explicit
// per-server static credentials) from trusted config, to be wired into the MCP
// manager via mcp.WithCredentialSource.
//
// It returns (nil, "") — leaving MCP tools on their existing static-token path —
// UNLESS at least one configured MCP server declares an identity-propagating
// (identity/oauth-exchange) auth type. This keeps the seam inert for every repo
// that has not opted in: no behavior change, no new config required. When an
// identity server IS configured but no signing key is set, it returns a nil
// source and a human-readable reason the caller logs at startup (the identity
// server then fails closed at call time — never silently downgrades).
//
// The static tier is derived from the MCP server configs themselves: a server
// whose auth type is bearer/oauth2/static contributes its resolved static token
// to the broker so a mixed config (some identity, some legacy) works — the legacy
// servers keep presenting their identity-free credential.
func buildToolIdentitySource(cfg config.Config) (toolidentity.CredentialSource, string) {
	anyIdentity := false
	for _, s := range cfg.MCPServers {
		if s.IsIdentityPropagating() {
			anyIdentity = true
			break
		}
	}
	if !anyIdentity {
		return nil, ""
	}

	// An identity-propagating server needs a signing key to mint tokens. Without
	// one, return no source + a reason: the server fails closed (the broker denies
	// the OAuth tier for a tenant with no key) rather than silently using a static
	// token it does not have.
	if cfg.ToolIdentity.SigningKey == "" {
		return nil, "an MCP server declares an identity/oauth-exchange auth type but tool_identity.signing_key is unset in ~/.fuse/config.yml — identity-propagating servers cannot mint downstream tokens and will fail closed"
	}

	ttl := 5 * time.Minute
	if cfg.ToolIdentity.TTL != "" {
		if d, err := time.ParseDuration(cfg.ToolIdentity.TTL); err == nil && d > 0 {
			ttl = d
		}
	}

	// SINGLE-TENANT ASSUMPTION (explicit): the built-in STS is keyed ONLY for
	// event.DefaultTenant here, because the sole path that wires MCP tools into
	// the agent registry today is the single-user local shell (see the change #52
	// reconcile log). A request whose Principal carries any OTHER tenant would fail
	// closed with ErrTenantNotConfigured.
	//
	// TODO(#52-followup): when MCP egress is wired into the multi-tenant
	// loop-server path (loop_server.go / loop_serve_net.go, which do NOT attach MCP
	// today), this must build TenantKeys for every configured tenant (or resolve
	// per-tenant signing material from the host's key source) — otherwise every
	// non-default tenant silently fails closed. Do not remove this note until that
	// wiring exists.
	sts, err := toolidentity.NewBuiltinSTS(toolidentity.BuiltinSTSConfig{
		Issuer: "fuse",
		TTL:    ttl,
		TenantKeys: map[event.TenantID][]byte{
			event.DefaultTenant: []byte(cfg.ToolIdentity.SigningKey),
		},
	})
	if err != nil {
		return nil, fmt.Sprintf("tool_identity: building the built-in STS failed: %v", err)
	}

	// Derive explicit static credentials for legacy-tier MCP servers so a mixed
	// config still reaches them (identity-free).
	static := map[string]toolidentity.StaticCredential{}
	for _, s := range cfg.MCPServers {
		if s.IsIdentityPropagating() || !s.HasDownstreamCredential() {
			continue
		}
		if tok, err := mcpStaticToken(s); err == nil && tok != "" {
			static[s.Name] = toolidentity.StaticCredential{Scheme: "Bearer", Token: tok}
		}
	}

	return toolidentity.NewBroker(sts, static, nil), ""
}

// mcpStaticToken resolves the static bearer token for a legacy-tier MCP server
// WITHOUT triggering any interactive/network auth flow: it reads the plainly
// configured bearer secret (auth.type bearer/static → ClientSecret). An oauth2
// server's token comes from its own on-disk exchange at dial time, so it is left
// to the client's existing static path (returned empty here) rather than driving
// a browser flow from the composition root.
func mcpStaticToken(s config.MCPServerConfig) (string, error) {
	switch s.Auth.Type {
	case "bearer", "static":
		return s.Auth.ClientSecret, nil
	default:
		return "", nil
	}
}

// localPrincipal is the authorization identity stamped as the loop initiator on
// the non-networked (CLI/shell) paths, where there is no bearer token to resolve
// a Principal from. It is a single explicit local identity (never an empty,
// spoofable one) under the default tenant.
func localPrincipal(cfg config.Config) loopauth.Principal {
	subject := cfg.ToolIdentity.LocalSubject
	if subject == "" {
		subject = "local"
	}
	return loopauth.Principal{Tenant: event.DefaultTenant, Subject: subject}
}

// logToolIdentityPosture emits a one-line startup note describing whether the
// identity-propagation seam is active, so the operator can see the tier each MCP
// server resolved to (the seam is security-relevant; silence would hide a
// misconfiguration).
func logToolIdentityPosture(w io.Writer, cfg config.Config, wired bool, reason string) {
	if reason != "" {
		fmt.Fprintf(w, "tool-identity: %s\n", reason)
		return
	}
	if !wired {
		return
	}
	for _, s := range cfg.MCPServers {
		tier := "static (identity-free)"
		if s.IsIdentityPropagating() {
			tier = "identity-propagation (per-call delegation, audience-bound)"
		} else if !s.HasDownstreamCredential() {
			tier = "none"
		}
		fmt.Fprintf(w, "tool-identity: mcp server %q → %s\n", s.Name, tier)
	}
}
