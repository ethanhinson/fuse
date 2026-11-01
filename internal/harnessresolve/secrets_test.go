package harnessresolve

import (
	"context"
	"errors"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
)

func TestDestinationAllowedCanonicalizesBothSides(t *testing.T) {
	cases := []struct {
		hosts       []string
		destination string
		want        bool
	}{
		{[]string{"MCP.Acme.Test."}, "https://mcp.acme.test/x", true},
		{[]string{"mcp.acme.test"}, "https://MCP.ACME.TEST./x", true},
		{[]string{"mcp.acme.test"}, "https://mcp.acme.test:8443/x", true},
		{[]string{"mcp.acme.test"}, "https://other.test/", false},
		{nil, "https://mcp.acme.test/", false},
		{[]string{"mcp.acme.test"}, "://bad", false},
		{[]string{""}, "https:///path", false},
	}
	for _, c := range cases {
		if got := destinationAllowed(c.hosts, c.destination); got != c.want {
			t.Errorf("destinationAllowed(%q, %q) = %v, want %v", c.hosts, c.destination, got, c.want)
		}
	}
}

func TestConfigSecretsHostRuleAcceptsTrailingDot(t *testing.T) {
	t.Setenv("FUSE_TEST_HR", secretValue)
	s := ConfigSecrets([]config.SecretSourceConfig{{Name: "tok", Env: "FUSE_TEST_HR", Hosts: []string{"mcp.acme.test"}}})
	v, err := s.ResolveSecret(context.Background(), "", "tok", "https://mcp.acme.test./")
	if err != nil || v != secretValue {
		t.Fatalf("trailing-dot destination: got (%q, %v), want the value", v, err)
	}
	if _, err := s.ResolveSecret(context.Background(), "", "tok", "https://other.test/"); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("other host: err = %v, want ErrSecretUnavailable", err)
	}
}
