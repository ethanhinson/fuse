package main

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// shortTempRoot points os.TempDir at a SHORT directory for the duration of the
// test. The proxy's per-principal socket path is <tempdir>/fuse-egressXXXX/<16
// hex>/egress.sock and the kernel caps sun_path at 104 bytes; darwin's default
// TMPDIR (/var/folders/...) is long enough to make that a coin flip. Nothing
// about the behaviour under test depends on the location.
func shortTempRoot(t *testing.T) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "fz")
	if err != nil {
		t.Fatalf("mkdir short temp root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("TMPDIR", dir)
}

// upstreamRecorder is a real HTTP origin server that records the Authorization
// header of the last request it served. It stands in for the operator's
// declared `credential:` destination.
type upstreamRecorder struct {
	mu   sync.Mutex
	auth string
	srv  *httptest.Server
}

func newUpstreamRecorder(t *testing.T) *upstreamRecorder {
	t.Helper()
	u := &upstreamRecorder{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.auth = r.Header.Get("Authorization")
		u.mu.Unlock()
		w.Header().Set("Content-Length", "2")
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstreamRecorder) authorization() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.auth
}

// hostPort splits the httptest server's URL into the host and port an
// egress.allow entry declares.
func (u *upstreamRecorder) hostPort(t *testing.T) (string, string) {
	t.Helper()
	parsed, err := url.Parse(u.srv.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	host, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatalf("split upstream host: %v", err)
	}
	return host, port
}

// forwardThroughProxy sends one absolute-form request (the shape a client emits
// for an http:// destination when HTTP_PROXY points at the proxy) over the
// principal's unix socket and returns the response.
func forwardThroughProxy(t *testing.T, sock, target string) *http.Response {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial egress socket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	parsed, err := url.Parse(target)
	if err != nil {
		t.Fatalf("parse target: %v", err)
	}
	if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", target, parsed.Host); err != nil {
		t.Fatalf("write request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// credentialEgressConfig is an enforce-mode off-switch file declaring ONE
// destination under a #52 delegated identity.
func credentialEgressConfig(host, port string) string {
	return "egress:\n  mode: enforce\n  allow:\n    - host: " + host + "\n      port: " + port + "\n      credential: internal-api\n"
}

// listenForTest registers the local principal on a proxy under the policy the
// off-switch file resolved to, and returns the socket path.
func listenForTest(t *testing.T, proxy *sandbox.Proxy, root string, cfg config.Config) string {
	t.Helper()
	loaded, _ := sandbox.LoadConfig(root)
	sock, err := proxy.Listen(localPrincipal(cfg), loaded.Egress)
	if err != nil {
		t.Fatalf("proxy.Listen: %v", err)
	}
	return sock
}

// TestEgressCredentialEntryResolvesAtCompositionRoot is the finding this file
// exists for: `withCredentialSource` had no production caller, so in the shipped
// binary an operator who declared `credential: internal-api` on an egress entry
// got a hard refusal with no way to satisfy it. The seam must resolve through
// the SAME toolidentity construction cmd/fuse already uses for MCP.
func TestEgressCredentialEntryResolvesAtCompositionRoot(t *testing.T) {
	shortTempRoot(t)
	upstream := newUpstreamRecorder(t)
	host, port := upstream.hostPort(t)

	root := t.TempDir()
	writeEgressConfig(t, root, credentialEgressConfig(host, port))
	withForwarderCandidates(t, []string{fakeForwarder(t, t.TempDir())})

	cfg := config.Config{}
	cfg.ToolIdentity.SigningKey = "composition-root-test-key"

	var buf syncBuffer
	proxy, _, stop := resolveEgressDatapath(cfg, root, &buf)
	if proxy == nil {
		t.Fatalf("no datapath; diagnostics: %s", buf.String())
	}
	t.Cleanup(func() { _ = proxy.Close(); stop() })

	sock := listenForTest(t, proxy, root, cfg)
	resp := forwardThroughProxy(t, sock, "http://"+net.JoinHostPort(host, port)+"/thing")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a `credential:` entry is still unreachable in the shipped binary; diagnostics: %s", resp.StatusCode, buf.String())
	}
	auth := upstream.authorization()
	if !strings.HasPrefix(auth, "Bearer ") || len(auth) <= len("Bearer ") {
		t.Fatalf("upstream saw Authorization %q, want a minted delegated bearer token", auth)
	}
}

// TestEgressCredentialWithoutSigningKeyStaysRefused pins the fail-closed half:
// wiring the seam must not create a configuration in which a declared identity
// degrades to an unauthenticated allow-through. With no signing key there is no
// source, so the entry is REFUSED — and the operator is told why, both at
// startup and at the refusal.
func TestEgressCredentialWithoutSigningKeyStaysRefused(t *testing.T) {
	shortTempRoot(t)
	upstream := newUpstreamRecorder(t)
	host, port := upstream.hostPort(t)

	root := t.TempDir()
	writeEgressConfig(t, root, credentialEgressConfig(host, port))
	withForwarderCandidates(t, []string{fakeForwarder(t, t.TempDir())})

	var buf syncBuffer
	cfg := config.Config{} // no tool_identity.signing_key
	proxy, _, stop := resolveEgressDatapath(cfg, root, &buf)
	if proxy == nil {
		t.Fatalf("no datapath; diagnostics: %s", buf.String())
	}
	t.Cleanup(func() { _ = proxy.Close(); stop() })

	sock := listenForTest(t, proxy, root, cfg)
	resp := forwardThroughProxy(t, sock, "http://"+net.JoinHostPort(host, port)+"/thing")

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 — a declared identity must never be served without one", resp.StatusCode)
	}
	if auth := upstream.authorization(); auth != "" {
		t.Errorf("upstream was reached (Authorization %q); the request must never have left", auth)
	}
	if out := buf.String(); !strings.Contains(out, "signing_key") {
		t.Errorf("startup diagnostics %q do not tell the operator the credential seam cannot mint", out)
	}
}

// TestEgressRefusalIsReportedToTheOperator: ProxyHooks.Refused is the only egress
// observability this package offers, and nothing consumed it — so an operator
// could not see WHY egress was denied. The composition root must report each
// refusal on the same channel it prints the UNCONTAINED / EGRESS ENFORCED
// notices on.
func TestEgressRefusalIsReportedToTheOperator(t *testing.T) {
	shortTempRoot(t)
	root := t.TempDir()
	writeEgressConfig(t, root, enforceConfig) // declares api.example.com:443 only
	withForwarderCandidates(t, []string{fakeForwarder(t, t.TempDir())})

	var buf syncBuffer
	cfg := config.Config{}
	proxy, _, stop := resolveEgressDatapath(cfg, root, &buf)
	if proxy == nil {
		t.Fatalf("no datapath; diagnostics: %s", buf.String())
	}
	sock := listenForTest(t, proxy, root, cfg)

	resp := forwardThroughProxy(t, sock, "http://not-declared.example.com/x")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	// Close the proxy (which waits for the connection goroutines) and then the
	// reporter, so the asynchronous emission is guaranteed to have landed.
	_ = proxy.Close()
	stop()

	out := buf.String()
	for _, want := range []string{"egress", "not-declared.example.com", string(sandbox.RefusedNotDeclared)} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal diagnostics %q do not contain %q", out, want)
		}
	}
}

// syncBuffer is a bytes.Buffer safe for the reporter's drain goroutine to write
// to while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
