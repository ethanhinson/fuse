package main

import (
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
)

func TestMCPsListEmpty(t *testing.T) {
	var out strings.Builder
	var errOut strings.Builder
	code := runMCPs([]string{"list"}, config.Config{}, &out, &errOut)
	if code != 0 {
		t.Errorf("exit %d, stderr: %s", code, errOut.String())
	}
}

func TestMCPsListStaticEntries(t *testing.T) {
	cfg := config.Config{
		MCPServers: []config.MCPServerConfig{
			{Name: "filesystem", Transport: "stdio", Command: []string{"cmd"}},
			{Name: "brave", Transport: "http", URL: "http://localhost:9000", Auth: config.MCPAuthConfig{Type: "bearer"}},
		},
	}
	var out strings.Builder
	code := runMCPs([]string{"list"}, cfg, &out, &strings.Builder{})
	if code != 0 {
		t.Errorf("exit %d", code)
	}
	if !strings.Contains(out.String(), "filesystem") {
		t.Error("output should contain 'filesystem'")
	}
	if !strings.Contains(out.String(), "brave") {
		t.Error("output should contain 'brave'")
	}
	if !strings.Contains(out.String(), "bearer") {
		t.Error("output should contain 'bearer'")
	}
}

func TestMCPsAddMissingName(t *testing.T) {
	var out, errOut strings.Builder
	code := runMCPs([]string{"add", "--transport", "stdio"}, config.Config{}, &out, &errOut)
	if code == 0 {
		t.Error("expected non-zero exit when --name is missing")
	}
	if !strings.Contains(errOut.String(), "--name") {
		t.Errorf("expected error about --name, got: %s", errOut.String())
	}
}

func TestMCPsRemoveMissingName(t *testing.T) {
	var out, errOut strings.Builder
	code := runMCPs([]string{"remove"}, config.Config{}, &out, &errOut)
	if code == 0 {
		t.Error("expected non-zero exit when name is missing")
	}
}

func TestMCPsUnknownSubcommand(t *testing.T) {
	var out, errOut strings.Builder
	code := runMCPs([]string{"bogus"}, config.Config{}, &out, &errOut)
	if code == 0 {
		t.Error("expected non-zero exit for unknown subcommand")
	}
	if !strings.Contains(errOut.String(), "bogus") {
		t.Errorf("expected error to mention subcommand, got: %s", errOut.String())
	}
}
