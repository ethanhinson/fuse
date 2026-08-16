package config

import (
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
)

// runScriptPath is the one-command launcher for the wander demo. It must start the
// rentals MCP server itself — the demo's headline feature is the live rentals
// backend, and a run path that only starts loop-serve-net demos nothing.
const runScriptPath = "../../examples/wander/run.sh"

// runScriptDefault re-implements the shell default-assignment shape run.sh uses for
// its knobs:  NAME="${NAME:-value}".  Pinning the literal shape is the point: this
// test exists to catch the demo config and the launcher drifting apart, and it can
// only do that if it reads the value the launcher would actually export. (RE2 has no
// backreferences, so the two names are compared in Go rather than in the pattern.)
var runScriptDefault = regexp.MustCompile(`(?m)^([A-Z_]+)="\$\{([A-Z_]+):-([^}]*)\}"`)

func runScriptEnv(t *testing.T) map[string]string {
	t.Helper()
	data, err := os.ReadFile(runScriptPath)
	if err != nil {
		t.Fatalf("read %s: %v", runScriptPath, err)
	}
	out := map[string]string{}
	for _, m := range runScriptDefault.FindAllStringSubmatch(string(data), -1) {
		if m[1] != m[2] {
			continue
		}
		out[m[1]] = m[3]
	}
	return out
}

// TestWanderRunScriptStartsRentalsServer is the config-consistency gate between
// examples/wander/run.sh and examples/wander/fuse.demo.yml. run.sh is a shell
// launcher with no unit-testable surface, but the failure mode that actually bit
// the demo is not shell logic — it is the two sides disagreeing (or one side not
// starting at all). Both are assertable from here.
func TestWanderRunScriptStartsRentalsServer(t *testing.T) {
	data, err := os.ReadFile(runScriptPath)
	if err != nil {
		t.Fatalf("read %s: %v", runScriptPath, err)
	}
	script := string(data)

	if !strings.Contains(script, "./cmd/rentals-mcp") {
		t.Fatalf("run.sh never builds/starts ./cmd/rentals-mcp; the documented one-command run path would leave nothing listening for the rentals MCP server and every tool call would fail to connect")
	}
	// Whatever it starts, it must also tear down: an orphaned rentals server holds
	// :8091 and the next run fails to bind.
	if !strings.Contains(script, "rentals_pid") {
		t.Errorf("run.sh does not track a rentals pid; the cleanup trap cannot reap the server it started")
	}

	env := runScriptEnv(t)
	cfg := loadDemoConfig(t)

	var srv *MCPServerConfig
	for i := range cfg.MCPServers {
		if cfg.MCPServers[i].Name == "rentals" {
			srv = &cfg.MCPServers[i]
		}
	}
	if srv == nil {
		t.Fatalf("demo config declares no MCP server named \"rentals\"")
	}

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse demo rentals url %q: %v", srv.URL, err)
	}
	if got, want := env["RENTALS_ADDR"], u.Host; got != want {
		t.Errorf("run.sh RENTALS_ADDR = %q, want %q (mcp_servers[rentals].url host in fuse.demo.yml); a mismatch means fuse dials a port nothing is listening on", got, want)
	}
	if got, want := env["RENTALS_AUDIENCE"], srv.Audience; got != want {
		t.Errorf("run.sh RENTALS_AUDIENCE = %q, want %q (mcp_servers[rentals].audience); a mismatch rejects every delegated token", got, want)
	}
	if got, want := env["RENTALS_SIGNING_KEY"], cfg.ToolIdentity.SigningKey; got != want {
		t.Errorf("run.sh RENTALS_SIGNING_KEY = %q, want %q (tool_identity.signing_key); a mismatch makes every minted token unverifiable", got, want)
	}

	// RENTALS_TENANTS must name every tenant that can reach the server: each tenant
	// under loop_server.auth PLUS `_default` (an auth entry without `tenant:` also
	// normalizes to `_default`, and the UI's picker defaults to it).
	listed := map[string]bool{}
	for _, tn := range strings.Split(env["RENTALS_TENANTS"], ",") {
		if tn = strings.TrimSpace(tn); tn != "" {
			listed[tn] = true
		}
	}
	want := map[string]bool{"_default": true}
	for _, e := range cfg.LoopServer.Auth {
		tn := e.Tenant
		if strings.TrimSpace(tn) == "" {
			tn = "_default"
		}
		want[tn] = true
	}
	for tn := range want {
		if !listed[tn] {
			t.Errorf("run.sh RENTALS_TENANTS (%q) omits tenant %q; that principal's every rentals call would be unauthorized", env["RENTALS_TENANTS"], tn)
		}
	}

	// The demo must work with no search credential. "auto" degrades to canned data
	// when none is set; "live" would hard-require one.
	switch env["RENTALS_DATA"] {
	case "auto", "canned":
	default:
		t.Errorf("run.sh RENTALS_DATA = %q, want \"auto\" or \"canned\"; the demo must not require a search credential", env["RENTALS_DATA"])
	}
}
