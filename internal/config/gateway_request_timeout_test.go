package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gateway.request_timeout is a per-deployment knob for the adapter's
// per-attempt deadline. Empty ⇒ unset (the adapter default); a valid duration
// is carried verbatim; garbage or a non-positive value is a load error, not a
// silent fallback.
func TestGatewayRequestTimeoutDefaultsEmpty(t *testing.T) {
	c := loadHomeConfig(t, "gateway:\n  url: http://example:5000/v1\n")
	if c.Gateway.RequestTimeout != "" {
		t.Fatalf("request_timeout = %q, want empty", c.Gateway.RequestTimeout)
	}
}

func TestGatewayRequestTimeoutParsed(t *testing.T) {
	c := loadHomeConfig(t, "gateway:\n  request_timeout: 20m\n")
	if c.Gateway.RequestTimeout != "20m" {
		t.Fatalf("request_timeout = %q, want 20m", c.Gateway.RequestTimeout)
	}
}

func TestGatewayRequestTimeoutRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"soon", "-5m", "0s"} {
		home := filepath.Join(t.TempDir(), "home")
		if err := os.MkdirAll(filepath.Join(home, ".fuse"), 0o755); err != nil {
			t.Fatal(err)
		}
		body := "gateway:\n  request_timeout: \"" + bad + "\"\n"
		if err := os.WriteFile(filepath.Join(home, ".fuse", "config.yml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOME", home)
		os.Unsetenv("LLM_GATEWAY_URL")
		os.Unsetenv("LLM_GATEWAY_KEY")
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "request_timeout") {
			t.Errorf("request_timeout %q: err = %v, want a request_timeout load error", bad, err)
		}
	}
}

// The repo-local file may not set it: a repo could otherwise stretch a hung request
// for as long as it likes.
func TestGatewayRequestTimeoutIgnoredFromLocalFile(t *testing.T) {
	cwd := chdirTemp(t)
	if err := os.WriteFile(filepath.Join(cwd, ".fuse.local.yml"), []byte("gateway:\n  request_timeout: 15m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := loadHomeConfig(t, "gateway:\n  url: http://example:5000/v1\n  request_timeout: 5m\n")
	if c.Gateway.RequestTimeout != "5m" {
		t.Fatalf("request_timeout = %q, want the home file's 5m", c.Gateway.RequestTimeout)
	}
}
