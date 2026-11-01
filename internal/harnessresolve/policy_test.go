package harnessresolve

import (
	"context"
	"net"
	"os"
	"testing"

	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/config"
)

// TestMain keeps every test hermetic: a hostname resolves to a public
// address unless a test says otherwise.
func TestMain(m *testing.M) {
	lookupIP = func(_ context.Context, host string) ([]net.IP, error) {
		if host == "metadata.internal" || host == "rebind.example.com" {
			return []net.IP{net.ParseIP("10.0.0.5")}, nil
		}
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	os.Exit(m.Run())
}

func mcpSpec(urls ...string) *harnessv1.HarnessSpec {
	spec := &harnessv1.HarnessSpec{}
	for _, u := range urls {
		spec.McpServers = append(spec.McpServers, &harnessv1.McpServer{
			Name: "s" + u[len(u)-3:], Transport: harnessv1.McpTransport_MCP_TRANSPORT_HTTP, Url: u,
		})
	}
	return spec
}

func policyResolver(t *testing.T, p config.SpecPolicyConfig) Resolver {
	base := operator(t)
	base.LoopServer.SpecPolicy = p
	return Resolver{Base: base}
}

func refusedPaths(t *testing.T, r Resolver, spec *harnessv1.HarnessSpec) map[string]string {
	t.Helper()
	out := map[string]string{}
	_, err := r.Resolve(context.Background(), "acme", spec)
	if err == nil {
		return out
	}
	for _, v := range violations(t, err) {
		out[v.FieldPath] = v.RuleID
	}
	return out
}

func TestSpecPolicyDefaultDeniesPrivateNetworks(t *testing.T) {
	r := policyResolver(t, config.SpecPolicyConfig{})
	got := refusedPaths(t, r, mcpSpec("http://127.0.0.1:8080", "https://10.0.0.5/", "https://metadata.internal/", "https://mcp.example.com/", "https://rebind.example.com/"))
	for _, p := range []string{"mcp_servers[0].url", "mcp_servers[1].url", "mcp_servers[2].url", "mcp_servers[4].url"} {
		if got[p] != "fuse.operator_only" {
			t.Errorf("%s not refused: %v", p, got)
		}
	}
	if _, bad := got["mcp_servers[3].url"]; bad {
		t.Errorf("public https url refused: %v", got)
	}
}

func TestSpecPolicyHostAllowlistAppliesToMCPAndResearch(t *testing.T) {
	r := policyResolver(t, config.SpecPolicyConfig{MCPHosts: []string{"*.example.com"}})
	spec := mcpSpec("https://api.example.com/", "https://example.org/")
	spec.Research = &harnessv1.Research{Custom: &harnessv1.CustomSearchProvider{Url: "https://example.org/search"}}
	got := refusedPaths(t, r, spec)
	if _, bad := got["mcp_servers[0].url"]; bad {
		t.Errorf("allowed host refused: %v", got)
	}
	if got["mcp_servers[1].url"] == "" || got["research.custom.url"] == "" {
		t.Errorf("disallowed hosts not refused: %v", got)
	}
}

func TestSpecPolicyLocalDevCase(t *testing.T) {
	off := false
	r := policyResolver(t, config.SpecPolicyConfig{AllowHTTPHosts: []string{"localhost"}, DenyPrivateNetworks: &off})
	got := refusedPaths(t, r, mcpSpec("http://localhost:9000/", "http://169.254.169.254/"))
	if _, bad := got["mcp_servers[0].url"]; bad {
		t.Errorf("localhost refused: %v", got)
	}
	if got["mcp_servers[1].url"] == "" {
		t.Errorf("http to a non-allowed host accepted: %v", got)
	}
	// Even with private networks allowed, http outside allow_http_hosts is refused.
	on := true
	r = policyResolver(t, config.SpecPolicyConfig{AllowHTTPHosts: []string{"localhost"}, DenyPrivateNetworks: &on})
	if got := refusedPaths(t, r, mcpSpec("http://localhost:9000/")); got["mcp_servers[0].url"] == "" {
		t.Errorf("localhost accepted while private networks are denied: %v", got)
	}
}

func TestSpecPolicyRegistryOnlyModels(t *testing.T) {
	spec := func(ids ...string) *harnessv1.HarnessSpec {
		m := map[string]*harnessv1.Model{}
		for i, id := range ids {
			m[string(rune('x'+i))] = &harnessv1.Model{Id: id}
		}
		return &harnessv1.HarnessSpec{Models: &harnessv1.Models{Entries: m}}
	}
	r := policyResolver(t, config.SpecPolicyConfig{Models: config.SpecModelsRegistryOnly})
	got := refusedPaths(t, r, spec("cloud/fast", "gw/other"))
	if _, bad := got[`models.entries["x"].id`]; bad {
		t.Errorf("registry id refused: %v", got)
	}
	if got[`models.entries["y"].id`] != "fuse.operator_only" {
		t.Errorf("non-registry id accepted: %v", got)
	}
	r = policyResolver(t, config.SpecPolicyConfig{Models: config.SpecModelsAny})
	if got := refusedPaths(t, r, spec("cloud/fast", "gw/other")); len(got) != 0 {
		t.Errorf("models: any refused: %v", got)
	}
}

func TestSpecPolicyExemptsOperatorMCPServers(t *testing.T) {
	base := operator(t)
	base.MCPServers = []config.MCPServerConfig{{Name: "own", URL: "http://10.1.2.3:9000"}}
	r := Resolver{Base: base}
	got, err := r.Resolve(context.Background(), "acme", nil)
	if err != nil {
		t.Fatalf("operator server refused: %v", err)
	}
	if len(got.Config.MCPServers) != 1 {
		t.Errorf("servers = %+v", got.Config.MCPServers)
	}
}

func TestSpecPolicyLoadsOnlyFromTrustedConfig(t *testing.T) {
	c, err := config.FromYAML([]byte("loop_server:\n  spec_policy: {mcp_hosts: [a.example.com], models: registry-only}\n"), "operator")
	if err != nil {
		t.Fatal(err)
	}
	p := c.LoopServer.SpecPolicy
	if len(p.MCPHosts) != 1 || !p.RegistryOnly() || !p.DenyPrivate() {
		t.Errorf("policy = %+v", p)
	}
}
