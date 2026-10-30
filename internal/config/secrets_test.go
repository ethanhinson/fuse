package config

import (
	"bytes"
	"strings"
	"testing"
)

func TestLoopServerSecretsLoadFromTrustedFileOnly(t *testing.T) {
	doc := []byte(`
loop_server:
  secrets:
    - {name: search-key, env: SEARCH_KEY, tenants: [acme]}
    - {name: github.token, file: /run/secrets/gh}
`)
	c, err := FromYAML(doc, "home")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.LoopServer.Secrets) != 2 || c.LoopServer.Secrets[0].Name != "search-key" ||
		c.LoopServer.Secrets[0].Env != "SEARCH_KEY" || c.LoopServer.Secrets[0].Tenants[0] != "acme" ||
		c.LoopServer.Secrets[1].File != "/run/secrets/gh" {
		t.Fatalf("secrets = %+v", c.LoopServer.Secrets)
	}

	var warn bytes.Buffer
	old := warnw
	warnw = &warn
	defer func() { warnw = old }()
	u := Default()
	if err := mergeData(&u, doc, ".fuse.local.yml", false, nil); err != nil {
		t.Fatal(err)
	}
	if len(u.LoopServer.Secrets) != 0 {
		t.Fatalf("an untrusted file declared secrets: %+v", u.LoopServer.Secrets)
	}
	if !strings.Contains(warn.String(), "loop_server") {
		t.Fatalf("no warning for an untrusted loop_server block: %q", warn.String())
	}
}

func TestLoopServerSecretsProblems(t *testing.T) {
	cases := []struct {
		secrets []SecretSourceConfig
		want    string
	}{
		{[]SecretSourceConfig{{Env: "X"}}, "loop_server.secrets[0]: name is required"},
		{[]SecretSourceConfig{{Name: "-bad", Env: "X"}}, `loop_server.secrets[0] ("-bad"): name must match ^[A-Za-z0-9][A-Za-z0-9_./-]*$`},
		{[]SecretSourceConfig{{Name: "a"}}, `loop_server.secrets[0] ("a"): exactly one of env or file is required`},
		{[]SecretSourceConfig{{Name: "a", Env: "X", File: "/f"}}, `loop_server.secrets[0] ("a"): exactly one of env or file is required`},
		{[]SecretSourceConfig{{Name: "a", Env: "X"}, {Name: "a", File: "/f"}}, `loop_server.secrets[1] ("a"): name "a" is already declared by loop_server.secrets[0]`},
		{[]SecretSourceConfig{{Name: "a", File: "relative"}}, `loop_server.secrets[0] ("a"): file must be an absolute path`},
	}
	for _, tc := range cases {
		c := Default()
		c.LoopServer.Secrets = tc.secrets
		ps := c.Problems()
		if len(ps) != 1 || ps[0].Path != "loop_server.secrets" || ps[0].Message != tc.want {
			t.Errorf("Problems() = %+v, want one loop_server.secrets problem %q", ps, tc.want)
		}
	}
	ok := Default()
	ok.LoopServer.Secrets = []SecretSourceConfig{{Name: "a", Env: "X"}, {Name: "b.c/d", File: "/f", Tenants: []string{"t"}}}
	if ps := ok.Problems(); len(ps) != 0 {
		t.Fatalf("valid secrets reported: %+v", ps)
	}
}
