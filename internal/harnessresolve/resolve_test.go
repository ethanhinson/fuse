package harnessresolve

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/harness"
	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/config"
)

const secretValue = "s3cr3t-VALUE-never-shown"

func operator(t *testing.T) config.Config {
	t.Helper()
	c, err := config.FromYAML([]byte(`
models: {default: fast, fast: {id: cloud/fast}, house: {id: cloud/house}}
throughput: {requests_per_minute: 100}
workflows: {research: {pool: {concurrent: 4}}}
`), "operator")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type mapSecrets map[string]string

func (m mapSecrets) ResolveSecret(_ context.Context, tenant, name string) (string, error) {
	if v, ok := m[tenant+"/"+name]; ok {
		return v, nil
	}
	return "", ErrSecretUnavailable
}

func installed(names ...string) func(string) bool {
	return func(n string) bool {
		for _, s := range names {
			if s == n {
				return true
			}
		}
		return false
	}
}

func violations(t *testing.T, err error) []harness.Violation {
	t.Helper()
	var ve *harness.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want *harness.ValidationError", err)
	}
	return ve.Violations
}

func TestResolveLayersAValidSpec(t *testing.T) {
	turns := uint32(1)
	spec := &harnessv1.HarnessSpec{
		Models:     &harnessv1.Models{DefaultAlias: "house"}, // an operator-defined alias
		MaxTurns:   &turns,
		Skills:     []*harnessv1.SkillRef{{Name: "research"}},
		Throughput: &harnessv1.Throughput{RequestsPerMinute: 10},
		McpServers: []*harnessv1.McpServer{{
			Name: "s", Transport: harnessv1.McpTransport_MCP_TRANSPORT_HTTP, Url: "https://mcp.example.com",
			Auth: &harnessv1.McpAuth{Type: harnessv1.McpAuthType_MCP_AUTH_TYPE_BEARER, ClientSecretRef: "tok"},
		}},
	}
	r := Resolver{Base: operator(t), Skills: installed("research"), Secrets: mapSecrets{"acme/tok": secretValue}}
	got, err := r.Resolve(context.Background(), "acme", spec)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	c := got.Config
	if c.Models.Default != "house" || *c.MaxTurns != 1 || c.Throughput.RequestsPerMinute != 10 {
		t.Errorf("resolved = %+v %v %+v", c.Models, c.MaxTurns, c.Throughput)
	}
	if c.MCPServers[0].Auth.ClientSecret != secretValue {
		t.Errorf("secret not resolved into the config")
	}
	if !reflect.DeepEqual(got.Skills, []string{"research"}) {
		t.Errorf("skills = %v", got.Skills)
	}
	if got.Digest != Digest(spec) || !strings.HasPrefix(got.Digest, "sha256:") || len(got.Digest) != len("sha256:")+64 {
		t.Errorf("digest = %q", got.Digest)
	}
}

func TestResolveReportsEveryViolationWithPaths(t *testing.T) {
	spec := &harnessv1.HarnessSpec{
		Models:     &harnessv1.Models{DefaultAlias: "ghost"},
		Skills:     []*harnessv1.SkillRef{{Name: "research"}, {Name: "nope"}},
		Throughput: &harnessv1.Throughput{RequestsPerMinute: 500},
		Workflows:  map[string]*harnessv1.Workflow{"research": {Pool: &harnessv1.Pool{Concurrent: 9}}},
		Context:    &harnessv1.Context{Summarization: &harnessv1.Summarization{Threshold: 7}},
		McpServers: []*harnessv1.McpServer{{
			Name: "s", Transport: harnessv1.McpTransport_MCP_TRANSPORT_HTTP, Url: "https://mcp.example.com",
			Auth: &harnessv1.McpAuth{Type: harnessv1.McpAuthType_MCP_AUTH_TYPE_BEARER, ClientSecretRef: "tok"},
		}},
		Research: &harnessv1.Research{Custom: &harnessv1.CustomSearchProvider{
			Url:     "https://search.example.com",
			Headers: map[string]*harnessv1.HeaderValue{"X-Api-Key": {Value: &harnessv1.HeaderValue_SecretRef{SecretRef: "other"}}},
		}},
	}
	// The secret exists, but for another tenant: refused the same way as a missing one.
	r := Resolver{Base: operator(t), Skills: installed("research"), Secrets: mapSecrets{"other-tenant/tok": secretValue}}
	_, err := r.Resolve(context.Background(), "acme", spec)
	got := violations(t, err)
	type pr struct{ path, rule string }
	var paths []pr
	for _, v := range got {
		paths = append(paths, pr{v.FieldPath, v.RuleID})
		if strings.Contains(v.Message, secretValue) {
			t.Errorf("a secret value leaked into %q", v.Message)
		}
	}
	want := []pr{
		{"context.summarization.threshold", "double.gte_lte"},
		{"mcp_servers[0].auth.client_secret_ref", harness.RuleSecretRef},
		{"models.default_alias", harness.RuleModelRef},
		{`research.custom.headers["X-Api-Key"].secret_ref`, harness.RuleSecretRef},
		{"skills[1].name", harness.RuleSkillRef},
		{"throughput.requests_per_minute", harness.RuleTightenOnly},
		{`workflows["research"].pool.concurrent`, harness.RuleTightenOnly},
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("violations =\n %v\nwant\n %v\nfull: %+v", paths, want, got)
	}
	for _, v := range got {
		switch v.FieldPath {
		case "throughput.requests_per_minute":
			if v.Message != "may only tighten this server's limit of 100" {
				t.Errorf("tighten message = %q", v.Message)
			}
		case "mcp_servers[0].auth.client_secret_ref":
			if v.Message != `secret "tok" is not available to this tenant` {
				t.Errorf("secret message = %q", v.Message)
			}
		case "skills[1].name":
			if v.Message != `skill "nope" is not installed on this server` {
				t.Errorf("skill message = %q", v.Message)
			}
		}
	}
}

func TestResolveWithoutSecretResolverDeniesEveryRef(t *testing.T) {
	spec := &harnessv1.HarnessSpec{McpServers: []*harnessv1.McpServer{{
		Name: "s", Transport: harnessv1.McpTransport_MCP_TRANSPORT_HTTP, Url: "https://mcp.example.com",
		Auth: &harnessv1.McpAuth{Type: harnessv1.McpAuthType_MCP_AUTH_TYPE_BEARER, ClientSecretRef: "tok"},
	}}}
	_, err := Resolver{Base: config.Default()}.Resolve(context.Background(), "", spec)
	got := violations(t, err)
	if len(got) != 1 || got[0].RuleID != harness.RuleSecretRef {
		t.Fatalf("violations = %+v", got)
	}
}

func TestResolveNilAndEmptySpecIsTheBase(t *testing.T) {
	base := operator(t)
	for _, spec := range []*harnessv1.HarnessSpec{nil, {}} {
		got, err := Resolver{Base: base}.Resolve(context.Background(), "", spec)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Config, base) || got.Skills != nil {
			t.Fatalf("resolved %+v", got)
		}
	}
}

func TestDigestIsStableAndContentAddressed(t *testing.T) {
	a := &harnessv1.HarnessSpec{Workflows: map[string]*harnessv1.Workflow{"a": {Skill: "x"}, "b": {Skill: "y"}, "c": {Skill: "z"}}}
	for i := 0; i < 20; i++ {
		if Digest(a) != Digest(a) {
			t.Fatal("digest is not deterministic over map order")
		}
	}
	b := &harnessv1.HarnessSpec{Workflows: map[string]*harnessv1.Workflow{"a": {Skill: "x"}}}
	if Digest(a) == Digest(b) {
		t.Fatal("different specs share a digest")
	}
	if Digest(nil) != Digest(&harnessv1.HarnessSpec{}) {
		t.Fatal("nil and empty spec differ")
	}
}

func TestConfigSecrets(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "gh")
	if err := os.WriteFile(file, []byte(secretValue+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FUSE_TEST_SECRET", secretValue)
	s := ConfigSecrets([]config.SecretSourceConfig{
		{Name: "env-default", Env: "FUSE_TEST_SECRET"},
		{Name: "env-acme", Env: "FUSE_TEST_SECRET", Tenants: []string{"acme"}},
		{Name: "file", File: file, Tenants: []string{"acme", "_default"}},
		{Name: "unset-env", Env: "FUSE_TEST_SECRET_UNSET"},
		{Name: "missing-file", File: filepath.Join(dir, "absent")},
	})
	ctx := context.Background()
	ok := []struct{ tenant, name string }{
		{"", "env-default"}, {"_default", "env-default"}, {"acme", "env-acme"}, {"acme", "file"}, {"", "file"},
	}
	for _, c := range ok {
		v, err := s.ResolveSecret(ctx, c.tenant, c.name)
		if err != nil || v != secretValue {
			t.Errorf("ResolveSecret(%q, %q) = %q, %v", c.tenant, c.name, v, err)
		}
	}
	denied := []struct{ tenant, name string }{
		{"acme", "env-default"}, {"", "env-acme"}, {"other", "file"}, {"", "nope"}, {"", "unset-env"}, {"", "missing-file"},
	}
	for _, c := range denied {
		v, err := s.ResolveSecret(ctx, c.tenant, c.name)
		if !errors.Is(err, ErrSecretUnavailable) || v != "" {
			t.Errorf("ResolveSecret(%q, %q) = %q, %v; want ErrSecretUnavailable", c.tenant, c.name, v, err)
		}
		if err != nil && (strings.Contains(err.Error(), secretValue) || strings.Contains(err.Error(), dir)) {
			t.Errorf("error leaks a value or host path: %v", err)
		}
	}
}
