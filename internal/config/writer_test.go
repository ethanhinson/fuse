package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAddMCPServerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	origHome := os.Getenv("HOME")
	t.Setenv("HOME", dir)
	defer os.Setenv("HOME", origHome)

	srv := MCPServerConfig{Name: "test-server", Transport: "stdio", Command: []string{"npx", "-y", "some-mcp"}}
	if err := AddMCPServer(srv); err != nil {
		t.Fatalf("AddMCPServer: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load after add: %v", err)
	}
	var found bool
	for _, s := range cfg.MCPServers {
		if s.Name == "test-server" {
			found = true
			if s.Transport != "stdio" {
				t.Errorf("transport = %q, want stdio", s.Transport)
			}
		}
	}
	if !found {
		t.Error("added server not found in config")
	}
}

func TestAddMCPServerReplace(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	srv1 := MCPServerConfig{Name: "srv", Transport: "stdio", Command: []string{"cmd1"}}
	if err := AddMCPServer(srv1); err != nil {
		t.Fatalf("first add: %v", err)
	}

	srv2 := MCPServerConfig{Name: "srv", Transport: "http", URL: "http://localhost:8080"}
	if err := AddMCPServer(srv2); err != nil {
		t.Fatalf("replace: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	count := 0
	for _, s := range cfg.MCPServers {
		if s.Name == "srv" {
			count++
			if s.Transport != "http" {
				t.Errorf("transport not updated: %q", s.Transport)
			}
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 server named 'srv', got %d", count)
	}
}

func TestRemoveMCPServer(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	for _, name := range []string{"alpha", "beta", "gamma"} {
		if err := AddMCPServer(MCPServerConfig{Name: name, Transport: "stdio", Command: []string{"cmd"}}); err != nil {
			t.Fatalf("add %s: %v", name, err)
		}
	}

	if err := RemoveMCPServer("beta"); err != nil {
		t.Fatalf("RemoveMCPServer: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, s := range cfg.MCPServers {
		if s.Name == "beta" {
			t.Error("beta should have been removed")
		}
	}
	if len(cfg.MCPServers) != 2 {
		t.Errorf("want 2 servers after remove, got %d", len(cfg.MCPServers))
	}
}

func TestRemoveMCPServerNoop(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Remove from empty config is a no-op.
	if err := RemoveMCPServer("nonexistent"); err != nil {
		t.Errorf("RemoveMCPServer nonexistent: %v", err)
	}
}

func TestWriteAtomic(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	srv := MCPServerConfig{Name: "atomic-test", Transport: "stdio", Command: []string{"cmd"}}
	if err := AddMCPServer(srv); err != nil {
		t.Fatalf("AddMCPServer: %v", err)
	}

	// Verify the file was written to the expected path.
	path := filepath.Join(dir, ".fuse", "config.yml")
	if _, err := os.Stat(path); err != nil {
		t.Errorf("config file not found at %s: %v", path, err)
	}
}
