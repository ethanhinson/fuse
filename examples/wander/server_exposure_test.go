//go:build browser

// Change 0060 review fix — the DEMO SERVER'S EXPOSURE lane.
//
// examples/wander/server.js does two things that are only safe on loopback: it reverse-
// proxies `/fuse.loop.v1.*` to the operator's `fuse loop-serve-net` forwarding Authorization
// verbatim, and it PUBLISHES the bearer-token directory of a demo config at
// `GET /demo-users.json`. Before this lane, `server.listen(PORT)` bound 0.0.0.0/:: and the
// FUSE_DEMO_CONFIG override was guarded only by a console.warn — so any peer on the network
// could fetch a valid token and then drive the local backend through the same proxy.
//
// This lane pins BOTH halves of the fix:
//
//  1. the static server binds loopback ONLY — it is not reachable on a non-loopback address
//     of this host;
//  2. the token endpoint FAILS CLOSED — pointing FUSE_DEMO_CONFIG at any file other than the
//     checked-in examples/wander/fuse.demo.yml refuses to publish unless the operator also
//     sets FUSE_DEMO_PUBLISH_TOKENS=1.
//
// It rides the `browser` tag (not the default lane) because it drives the REAL server.js as
// a subprocess and node is only a guaranteed part of this lane's toolchain — the same lane
// CI runs as `go test -tags browser ./examples/wander/...`. Nothing here needs a browser, so
// it is cheap; it is LOUD on toolchain absence like its neighbours, never a green skip.
package wander_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// foreignConfigYAML is a stand-in for "an operator pointed FUSE_DEMO_CONFIG at a real
// config" — e.g. ~/.fuse/config.yml. Its token is what must NOT ship without the opt-in.
const foreignConfigToken = "not-a-demo-token-please-do-not-publish-me"

const foreignConfigYAML = `loop_server:
  auth:
    - token: ` + foreignConfigToken + `
      tenant: prod
      subject: operator
`

// TestWanderStaticServerBindsLoopbackOnly proves half 1: server.js must not accept on a
// non-loopback address of this host. The proxy behind it forwards Authorization verbatim to
// the operator's local backend, so a wildcard bind is a remote-drive surface.
func TestWanderStaticServerBindsLoopbackOnly(t *testing.T) {
	requireNode(t)
	repoRoot := repoRootFromTest(t)
	port := freePort(t)
	startWanderServerJS(t, repoRoot, port)

	// Loopback still works — the fix must not break the demo or the browser lanes.
	if code, _ := getExposure(t, net.JoinHostPort("127.0.0.1", port), "/demo-users.json", 5*time.Second); code != 200 {
		t.Fatalf("loopback GET /demo-users.json = %d, want 200 (the default in-repo demo config must keep working)", code)
	}

	nonLoopback := nonLoopbackIPv4(t)
	if nonLoopback == "" {
		// No routable IPv4 on this host at all: the exposure this test asserts about cannot
		// be constructed here, and a silent pass would be a fiction.
		t.Fatal("no non-loopback IPv4 interface on this host: cannot prove server.js refuses a remote peer")
	}
	addr := net.JoinHostPort(nonLoopback, port)
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err == nil {
		_ = conn.Close()
		code, body := getExposure(t, addr, "/demo-users.json", 5*time.Second)
		t.Fatalf("server.js accepted a connection on non-loopback %s (GET /demo-users.json -> %d %q); it must bind 127.0.0.1 only",
			addr, code, truncate(body, 200))
	}
}

// TestDemoUsersFailsClosedOnConfigOverride proves half 2: the token-publishing endpoint
// refuses a FUSE_DEMO_CONFIG that is not the checked-in demo file unless the operator sets
// the second, explicit opt-in — and that the opt-in still works, because the identity lane
// and `run.sh`-style demos legitimately supply their own demo config.
func TestDemoUsersFailsClosedOnConfigOverride(t *testing.T) {
	requireNode(t)
	repoRoot := repoRootFromTest(t)

	foreign := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(foreign, []byte(foreignConfigYAML), 0o600); err != nil {
		t.Fatalf("write foreign config: %v", err)
	}

	t.Run("refuses without opt-in", func(t *testing.T) {
		port := freePort(t)
		startWanderServerJS(t, repoRoot, port, "FUSE_DEMO_CONFIG="+foreign)
		code, body := getExposure(t, net.JoinHostPort("127.0.0.1", port), "/demo-users.json", 5*time.Second)
		if code == 200 {
			t.Errorf("GET /demo-users.json = 200 for an overridden config with no opt-in; want a refusal")
		}
		if strings.Contains(body, foreignConfigToken) {
			t.Errorf("GET /demo-users.json published the overridden config's bearer token (%d): %q", code, truncate(body, 300))
		}
	})

	t.Run("publishes with the explicit opt-in", func(t *testing.T) {
		port := freePort(t)
		startWanderServerJS(t, repoRoot, port, "FUSE_DEMO_CONFIG="+foreign, "FUSE_DEMO_PUBLISH_TOKENS=1")
		code, body := getExposure(t, net.JoinHostPort("127.0.0.1", port), "/demo-users.json", 5*time.Second)
		if code != 200 || !strings.Contains(body, foreignConfigToken) {
			t.Fatalf("with FUSE_DEMO_PUBLISH_TOKENS=1 the override must publish; got %d %q", code, truncate(body, 300))
		}
	})

	t.Run("default in-repo demo config needs no opt-in", func(t *testing.T) {
		port := freePort(t)
		startWanderServerJS(t, repoRoot, port)
		code, body := getExposure(t, net.JoinHostPort("127.0.0.1", port), "/demo-users.json", 5*time.Second)
		if code != 200 {
			t.Fatalf("default GET /demo-users.json = %d, want 200 (the identity lane reads this)", code)
		}
		if !strings.Contains(body, "token") {
			t.Fatalf("default GET /demo-users.json published no directory: %q", truncate(body, 300))
		}
	})
}

// --- helpers (local to this file; the browser lanes' serveWander* take no extra env) ----

func requireNode(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatalf("node not on PATH: this lane drives the real examples/wander/server.js (%v)", err)
	}
}

// startWanderServerJS starts `node server.js` on port with extraEnv appended, waits for it
// to accept on loopback, and tears it down via t.Cleanup.
func startWanderServerJS(t *testing.T, repoRoot, port string, extraEnv ...string) {
	t.Helper()
	serverJS := filepath.Join(repoRoot, "examples", "wander", "server.js")
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "node", serverJS)
	cmd.Dir = filepath.Join(repoRoot, "examples", "wander")
	cmd.Env = append(append(cmd.Environ(), "PORT="+port), extraEnv...)
	var out syncBuffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start wander server.js: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Process.Kill()
		waitBounded(cmd, 10*time.Second)
	})
	waitForListen(t, net.JoinHostPort("127.0.0.1", port), 15*time.Second, &out)
}

// getExposure performs one GET against addr and returns the status code and body. A
// transport error yields code 0 — the caller decides whether that is the desired outcome.
func getExposure(t *testing.T, addr, path string, timeout time.Duration) (int, string) {
	t.Helper()
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(fmt.Sprintf("http://%s%s", addr, path))
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// nonLoopbackIPv4 returns an up, non-loopback IPv4 address of this host, or "" if none.
func nonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("enumerate interfaces: %v", err)
	}
	for _, ifa := range ifaces {
		if ifa.Flags&net.FlagUp == 0 || ifa.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifa.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
				continue
			}
			return ip4.String()
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
