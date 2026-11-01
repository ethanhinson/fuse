package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeTrustedHomeConfig points HOME at a temp dir holding body as the trusted
// ~/.fuse/config.yml (the only layer that may declare loop_server.auth).
func writeTrustedHomeConfig(t *testing.T, body string) {
	t.Helper()
	cwd := chdirTemp(t)
	home := filepath.Join(cwd, "home")
	if err := os.MkdirAll(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	os.Unsetenv("LLM_GATEWAY_URL")
	os.Unsetenv("LLM_GATEWAY_KEY")
	if err := os.WriteFile(filepath.Join(home, ".fuse", "config.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAuthTokenScopesLoad(t *testing.T) {
	writeTrustedHomeConfig(t, `
loop_server:
  auth:
    - {token: a, tenant: acme, subject: x, scopes: [loop:observe]}
    - {token: b, tenant: acme, subject: y}
`)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.LoopServer.Auth) != 2 {
		t.Fatalf("auth entries = %d, want 2", len(c.LoopServer.Auth))
	}
	if got := c.LoopServer.Auth[0].Scopes; !reflect.DeepEqual(got, []string{"loop:observe"}) {
		t.Errorf("auth[0].scopes = %#v, want [loop:observe]", got)
	}
	if got := c.LoopServer.Auth[1].Scopes; got != nil {
		t.Errorf("auth[1].scopes = %#v, want nil (omitted = unrestricted)", got)
	}
}

func TestAuthTokenScopesValidation(t *testing.T) {
	cases := []struct {
		name     string
		scopes   []string
		wantPath string // "" means no problem expected
		wantMsg  string
	}{
		{"explicit empty", []string{}, "loop_server.auth[0].scopes", "omit scopes for an unrestricted token"},
		{"whitespace entry", []string{"loop:start loop:send"}, "loop_server.auth[0].scopes[0]", "is not a single scope name"},
		{"empty entry", []string{""}, "loop_server.auth[0].scopes[0]", "is not a single scope name"},
		{"unknown ignored", []string{"loop:observe", "unknown:thing"}, "", ""},
		{"omitted", nil, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{LoopServer: LoopServerConfig{Auth: []AuthTokenConfig{
				{Token: "secret-token-value", Tenant: "acme", Subject: "x", Scopes: tc.scopes},
			}}}
			var hits []Problem
			for _, p := range c.Problems() {
				if strings.HasPrefix(p.Path, "loop_server.auth") {
					hits = append(hits, p)
				}
			}
			if tc.wantPath == "" {
				if len(hits) != 0 {
					t.Fatalf("want no auth problem, got %+v", hits)
				}
				return
			}
			if len(hits) != 1 {
				t.Fatalf("want exactly one auth problem, got %+v", hits)
			}
			if hits[0].Path != tc.wantPath {
				t.Errorf("path = %q, want %q", hits[0].Path, tc.wantPath)
			}
			if !strings.Contains(hits[0].Message, tc.wantMsg) {
				t.Errorf("message = %q, want it to contain %q", hits[0].Message, tc.wantMsg)
			}
			if strings.Contains(hits[0].Error(), "secret-token-value") {
				t.Errorf("problem must never echo the token: %q", hits[0].Error())
			}
		})
	}
}

// TestAuthTokenScopesExplicitEmptyReachesValidator proves YAML `scopes: []`
// survives the loader as non-nil and empty, so the validator can tell an
// explicitly empty list (refused) from an omitted one (unrestricted).
func TestAuthTokenScopesExplicitEmptyReachesValidator(t *testing.T) {
	writeTrustedHomeConfig(t, `
loop_server:
  auth:
    - {token: a, tenant: acme, subject: x, scopes: []}
`)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.LoopServer.Auth) != 1 {
		t.Fatalf("auth entries = %d, want 1", len(c.LoopServer.Auth))
	}
	if s := c.LoopServer.Auth[0].Scopes; s == nil || len(s) != 0 {
		t.Fatalf("scopes = %#v, want non-nil empty", s)
	}
	err = c.Validate()
	if err == nil || !strings.Contains(err.Error(), "loop_server.auth[0].scopes") {
		t.Fatalf("Validate() = %v, want the explicit-empty scopes problem", err)
	}
}
