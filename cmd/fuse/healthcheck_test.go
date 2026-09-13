package main

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// addrOf strips the scheme from an httptest server URL, leaving host:port for --addr.
func addrOf(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestHealthcheckExitsZeroOn2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	if code := runHealthcheck([]string{"--addr", addrOf(t, srv), "--path", "/readyz"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, errb.String())
	}
	// Silent on success: a probe that runs every few seconds must not log.
	if out.Len() != 0 || errb.Len() != 0 {
		t.Errorf("healthcheck printed on success: stdout=%q stderr=%q", out.String(), errb.String())
	}
}

func TestHealthcheckExitsOneOn503(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	if code := runHealthcheck([]string{"--addr", addrOf(t, srv)}, &out, &errb); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
}

func TestHealthcheckExitsOneOnConnectionRefused(t *testing.T) {
	// Bind then immediately close, so the port is (almost certainly) unused.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	var out, errb bytes.Buffer
	if code := runHealthcheck([]string{"--addr", addr, "--timeout", "2s"}, &out, &errb); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if errb.Len() == 0 {
		t.Errorf("a failing probe should say why on stderr")
	}
}

func TestHealthcheckExitsOneOnTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer func() { close(block); srv.Close() }()

	var out, errb bytes.Buffer
	if code := runHealthcheck([]string{"--addr", addrOf(t, srv), "--timeout", "50ms"}, &out, &errb); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
}

// TestHealthcheckDefaultAddrMatchesServer pins the no-argument default against
// the loop-serve-net default: the compose stack runs `["/fuse","healthcheck"]`
// with no args against a server started with the same image.
func TestHealthcheckDefaultAddrMatchesServer(t *testing.T) {
	if got, want := defaultHealthcheckAddr, "127.0.0.1:8787"; got != want {
		t.Errorf("default addr = %q, want %q (the loop-serve-net --addr default)", got, want)
	}
}

// TestHealthcheckPrecedesConfigLoad is the regression that matters: a container
// healthcheck must answer on a server whose ~/.fuse/config.yml is broken —
// exactly when you most want to learn the container is unhealthy. Mirrors
// TestVersionPrecedesConfigLoad.
func TestHealthcheckPrecedesConfigLoad(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	broken := []byte("models: [unterminated\n\t\tbad: :\n")
	if err := os.WriteFile(filepath.Join(home, ".fuse", "config.yml"), broken, 0o600); err != nil {
		t.Fatal(err)
	}

	// Sanity: the fixture really is broken for a subcommand below config.Load().
	var mOut, mErr bytes.Buffer
	if code := run([]string{"models"}, &mOut, &mErr); code == 0 {
		t.Fatalf("fixture is not broken: `models` succeeded with a malformed config")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	if code := run([]string{"healthcheck", "--addr", addrOf(t, srv)}, &out, &errb); code != 0 {
		t.Fatalf("healthcheck with a broken config: exit = %d, stderr=%s", code, errb.String())
	}

	// And a 503 still reports unhealthy rather than a config error.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	var out2, errb2 bytes.Buffer
	if code := run([]string{"healthcheck", "--addr", addrOf(t, bad)}, &out2, &errb2); code != 1 {
		t.Fatalf("503 with a broken config: exit = %d, want 1", code)
	}
	if strings.Contains(errb2.String(), "config error") {
		t.Errorf("healthcheck reported a config error instead of the probe result: %q", errb2.String())
	}
}
