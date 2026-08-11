package toolidentity

import (
	"strings"
	"testing"
)

// A Credential must never print its token through any of the fmt formatting
// verbs a stray log or error-wrap would reach. The token lives only in the
// outbound transport header (D6 redaction constraint).
func TestCredentialRedactsToken(t *testing.T) {
	const secret = "super-secret-minted-token-value"
	c := Credential{Scheme: "Bearer", Token: secret, carriesIdentity: true}

	for _, got := range []string{
		c.String(),
		c.GoString(),
	} {
		if strings.Contains(got, secret) {
			t.Fatalf("credential rendering leaked the token: %q", got)
		}
		if !strings.Contains(strings.ToLower(got), "redacted") {
			t.Fatalf("credential rendering should mark the token redacted, got %q", got)
		}
	}
}

// The token is still readable through the one accessor the transport injector
// uses — redaction hides it from formatting, not from the injector.
func TestCredentialAccessorReturnsToken(t *testing.T) {
	c := Credential{Scheme: "Bearer", Token: "tok", carriesIdentity: false}
	if got := c.Header(); got != "Bearer tok" {
		t.Fatalf("Header() = %q, want %q", got, "Bearer tok")
	}
	if c.CarriesIdentity() {
		t.Fatal("CarriesIdentity() should be false for a static credential")
	}
}

// An empty Header (empty token) yields no Authorization value — the caller must
// send no header rather than "Bearer " (an empty bearer).
func TestCredentialEmptyHeader(t *testing.T) {
	if got := (Credential{}).Header(); got != "" {
		t.Fatalf("empty Credential.Header() = %q, want empty", got)
	}
}

// A Target with no Audience is "undeclared" — the egress seam and the mediation
// gate deny it (root-of-trust: a tool must declare its downstream, never the
// model). A declared OAuth target is valid.
func TestTargetValidity(t *testing.T) {
	if (Target{Name: "srv"}).Valid() {
		t.Fatal("a target with an empty Audience must be invalid (undeclared)")
	}
	ok := Target{Name: "srv", Audience: "https://api.example.com", Tier: TierOAuth}
	if !ok.Valid() {
		t.Fatal("a target with an audience should be valid")
	}
	// A static-tier target is reachable without an audience (fuse-as-client, no
	// delegation), so a static target with a name is valid even with no audience.
	st := Target{Name: "legacy", Tier: TierStatic}
	if !st.Valid() {
		t.Fatal("a named static-tier target should be valid without an audience")
	}
}

// The zero Tier is TierOAuth — an unset tier defaults to the identity-propagating
// path, never silently to the weaker static tier.
func TestZeroTierIsOAuth(t *testing.T) {
	if (Target{}).Tier != TierOAuth {
		t.Fatal("the zero Tier must be TierOAuth, not the weaker static tier")
	}
}
