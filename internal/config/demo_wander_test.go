package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// demoConfigPath is the checked-in wander demo config (change #0060, task 6).
const demoConfigPath = "../../examples/wander/fuse.demo.yml"

// loadDemoConfig copies the checked-in demo config into a temp HOME as the
// TRUSTED ~/.fuse/config.yml and runs the real Load(). Using the trusted path is
// deliberate: loop_server.auth and tool_identity are credential surfaces that a
// repo-plantable .fuse.local.yml is not allowed to set (ADR-0006), so a demo
// operator must merge this file into their home config — and that is exactly the
// path this test exercises.
func loadDemoConfig(t *testing.T) Config {
	t.Helper()
	data, err := os.ReadFile(demoConfigPath)
	if err != nil {
		t.Fatalf("read demo config: %v", err)
	}
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, ".fuse", "config.yml"), data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("HOME", home)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load demo config: %v", err)
	}
	return cfg
}

// TestWanderDemoConfigDeclaresRentalsServer pins the demo config to the LITERAL
// MCP server shape #59's acceptance lane uses: transport sse, a url, an audience,
// and the identity auth tier. If the schema or the demo file drifts apart, this
// fails rather than the demo silently rotting.
func TestWanderDemoConfigDeclaresRentalsServer(t *testing.T) {
	cfg := loadDemoConfig(t)

	var srv *MCPServerConfig
	for i := range cfg.MCPServers {
		if cfg.MCPServers[i].Name == "rentals" {
			srv = &cfg.MCPServers[i]
		}
	}
	if srv == nil {
		t.Fatalf("demo config declares no MCP server named \"rentals\"; got %+v", cfg.MCPServers)
	}
	if srv.Transport != "sse" {
		t.Errorf("transport = %q, want \"sse\" (the literal spelling the #59 lane uses)", srv.Transport)
	}
	if srv.URL == "" {
		t.Errorf("url is empty; the demo must point at the cmd/rentals-mcp listen address")
	}
	// BASE url only (change 0060, task 8). The MCP HTTP transport appends the SSE and
	// message paths itself (internal/mcp/http_client.go: `{url}/sse`), so a url that
	// already carries the suffix dials `/sse/sse` and every rentals call fails with a
	// 404 on connect — which is exactly how the demo config shipped until the browser
	// identity lane drove it end-to-end. Nothing else in the suite catches it, so pin it.
	if strings.HasSuffix(strings.TrimRight(srv.URL, "/"), "/sse") {
		t.Errorf("url = %q must be the BASE url with no /sse suffix; the MCP transport appends it (a suffixed url dials /sse/sse → 404)", srv.URL)
	}
	if srv.Audience == "" {
		t.Errorf("audience is empty; an identity-tier server requires an RFC 8707 resource id")
	}
	if srv.Auth.Type != "identity" {
		t.Errorf("auth.type = %q, want \"identity\" (TierOAuth delegation)", srv.Auth.Type)
	}
	// The identity tier can only mint with a signing key, and the rentals server
	// derives its verification keys from the SAME key (RENTALS_SIGNING_KEY).
	if cfg.ToolIdentity.SigningKey == "" {
		t.Errorf("tool_identity.signing_key is empty; identity-propagating servers cannot mint")
	}
}

// TestWanderDemoConfigUserDirectory pins D5's user directory: at least two demo
// bearer tokens resolving to DISTINCT {tenant, subject} principals, so the UI's
// token picker has something to switch between and per-principal favorites are
// observably different.
func TestWanderDemoConfigUserDirectory(t *testing.T) {
	cfg := loadDemoConfig(t)

	if len(cfg.LoopServer.Auth) < 2 {
		t.Fatalf("loop_server.auth has %d entries, want >= 2 demo principals", len(cfg.LoopServer.Auth))
	}
	type principal struct{ tenant, subject string }
	principals := map[principal]bool{}
	tokens := map[string]bool{}
	for i, e := range cfg.LoopServer.Auth {
		if e.Token == "" {
			t.Errorf("auth[%d] has an empty token", i)
		}
		if e.Subject == "" {
			t.Errorf("auth[%d] (%q) has an empty subject", i, e.Token)
		}
		if tokens[e.Token] {
			t.Errorf("auth[%d] reuses token %q; each demo user needs its own", i, e.Token)
		}
		tokens[e.Token] = true
		principals[principal{e.Tenant, e.Subject}] = true
	}
	if len(principals) < 2 {
		t.Errorf("demo tokens resolve to %d distinct {tenant,subject} principals, want >= 2", len(principals))
	}
}
