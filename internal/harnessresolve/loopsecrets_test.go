package harnessresolve

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/harness"
	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/loopsecret"
)

const loopValue = "loop-VALUE-never-shown"

// loopMCPSpec is one bearer MCP server at url whose secret comes from ref.
func loopMCPSpec(url, ref string) *harnessv1.HarnessSpec {
	return &harnessv1.HarnessSpec{McpServers: []*harnessv1.McpServer{loopMCPServer("s", url, ref)}}
}

func loopMCPServer(name, url, ref string) *harnessv1.McpServer {
	return &harnessv1.McpServer{
		Name: name, Transport: harnessv1.McpTransport_MCP_TRANSPORT_HTTP, Url: url,
		Auth: &harnessv1.McpAuth{Type: harnessv1.McpAuthType_MCP_AUTH_TYPE_BEARER, ClientSecretRef: ref},
	}
}

func withLoop(ss ...loopsecret.Secret) context.Context {
	return loopsecret.WithSecrets(context.Background(), ss)
}

func TestLoopSecretResolvesForAListedHost(t *testing.T) {
	r := Resolver{Base: operator(t), Secrets: mapSecrets{}}
	ctx := withLoop(loopsecret.Secret{Name: "my-token", Value: loopValue, Hosts: []string{"mcp.acme.test"}})
	res, err := r.Resolve(ctx, "acme", loopMCPSpec("https://mcp.acme.test/", "my-token"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Config.MCPServers[0].Auth.ClientSecret != loopValue {
		t.Fatal("the loop secret was not resolved into the config")
	}
}

func TestLoopSecretHostsMatchCanonically(t *testing.T) {
	r := Resolver{Base: operator(t)}
	ctx := withLoop(loopsecret.Secret{Name: "my-token", Value: loopValue, Hosts: []string{"MCP.Acme.Test."}})
	res, err := r.Resolve(ctx, "acme", loopMCPSpec("https://mcp.acme.test:8443/x", "my-token"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Config.MCPServers[0].Auth.ClientSecret != loopValue {
		t.Fatal("the loop secret was not resolved into the config")
	}
}

func TestLoopSecretRefusedForAnUnlistedHost(t *testing.T) {
	r := Resolver{Base: operator(t), Secrets: mapSecrets{}}
	ctx := withLoop(loopsecret.Secret{Name: "my-token", Value: loopValue, Hosts: []string{"other.test"}})
	_, err := r.Resolve(ctx, "acme", loopMCPSpec("https://mcp.acme.test/", "my-token"))
	vs := violations(t, err)
	if len(vs) != 1 {
		t.Fatalf("violations = %+v, want exactly one", vs)
	}
	v := vs[0]
	if v.FieldPath != "mcp_servers[0].auth.client_secret_ref" || v.RuleID != harness.RuleSecretRef {
		t.Fatalf("violation = %+v", v)
	}
	want := `loop secret "my-token" may only be sent to the hosts it lists, and "mcp.acme.test" is not one of them`
	if v.Message != want {
		t.Fatalf("message = %q, want %q", v.Message, want)
	}
}

func TestOperatorSecretWinsOverLoopSecret(t *testing.T) {
	r := Resolver{Base: operator(t), Secrets: mapSecrets{"acme/my-token": secretValue}}
	ctx := withLoop(loopsecret.Secret{Name: "my-token", Value: loopValue, Hosts: []string{"mcp.acme.test"}})
	res, err := r.Resolve(ctx, "acme", loopMCPSpec("https://mcp.acme.test/", "my-token"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := res.Config.MCPServers[0].Auth.ClientSecret; got != secretValue {
		t.Fatalf("resolved the loop value over the operator's (loop=%v)", got == loopValue)
	}
}

func TestLoopSecretFallsBackWhenOperatorRefuses(t *testing.T) {
	r := Resolver{Base: operator(t), Secrets: mapSecrets{"other-tenant/my-token": secretValue}}
	ctx := withLoop(loopsecret.Secret{Name: "my-token", Value: loopValue, Hosts: []string{"mcp.acme.test"}})
	res, err := r.Resolve(ctx, "acme", loopMCPSpec("https://mcp.acme.test/", "my-token"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Config.MCPServers[0].Auth.ClientSecret != loopValue {
		t.Fatal("the loop secret was not used after the operator refused")
	}
}

func TestLoopSecretFallsBackWithoutAnOperatorResolver(t *testing.T) {
	r := Resolver{Base: operator(t)} // Secrets nil: every operator reference is denied
	ctx := withLoop(loopsecret.Secret{Name: "my-token", Value: loopValue, Hosts: []string{"mcp.acme.test"}})
	res, err := r.Resolve(ctx, "acme", loopMCPSpec("https://mcp.acme.test/", "my-token"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Config.MCPServers[0].Auth.ClientSecret != loopValue {
		t.Fatal("the loop secret was not used without an operator resolver")
	}
}

func TestLoopSecretHostsMustBeBareHostnames(t *testing.T) {
	const v = "host-SENTINEL"
	bad := []string{"mcp.acme.test:443", "https://x", "*.acme.test", "a b.test", "tab\t.test", "a/b"}
	var ss []loopsecret.Secret
	for i, h := range bad {
		ss = append(ss, loopsecret.Secret{Name: fmt.Sprintf("s%d", i), Value: v, Hosts: []string{"ok.test", h}})
	}
	_, err := Resolver{Base: operator(t)}.Resolve(withLoop(ss...), "acme", &harnessv1.HarnessSpec{})
	got := violations(t, err)
	if len(got) != len(bad) {
		t.Fatalf("got %d violations, want %d: %+v", len(got), len(bad), got)
	}
	for i, vi := range got {
		if want := fmt.Sprintf("secrets[%d].hosts[1]", i); vi.FieldPath != want {
			t.Errorf("violation %d path = %q, want %q", i, vi.FieldPath, want)
		}
		if vi.RuleID != harness.RuleSecretRef {
			t.Errorf("violation %d rule = %q", i, vi.RuleID)
		}
		if vi.Message != "must be a bare hostname (no scheme, port, path, wildcard or whitespace)" {
			t.Errorf("violation %d message = %q", i, vi.Message)
		}
		if strings.Contains(vi.Message, v) {
			t.Errorf("a loop secret value leaked into %q", vi.Message)
		}
	}
	if strings.Contains(err.Error(), v) {
		t.Fatalf("a loop secret value leaked into the error: %v", err)
	}
}

func TestLoopSecretEntryViolations(t *testing.T) {
	const v, h = "entry-SENTINEL", "h.test"
	ctx := withLoop(
		loopsecret.Secret{Name: "", Value: v + "0", Hosts: []string{h}},
		loopsecret.Secret{Name: "bad name!", Value: v + "1", Hosts: []string{h}},
		loopsecret.Secret{Name: "ok", Value: "", Hosts: []string{h}},
		loopsecret.Secret{Name: "nohosts", Value: v + "3", Hosts: nil},
		loopsecret.Secret{Name: "blankhost", Value: v + "4", Hosts: []string{"h", ""}},
		loopsecret.Secret{Name: "dup", Value: v + "5", Hosts: []string{h}},
		loopsecret.Secret{Name: "dup", Value: v + "6", Hosts: []string{h}},
	)
	_, err := Resolver{Base: operator(t)}.Resolve(ctx, "acme", &harnessv1.HarnessSpec{})
	got := violations(t, err)
	type pr struct{ path, rule string }
	var paths []pr
	for _, vi := range got {
		paths = append(paths, pr{vi.FieldPath, vi.RuleID})
		if strings.Contains(vi.Message, v) {
			t.Errorf("a loop secret value leaked into %q", vi.Message)
		}
	}
	want := []pr{
		{"secrets[0].name", harness.RuleSecretRef},
		{"secrets[1].name", harness.RuleSecretRef},
		{"secrets[2].value", harness.RuleSecretRef},
		{"secrets[3].hosts", harness.RuleSecretRef},
		{"secrets[4].hosts[1]", harness.RuleSecretRef},
		{"secrets[6].name", harness.RuleSecretRef},
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("violations =\n %v\nwant\n %v\nfull: %+v", paths, want, got)
	}
	if strings.Contains(err.Error(), v) {
		t.Fatalf("a loop secret value leaked into the error: %v", err)
	}
	msgs := map[string]string{}
	for _, vi := range got {
		msgs[vi.FieldPath] = vi.Message
	}
	wantMsgs := map[string]string{
		"secrets[0].name":     "must match " + config.SecretRefPattern,
		"secrets[2].value":    "must not be empty",
		"secrets[3].hosts":    "a loop secret must list the hosts its value may be sent to",
		"secrets[4].hosts[1]": "must not be empty",
		"secrets[6].name":     "duplicates the name of an earlier loop secret",
	}
	for path, m := range wantMsgs {
		if msgs[path] != m {
			t.Errorf("%s message = %q, want %q", path, msgs[path], m)
		}
	}
}

func TestInvalidLoopSecretIsNotUsed(t *testing.T) {
	ctx := withLoop(loopsecret.Secret{Name: "my-token", Value: loopValue, Hosts: nil})
	_, err := Resolver{Base: operator(t)}.Resolve(ctx, "acme", loopMCPSpec("https://mcp.acme.test/", "my-token"))
	msgs := map[string]string{}
	for _, v := range violations(t, err) {
		msgs[v.FieldPath] = v.Message
	}
	if _, ok := msgs["secrets[0].hosts"]; !ok {
		t.Errorf("no violation at secrets[0].hosts: %v", msgs)
	}
	if got := msgs["mcp_servers[0].auth.client_secret_ref"]; got != `secret "my-token" is not available to this tenant` {
		t.Errorf("ref message = %q", got)
	}
}

func TestADuplicateLoopSecretIsNotUsed(t *testing.T) {
	// The first entry is invalid (empty value), so the later duplicate must not
	// stand in for it: neither entry resolves anything.
	ctx := withLoop(
		loopsecret.Secret{Name: "my-token", Value: "", Hosts: []string{"mcp.acme.test"}},
		loopsecret.Secret{Name: "my-token", Value: loopValue, Hosts: []string{"mcp.acme.test"}},
	)
	_, err := Resolver{Base: operator(t)}.Resolve(ctx, "acme", loopMCPSpec("https://mcp.acme.test/", "my-token"))
	msgs := map[string]string{}
	for _, v := range violations(t, err) {
		msgs[v.FieldPath] = v.Message
	}
	if got := msgs["mcp_servers[0].auth.client_secret_ref"]; got != `secret "my-token" is not available to this tenant` {
		t.Errorf("ref message = %q (msgs %v)", got, msgs)
	}
	if _, ok := msgs["secrets[1].name"]; !ok {
		t.Errorf("no duplicate violation at secrets[1].name: %v", msgs)
	}
}

// TestOneNameNeverMixesOperatorAndLoopValues pins that a name resolved from
// the operator for one reference and from a loop secret for another is
// refused. The overlay substitutes values by name, so letting both through
// would send the operator's value to the loop secret's host (disclosing it to
// the client) or the client's value to a host its entry does not list.
func TestOneNameNeverMixesOperatorAndLoopValues(t *testing.T) {
	t.Setenv("FUSE_TEST_MIX", secretValue)
	r := Resolver{Base: operator(t), Secrets: ConfigSecrets([]config.SecretSourceConfig{{
		Name: "tok", Env: "FUSE_TEST_MIX", Tenants: []string{"acme"}, Hosts: []string{"op.acme.test"},
	}})}
	ctx := withLoop(loopsecret.Secret{Name: "tok", Value: loopValue, Hosts: []string{"loop.acme.test"}})
	for name, servers := range map[string][]*harnessv1.McpServer{
		"operator first": {loopMCPServer("a", "https://op.acme.test/", "tok"), loopMCPServer("b", "https://loop.acme.test/", "tok")},
		"loop first":     {loopMCPServer("a", "https://loop.acme.test/", "tok"), loopMCPServer("b", "https://op.acme.test/", "tok")},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := r.Resolve(ctx, "acme", &harnessv1.HarnessSpec{McpServers: servers})
			if err == nil {
				t.Fatalf("resolved %q and %q for one name across sources",
					redactedPick(res.Config.MCPServers[0].Auth.ClientSecret), redactedPick(res.Config.MCPServers[1].Auth.ClientSecret))
			}
			paths := map[string]string{}
			for _, v := range violations(t, err) {
				paths[v.FieldPath] = v.Message
				if v.RuleID != harness.RuleSecretRef {
					t.Errorf("rule = %q at %s", v.RuleID, v.FieldPath)
				}
			}
			for _, p := range []string{"mcp_servers[0].auth.client_secret_ref", "mcp_servers[1].auth.client_secret_ref"} {
				if _, ok := paths[p]; !ok {
					t.Errorf("no violation at %s: %v", p, paths)
				}
			}
			if s := err.Error(); strings.Contains(s, secretValue) || strings.Contains(s, loopValue) {
				t.Fatalf("a value leaked into the error: %v", err)
			}
		})
	}
}

// redactedPick names which value a config field holds without printing it.
func redactedPick(v string) string {
	switch v {
	case secretValue:
		return "operator value"
	case loopValue:
		return "loop value"
	}
	return "other"
}

func TestLoopSecretResolvesAResearchHeader(t *testing.T) {
	spec := &harnessv1.HarnessSpec{Research: &harnessv1.Research{Custom: &harnessv1.CustomSearchProvider{
		Url:     "https://search.acme.test/",
		Headers: map[string]*harnessv1.HeaderValue{"X-Api-Key": {Value: &harnessv1.HeaderValue_SecretRef{SecretRef: "hdr"}}},
	}}}
	ctx := withLoop(loopsecret.Secret{Name: "hdr", Value: loopValue, Hosts: []string{"search.acme.test"}})
	res, err := Resolver{Base: operator(t)}.Resolve(ctx, "acme", spec)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Config.Research.Custom.Headers["X-Api-Key"] != loopValue {
		t.Fatal("the loop secret was not resolved into the research header")
	}

	ctx = withLoop(loopsecret.Secret{Name: "hdr", Value: loopValue, Hosts: []string{"mcp.acme.test"}})
	_, err = Resolver{Base: operator(t)}.Resolve(ctx, "acme", spec)
	vs := violations(t, err)
	if len(vs) != 1 || vs[0].FieldPath != `research.custom.headers["X-Api-Key"].secret_ref` || vs[0].RuleID != harness.RuleSecretRef {
		t.Fatalf("unlisted research host: %+v", vs)
	}
}

func TestNoViolationOrErrorEchoesALoopSecretValue(t *testing.T) {
	const sentinel = "ECHO-SENTINEL"
	cases := map[string]struct {
		ss   []loopsecret.Secret
		spec *harnessv1.HarnessSpec
	}{
		"unlisted host": {
			ss:   []loopsecret.Secret{{Name: "my-token", Value: sentinel + "-a", Hosts: []string{"other.test"}}},
			spec: loopMCPSpec("https://mcp.acme.test/", "my-token"),
		},
		"no hosts": {
			ss:   []loopsecret.Secret{{Name: "my-token", Value: sentinel + "-b"}},
			spec: loopMCPSpec("https://mcp.acme.test/", "my-token"),
		},
		"bad name": {
			ss:   []loopsecret.Secret{{Name: "bad name!", Value: sentinel + "-c", Hosts: []string{"mcp.acme.test"}}},
			spec: loopMCPSpec("https://mcp.acme.test/", "my-token"),
		},
		"blank host and duplicate": {
			ss: []loopsecret.Secret{
				{Name: "my-token", Value: sentinel + "-d", Hosts: []string{""}},
				{Name: "my-token", Value: sentinel + "-e", Hosts: []string{"mcp.acme.test"}},
			},
			spec: loopMCPSpec("https://mcp.acme.test/", "my-token"),
		},
		"unparsable destination": {
			ss:   []loopsecret.Secret{{Name: "my-token", Value: sentinel + "-f", Hosts: []string{"mcp.acme.test"}}},
			spec: loopMCPSpec("://bad", "my-token"),
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Resolver{Base: operator(t)}.Resolve(withLoop(c.ss...), "acme", c.spec)
			if err == nil {
				t.Fatal("Resolve succeeded, want a violation")
			}
			if strings.Contains(err.Error(), sentinel) {
				t.Fatalf("a value leaked into the error: %v", err)
			}
			for _, v := range violations(t, err) {
				if strings.Contains(v.Message, sentinel) || strings.Contains(v.FieldPath, sentinel) {
					t.Fatalf("a value leaked into %+v", v)
				}
			}
		})
	}
}
