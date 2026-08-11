package config

import "github.com/ethanhinson/fuse/internal/toolidentity"

// tierForAuthType maps an MCP auth `type` to an identity-propagation tier plus a
// hasCredential flag. Identity-propagating types are TierOAuth (per-call
// delegation, audience-bound); the legacy static types are TierStatic
// (identity-free per-server credential); "none" carries no credential.
//
// Existing "bearer"/"oauth2" configs deliberately map to TierStatic — a config
// re-cut is NOT required to keep working, and they never silently become
// identity-propagating (which would change their security posture).
func tierForAuthType(authType string) (tier toolidentity.Tier, hasCredential bool) {
	switch authType {
	case "identity", "oauth-exchange":
		return toolidentity.TierOAuth, true
	case "bearer", "oauth2", "static":
		return toolidentity.TierStatic, true
	default: // "none" or unset
		return toolidentity.TierStatic, false
	}
}

// HasDownstreamCredential reports whether the server is configured to present a
// downstream credential at all (any auth type other than "none"/unset).
func (s MCPServerConfig) HasDownstreamCredential() bool {
	_, has := tierForAuthType(s.Auth.Type)
	return has
}

// IsIdentityPropagating reports whether the server uses an identity-propagation
// (TierOAuth) auth type — a per-call delegation token minted from the loop
// initiator's identity, audience-bound to the server's Audience.
func (s MCPServerConfig) IsIdentityPropagating() bool {
	tier, has := tierForAuthType(s.Auth.Type)
	return has && tier == toolidentity.TierOAuth
}

// ToolIdentityTarget derives the toolidentity.Target the egress seam resolves a
// credential for when reaching this server. The Target is declared entirely from
// trusted config — Name and Audience from the server block, Scopes and Tier from
// the auth block — never from model output.
//
// For an identity/oauth-exchange server the Tier is TierOAuth and the Audience
// is load-bearing (an empty Audience yields an invalid Target, which the broker
// denies). For bearer/oauth2/static the Tier is TierStatic and only the Name is
// required. A "none"/unset server still yields a Target (TierStatic, no
// credential configured), which the broker denies as ErrNoStaticCredential — the
// composition root simply does not wire a source for such servers.
func (s MCPServerConfig) ToolIdentityTarget() toolidentity.Target {
	tier, _ := tierForAuthType(s.Auth.Type)
	// Prefer the server-level Scopes; fall back to auth.scopes so an existing
	// oauth2 config that declared its scopes under auth still populates the target.
	scopes := s.Scopes
	if len(scopes) == 0 {
		scopes = s.Auth.Scopes
	}
	return toolidentity.Target{
		Name:     s.Name,
		Audience: s.Audience,
		Scopes:   scopes,
		Tier:     tier,
	}
}
