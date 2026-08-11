package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
	"github.com/ethanhinson/fuse/internal/mcp"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/toolidentity"
)

// mcpAttach is the single composition-root helper every loop binding calls to
// wire an mcp.Manager. It resolves, together, the identity-propagation egress
// seam (the mcp.WithCredentialSource ManagerOption, when a source is configured)
// AND the complete-mediation TargetMediator permissions.Option — so no binding
// can construct a manager without also wiring the egress seam + the mediation
// gate. Co-locating them here (with buildToolIdentitySource / buildTargetMediator)
// is the structural spine (spec §1): the two options are derived from the same
// config in one place and cannot drift apart per binding.
//
// It also writes the startup posture log to w (the security-relevant one-liner
// describing each MCP server's resolved tier, or the fail-closed reason), so a
// misconfiguration is never silent regardless of which binding attached MCP.
//
// Contract:
//   - mcpOpts carries WithCredentialSource exactly when buildToolIdentitySource
//     yields a source; empty otherwise (pre-#52 static-token path, byte-identical).
//   - mediator is non-nil exactly when an identity-propagating server is declared
//     (buildTargetMediator's gate), independent of whether a signing key resolved —
//     an identity server with no key still declares a target the mediator gates.
func mcpAttach(cfg config.Config, w io.Writer) (mcpOpts []mcp.ManagerOption, mediator permissions.Option) {
	if src, reason := buildToolIdentitySource(cfg); src != nil {
		mcpOpts = append(mcpOpts, mcp.WithCredentialSource(src))
		logToolIdentityPosture(w, cfg, true, "")
	} else if reason != "" {
		logToolIdentityPosture(w, cfg, false, reason)
	}
	mediator = buildTargetMediator(cfg)
	return mcpOpts, mediator
}

// configTargetMediator is the concrete complete-mediation gate (change #52 D5)
// wired at the composition root. It enforces, from the loop-start root of trust
// (the MCP server config — never model output), the tool→target DECLARATION: a
// tool call reaching an MCP server may proceed only if that server is configured
// and its identity tier is DECLARED well enough to reach (an identity/oauth
// server MUST declare an audience). A schema-valid call to an undeclared or
// unknown MCP target is denied terminally, even in ModeOff — the gate consults
// this before any mode/approval logic.
//
// It intentionally covers the reachable slice of D5 today: the tool→target
// declaration allowlist. Richer per-principal/tenant scope ceilings are a
// follow-on that rides this same seam (they need the multi-tenant loop-server MCP
// wiring that does not exist yet — see buildToolIdentitySource's TODO).
type configTargetMediator struct {
	// targets maps an MCP tool name prefix ("mcp:<server>/") to that server's
	// declared toolidentity.Target. A tool whose server is absent is unknown.
	byServer map[string]toolidentity.Target
}

// newConfigTargetMediator builds the mediator from the MCP server configs. It is
// nil-safe to construct with no servers (every MCP call is then denied as
// unknown, and non-MCP tools are always allowed).
func newConfigTargetMediator(cfg config.Config) *configTargetMediator {
	m := &configTargetMediator{byServer: map[string]toolidentity.Target{}}
	for _, s := range cfg.MCPServers {
		m.byServer[s.Name] = s.ToolIdentityTarget()
	}
	return m
}

// MediateTarget implements permissions.TargetMediator. A non-MCP tool (no
// "mcp:" prefix) reaches no downstream target and is always permitted. An MCP
// tool is permitted only when its server is configured AND its declared Target
// is valid (an identity target must carry an audience); otherwise it is denied.
func (m *configTargetMediator) MediateTarget(_ context.Context, toolName, _ string) (bool, string) {
	if !strings.HasPrefix(toolName, "mcp:") {
		return true, "" // in-process tool: no downstream target to mediate
	}
	// toolName is "mcp:<server>/<tool>"; extract <server>.
	rest := strings.TrimPrefix(toolName, "mcp:")
	server := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		server = rest[:i]
	}
	target, ok := m.byServer[server]
	if !ok {
		return false, fmt.Sprintf("tool target %q is not a configured MCP server (root-of-trust allowlist)", server)
	}
	if !target.Valid() {
		return false, fmt.Sprintf("MCP server %q declares an identity tier but no audience — undeclared downstream target, denied", server)
	}
	return true, ""
}

// buildTargetMediator returns the complete-mediation gate option to wire into the
// permission gate, but ONLY when the identity-propagation seam is active (an
// identity-propagating MCP server is configured). When inert, it returns nil so
// pre-#52 behavior is byte-identical (no mediation in the path). Enforcing the
// allowlist only when identity propagation is on keeps the gate's behavior for
// existing repos unchanged while making the D5 guarantee real wherever the seam
// is active.
func buildTargetMediator(cfg config.Config) permissions.Option {
	anyIdentity := false
	for _, s := range cfg.MCPServers {
		if s.IsIdentityPropagating() {
			anyIdentity = true
			break
		}
	}
	if !anyIdentity {
		return nil
	}
	return permissions.WithTargetMediator(newConfigTargetMediator(cfg))
}

// isHTTPTransport reports whether an MCP server transport carries an outbound
// HTTP request the egress seam can attach a per-call credential to. The
// identity-propagation credential is injected only on these transports.
func isHTTPTransport(transport string) bool {
	switch transport {
	case "http", "sse", "streamable-http":
		return true
	default: // "stdio" or unset (defaults to stdio in mcp.dial)
		return false
	}
}

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
			// An identity-propagating server on a non-HTTP transport is a
			// misconfiguration: the per-call credential is injected only on the HTTP
			// egress (applyCallAuth on the HTTP/streamable clients); a stdio server
			// would mint a delegation token every call that never leaves the process.
			// Fail closed with a startup reason rather than silently minting into the
			// void, so the operator fixes the transport instead of believing identity
			// propagation is active.
			if !isHTTPTransport(s.Transport) {
				return nil, fmt.Sprintf("MCP server %q declares an identity/oauth-exchange auth type but transport %q is not http/streamable-http — the minted token cannot be carried over stdio; identity propagation is inert for it", s.Name, s.Transport)
			}
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
