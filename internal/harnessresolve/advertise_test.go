package harnessresolve

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/harnessconfig"
)

func TestSpecMCPAuthKindsAreTheOnesTheLayeringAccepts(t *testing.T) {
	want := []harnessv1.McpAuthType{harnessv1.McpAuthType_MCP_AUTH_TYPE_UNSPECIFIED, harnessv1.McpAuthType_MCP_AUTH_TYPE_BEARER, harnessv1.McpAuthType_MCP_AUTH_TYPE_STATIC}
	if got := SpecMCPAuthKinds(); !reflect.DeepEqual(got, want) {
		t.Fatalf("SpecMCPAuthKinds = %v", got)
	}
	for _, k := range want {
		if s, ok := harnessconfig.AuthTypeToConfig(k); !ok || !config.SpecMCPAuthTypes[s] {
			t.Errorf("advertised %v maps to %q, which the overlay drops", k, s)
		}
	}
	vals := harnessv1.McpAuthType(0).Descriptor().Values()
	for i := 0; i < vals.Len(); i++ {
		k := harnessv1.McpAuthType(vals.Get(i).Number())
		spec := &harnessv1.HarnessSpec{McpServers: []*harnessv1.McpServer{{Auth: &harnessv1.McpAuth{Type: k}}}}
		if refused := len(operatorOnlyProblems(spec)) > 0; specAuthAllowed(k) == refused {
			t.Errorf("%v: advertised=%v refused=%v", k, specAuthAllowed(k), refused)
		}
	}
}

// An enum number the descriptor does not declare maps to no config auth
// string, so it is neither advertised nor accepted.
func TestUndeclaredMCPAuthKindIsRefused(t *testing.T) {
	k := harnessv1.McpAuthType(99)
	if specAuthAllowed(k) {
		t.Fatalf("undeclared auth type %v is allowed", k)
	}
	spec := &harnessv1.HarnessSpec{McpServers: []*harnessv1.McpServer{{Auth: &harnessv1.McpAuth{Type: k}}}}
	if len(operatorOnlyProblems(spec)) != 1 {
		t.Fatalf("undeclared auth type %v not refused: %v", k, operatorOnlyProblems(spec))
	}
}

// The listing advertises for a tenant exactly what ResolveSecret resolves
// for it (unrestricted destination).
func TestSecretRefsAgreeWithResolveSecret(t *testing.T) {
	t.Setenv("FUSE_T_A", "a")
	entries := []config.SecretSourceConfig{{Name: "a", Env: "FUSE_T_A", Tenants: []string{"acme"}}, {Name: "b", Env: "FUSE_T_A"}, {Name: "c", Env: "FUSE_T_NOPE"}}
	s := ConfigSecrets(entries)
	for _, tenant := range []string{"", "acme", "other"} {
		listed := map[string]bool{}
		for _, n := range (Resolver{Secrets: s}).SecretRefs(context.Background(), tenant) {
			listed[n] = true
		}
		for _, e := range entries {
			_, err := s.ResolveSecret(context.Background(), tenant, e.Name, "https://anywhere.example.com")
			if (err == nil) != listed[e.Name] {
				t.Errorf("tenant %q %q: resolves=%v listed=%v", tenant, e.Name, err == nil, listed[e.Name])
			}
		}
	}
}

func TestSecretRefsFilterByTenant(t *testing.T) {
	t.Setenv("FUSE_T_DEFAULT", "v1")
	t.Setenv("FUSE_T_ACME", "v2")
	t.Setenv("FUSE_T_EMPTY", "")
	dir := t.TempDir()
	file := filepath.Join(dir, "both")
	if err := os.WriteFile(file, []byte("v3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := ConfigSecrets([]config.SecretSourceConfig{
		{Name: "zeta", Env: "FUSE_T_DEFAULT"},
		{Name: "acme-key", Env: "FUSE_T_ACME", Tenants: []string{"acme"}},
		{Name: "both", File: file, Tenants: []string{"acme", "_default"}},
		{Name: "pinned", Env: "FUSE_T_DEFAULT", Hosts: []string{"api.example.com"}},
		{Name: "unset", Env: "FUSE_T_EMPTY"},
		{Name: "gone", File: filepath.Join(dir, "missing")},
	})
	r := Resolver{Secrets: s}
	for _, tc := range []struct {
		tenant string
		want   []string
	}{
		{"", []string{"both", "pinned", "zeta"}},
		{"_default", []string{"both", "pinned", "zeta"}},
		{"acme", []string{"acme-key", "both"}},
		{"other", nil},
	} {
		got := r.SecretRefs(context.Background(), tc.tenant)
		if len(got) != len(tc.want) || (len(got) > 0 && !reflect.DeepEqual(got, tc.want)) {
			t.Errorf("SecretRefs(%q) = %v, want %v", tc.tenant, got, tc.want)
		}
	}

	// Listing a host-restricted name does not widen where it resolves: the
	// destination-host rule still applies on top of the listing rule.
	ctx := context.Background()
	if v, err := s.ResolveSecret(ctx, "", "pinned", "https://api.example.com/mcp"); err != nil || v != "v1" {
		t.Errorf("pinned to its host = %q, %v", v, err)
	}
	if v, err := s.ResolveSecret(ctx, "", "pinned", "https://elsewhere.example.com/mcp"); !errors.Is(err, ErrSecretUnavailable) || v != "" {
		t.Errorf("pinned to another host = %q, %v; want ErrSecretUnavailable", v, err)
	}
	if v, err := s.ResolveSecret(ctx, "acme", "both", "https://anywhere.example.com"); err != nil || v != "v3" {
		t.Errorf("file secret = %q, %v", v, err)
	}
}

// resolveOnly is a SecretResolver that cannot list.
type resolveOnly struct{}

func (resolveOnly) ResolveSecret(context.Context, string, string, string) (string, error) {
	return "value", nil
}

func TestSecretRefsWithoutAListerOrResolver(t *testing.T) {
	ctx := context.Background()
	if got := (Resolver{}).SecretRefs(ctx, ""); len(got) != 0 {
		t.Errorf("no resolver lists %v", got)
	}
	if got := (Resolver{Secrets: resolveOnly{}}).SecretRefs(ctx, ""); len(got) != 0 {
		t.Errorf("resolve-only resolver lists %v", got)
	}
}
