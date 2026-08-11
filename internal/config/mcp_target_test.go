package config

import (
	"testing"

	"github.com/ethanhinson/fuse/internal/toolidentity"
)

// TestToolIdentityTarget_TierMapping asserts every auth type maps to the correct
// identity-propagation tier and credential-presence, and that a Target derived
// from config carries the server's declared name/audience/scopes — never model
// input. Load-bearing back-compat: existing bearer/oauth2 map to TierStatic.
func TestToolIdentityTarget_TierMapping(t *testing.T) {
	cases := []struct {
		name        string
		authType    string
		wantTier    toolidentity.Tier
		wantHasCred bool
		wantIdent   bool
	}{
		{"identity", "identity", toolidentity.TierOAuth, true, true},
		{"oauth-exchange", "oauth-exchange", toolidentity.TierOAuth, true, true},
		{"bearer-static", "bearer", toolidentity.TierStatic, true, false},
		{"oauth2-static", "oauth2", toolidentity.TierStatic, true, false},
		{"static", "static", toolidentity.TierStatic, true, false},
		{"none", "none", toolidentity.TierStatic, false, false},
		{"unset", "", toolidentity.TierStatic, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := MCPServerConfig{
				Name:     "srv-" + tc.name,
				Audience: "https://api.example.test",
				Scopes:   []string{"read", "write"},
				Auth:     MCPAuthConfig{Type: tc.authType},
			}
			tgt := srv.ToolIdentityTarget()
			if tgt.Tier != tc.wantTier {
				t.Errorf("Tier = %d, want %d", tgt.Tier, tc.wantTier)
			}
			if tgt.Name != srv.Name {
				t.Errorf("Name = %q, want %q", tgt.Name, srv.Name)
			}
			if tgt.Audience != srv.Audience {
				t.Errorf("Audience = %q, want %q", tgt.Audience, srv.Audience)
			}
			if len(tgt.Scopes) != 2 {
				t.Errorf("Scopes = %v, want 2 entries", tgt.Scopes)
			}
			if got := srv.HasDownstreamCredential(); got != tc.wantHasCred {
				t.Errorf("HasDownstreamCredential = %v, want %v", got, tc.wantHasCred)
			}
			if got := srv.IsIdentityPropagating(); got != tc.wantIdent {
				t.Errorf("IsIdentityPropagating = %v, want %v", got, tc.wantIdent)
			}
		})
	}
}

// TestToolIdentityTarget_OAuthAudienceRequired: an identity-tier server with an
// empty Audience yields an INVALID target (the broker denies it), while a
// static-tier server needs only a name. This is the fail-closed guard against an
// undeclared OAuth target.
func TestToolIdentityTarget_OAuthAudienceRequired(t *testing.T) {
	oauthNoAud := MCPServerConfig{Name: "s", Auth: MCPAuthConfig{Type: "identity"}}
	if oauthNoAud.ToolIdentityTarget().Valid() {
		t.Error("an identity-tier target with no audience must be invalid (undeclared)")
	}
	oauthWithAud := MCPServerConfig{Name: "s", Audience: "aud", Auth: MCPAuthConfig{Type: "identity"}}
	if !oauthWithAud.ToolIdentityTarget().Valid() {
		t.Error("an identity-tier target with an audience must be valid")
	}
	static := MCPServerConfig{Name: "s", Auth: MCPAuthConfig{Type: "bearer"}}
	if !static.ToolIdentityTarget().Valid() {
		t.Error("a static-tier target with a name must be valid")
	}
}

// TestToolIdentityTarget_ScopesFallBackToAuth: an existing oauth2 config that
// declared scopes under auth.scopes (not the new server-level field) still
// populates the derived Target's scopes — back-compat for pre-#52 configs.
func TestToolIdentityTarget_ScopesFallBackToAuth(t *testing.T) {
	srv := MCPServerConfig{
		Name: "s",
		Auth: MCPAuthConfig{Type: "oauth2", Scopes: []string{"legacy.scope"}},
	}
	tgt := srv.ToolIdentityTarget()
	if len(tgt.Scopes) != 1 || tgt.Scopes[0] != "legacy.scope" {
		t.Errorf("Scopes = %v, want [legacy.scope] from auth.scopes fallback", tgt.Scopes)
	}
}
