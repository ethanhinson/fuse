package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testIssuerKey = "0123456789abcdef0123456789abcdef"

func validIssuer() IssuerConfig {
	return IssuerConfig{Issuer: "cms", SigningKey: testIssuerKey, Audience: "fuse-loop", Tenants: []string{"_default"}}
}

// TestValidateIssuers pins each startup refusal for loop_server.issuers, and the
// exact config key it names, so an operator is pointed at the right line.
func TestValidateIssuers(t *testing.T) {
	cases := []struct {
		desc    string
		issuers []IssuerConfig
		wantErr string // "" means valid
	}{
		{"a complete issuer is valid", []IssuerConfig{validIssuer()}, ""},
		{"max_ttl set is valid", []IssuerConfig{func() IssuerConfig { i := validIssuer(); i.MaxTTL = "15m"; return i }()}, ""},
		{"empty tenants is valid (only _default)", []IssuerConfig{func() IssuerConfig { i := validIssuer(); i.Tenants = nil; return i }()}, ""},
		{"two distinct issuers are valid", []IssuerConfig{validIssuer(), func() IssuerConfig { i := validIssuer(); i.Issuer = "billing"; return i }()}, ""},
		{"empty issuer", []IssuerConfig{func() IssuerConfig { i := validIssuer(); i.Issuer = ""; return i }()},
			"loop_server.issuers[0]: issuer is required"},
		{"blank issuer", []IssuerConfig{func() IssuerConfig { i := validIssuer(); i.Issuer = "  "; return i }()},
			"issuer is required"},
		{"duplicate issuer", []IssuerConfig{validIssuer(), validIssuer()},
			`loop_server.issuers[1] ("cms"): issuer "cms" is already declared by loop_server.issuers[0]`},
		{"missing signing key", []IssuerConfig{func() IssuerConfig { i := validIssuer(); i.SigningKey = ""; return i }()},
			`loop_server.issuers[0] ("cms"): signing_key is required`},
		{"short signing key", []IssuerConfig{func() IssuerConfig { i := validIssuer(); i.SigningKey = "short"; return i }()},
			"signing_key must be at least 32 bytes for HS256, got 5"},
		{"missing audience", []IssuerConfig{func() IssuerConfig { i := validIssuer(); i.Audience = ""; return i }()},
			"audience is required"},
		{"zero max_ttl", []IssuerConfig{func() IssuerConfig { i := validIssuer(); i.MaxTTL = "0s"; return i }()},
			"max_ttl must be positive, got 0s"},
		{"negative max_ttl", []IssuerConfig{func() IssuerConfig { i := validIssuer(); i.MaxTTL = "-1m"; return i }()},
			"max_ttl must be positive, got -1m0s"},
		{"max_ttl without unit", []IssuerConfig{func() IssuerConfig { i := validIssuer(); i.MaxTTL = "60"; return i }()},
			`max_ttl "60" is not a valid duration`},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			c := Default()
			c.LoopServer.Issuers = tc.issuers
			err := c.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestIssuerMaxTTLDefault(t *testing.T) {
	d, err := validIssuer().MaxTTLDuration()
	if err != nil || d != time.Hour {
		t.Fatalf("unset max_ttl = %s, %v; want 1h", d, err)
	}
}

// TestLoopServerIssuersLoadFromTrustedHome asserts the issuers block lands from
// ~/.fuse/config.yml with every field, and alongside static auth entries.
func TestLoopServerIssuersLoadFromTrustedHome(t *testing.T) {
	cwd := chdirTemp(t)
	home := filepath.Join(cwd, "home")
	if err := os.MkdirAll(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	os.Unsetenv("LLM_GATEWAY_URL")
	os.Unsetenv("LLM_GATEWAY_KEY")

	homeCfg := `
loop_server:
  auth:
    - token: tok-a
      subject: alice
  issuers:
    - issuer: cms
      signing_key: ` + testIssuerKey + `
      audience: fuse-loop
      tenants: [_default, acme]
      max_ttl: 15m
      observability_operator: true
`
	if err := os.WriteFile(filepath.Join(home, ".fuse", "config.yml"), []byte(homeCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.LoopServer.Auth) != 1 {
		t.Fatalf("auth entries = %d, want 1", len(c.LoopServer.Auth))
	}
	if len(c.LoopServer.Issuers) != 1 {
		t.Fatalf("issuers = %d, want 1", len(c.LoopServer.Issuers))
	}
	got := c.LoopServer.Issuers[0]
	if got.Issuer != "cms" || got.SigningKey != testIssuerKey || got.Audience != "fuse-loop" ||
		strings.Join(got.Tenants, ",") != "_default,acme" || got.MaxTTL != "15m" || !got.ObservabilityOperator {
		t.Fatalf("issuer = %+v", got)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestLoopServerIssuersIgnoredFromUntrustedLocal asserts a repo-plantable
// .fuse.local.yml cannot declare a trusted issuer: an issuer key mints tokens
// for any subject, so it is a credential surface like loop_server.auth.
func TestLoopServerIssuersIgnoredFromUntrustedLocal(t *testing.T) {
	cwd := chdirTemp(t)
	home := filepath.Join(cwd, "home")
	if err := os.MkdirAll(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	os.Unsetenv("LLM_GATEWAY_URL")
	os.Unsetenv("LLM_GATEWAY_KEY")

	local := `
loop_server:
  issuers:
    - issuer: evil
      signing_key: ` + testIssuerKey + `
      audience: fuse-loop
      tenants: [acme]
`
	if err := os.WriteFile(filepath.Join(cwd, ".fuse.local.yml"), []byte(local), 0o644); err != nil {
		t.Fatal(err)
	}
	warnings := captureWarnings(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.LoopServer.Issuers) != 0 {
		t.Fatalf("untrusted loop_server.issuers must be dropped, got %+v", c.LoopServer.Issuers)
	}
	if !strings.Contains(warnings(), "loop_server") {
		t.Errorf("expected a warning naming loop_server, got: %q", warnings())
	}
}

func TestValidateIssuersJWKSSources(t *testing.T) {
	both := []IssuerConfig{{Issuer: "cms", Audience: "a", SigningKey: strings.Repeat("k", 32), JWKSURL: "https://x/jwks"}}
	err := validateIssuers(both)
	if err == nil || !strings.Contains(err.Error(), `("cms")`) {
		t.Fatalf("both sources: want error naming issuer, got %v", err)
	}
	if err := validateIssuers([]IssuerConfig{{Issuer: "cms", Audience: "a", JWKSURL: "http://example.com/jwks"}}); err == nil {
		t.Fatal("non-https jwks_url accepted")
	}
	if err := validateIssuers([]IssuerConfig{{Issuer: "cms", Audience: "a", JWKSURL: "http://localhost:9/jwks", TenantPrefix: "team-"}}); err != nil {
		t.Fatalf("loopback http jwks_url refused: %v", err)
	}
	if err := validateIssuers([]IssuerConfig{{Issuer: "cms", Audience: "a", JWKSURL: "https://x/jwks", TenantPrefix: "Team"}}); err == nil {
		t.Fatal("bad tenant_prefix accepted")
	}
}

// TestLoopServerSpecPolicyIgnoredFromUntrustedLocal asserts a repo-plantable
// .fuse.local.yml cannot switch off the spec policy's private-network floor.
func TestLoopServerSpecPolicyIgnoredFromUntrustedLocal(t *testing.T) {
	cwd := chdirTemp(t)
	home := filepath.Join(cwd, "home")
	if err := os.MkdirAll(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	os.Unsetenv("LLM_GATEWAY_URL")
	os.Unsetenv("LLM_GATEWAY_KEY")
	local := "loop_server:\n  spec_policy: {deny_private_networks: false, models: any}\n"
	if err := os.WriteFile(filepath.Join(cwd, ".fuse.local.yml"), []byte(local), 0o644); err != nil {
		t.Fatal(err)
	}
	warnings := captureWarnings(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.LoopServer.SpecPolicy.DenyPrivate() {
		t.Fatalf("untrusted spec_policy must be dropped, got %+v", c.LoopServer.SpecPolicy)
	}
	if !strings.Contains(warnings(), "loop_server") {
		t.Errorf("expected a warning naming loop_server, got: %q", warnings())
	}
}

// carriedSkillsHome points HOME at a fresh temp home (and cwd at its parent)
// and writes home and local config files when non-empty.
func carriedSkillsHome(t *testing.T, homeCfg, localCfg string) {
	t.Helper()
	cwd := chdirTemp(t)
	home := filepath.Join(cwd, "home")
	if err := os.MkdirAll(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	os.Unsetenv("LLM_GATEWAY_URL")
	os.Unsetenv("LLM_GATEWAY_KEY")
	if homeCfg != "" {
		if err := os.WriteFile(filepath.Join(home, ".fuse", "config.yml"), []byte(homeCfg), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if localCfg != "" {
		if err := os.WriteFile(filepath.Join(cwd, ".fuse.local.yml"), []byte(localCfg), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestSpecPolicyCarriedSkillsDefaultsTrue pins loop_server.spec_policy.carried_skills:
// unset means carried skills are accepted; an explicit bool is honored.
func TestSpecPolicyCarriedSkillsDefaultsTrue(t *testing.T) {
	if !(SpecPolicyConfig{}).CarriedSkillsAllowed() {
		t.Fatal("zero SpecPolicyConfig must allow carried skills")
	}
	carriedSkillsHome(t, "", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.LoopServer.SpecPolicy.CarriedSkillsAllowed() {
		t.Fatal("a config with no spec_policy block must allow carried skills")
	}
	for doc, want := range map[string]bool{
		"loop_server:\n  spec_policy: {carried_skills: false}\n": false,
		"loop_server:\n  spec_policy: {carried_skills: true}\n":  true,
	} {
		c, err := FromYAML([]byte(doc), "test.yml")
		if err != nil {
			t.Fatalf("%q: %v", doc, err)
		}
		if got := c.LoopServer.SpecPolicy.CarriedSkillsAllowed(); got != want {
			t.Errorf("%q: CarriedSkillsAllowed() = %v, want %v", doc, got, want)
		}
	}
}

// TestSpecPolicyCarriedSkillsOnlyKeyIsKept guards the trusted merge: a
// spec_policy block whose only key is carried_skills is not treated as empty.
func TestSpecPolicyCarriedSkillsOnlyKeyIsKept(t *testing.T) {
	carriedSkillsHome(t, "loop_server:\n  spec_policy:\n    carried_skills: false\n", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.LoopServer.SpecPolicy.CarriedSkillsAllowed() {
		t.Fatalf("trusted carried_skills: false was dropped, got %+v", c.LoopServer.SpecPolicy)
	}
}

// TestSpecPolicyCarriedSkillsIgnoredFromUntrustedLocal: like the other
// spec_policy keys, carried_skills is honored only from the trusted home file.
func TestSpecPolicyCarriedSkillsIgnoredFromUntrustedLocal(t *testing.T) {
	carriedSkillsHome(t, "", "loop_server:\n  spec_policy: {carried_skills: false}\n")
	warnings := captureWarnings(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.LoopServer.SpecPolicy.CarriedSkillsAllowed() {
		t.Fatalf("untrusted carried_skills must be dropped, got %+v", c.LoopServer.SpecPolicy)
	}
	if !strings.Contains(warnings(), "loop_server") {
		t.Errorf("expected a warning naming loop_server, got: %q", warnings())
	}
}

// TestSpecPolicyCarriedSkillsMustBeABool: a non-bool value is a load error,
// reported exactly as a non-bool deny_private_networks is.
func TestSpecPolicyCarriedSkillsMustBeABool(t *testing.T) {
	_, sibling := FromYAML([]byte("loop_server:\n  spec_policy: {deny_private_networks: maybe}\n"), "test.yml")
	_, err := FromYAML([]byte("loop_server:\n  spec_policy: {carried_skills: maybe}\n"), "test.yml")
	if sibling == nil || err == nil {
		t.Fatalf("non-bool spec_policy values must fail to load: sibling=%v carried=%v", sibling, err)
	}
	if err.Error() != sibling.Error() {
		t.Errorf("carried_skills error %q differs from sibling %q", err, sibling)
	}
}
