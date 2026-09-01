package sandbox

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// shortDir returns a SHORT temporary directory.
//
// It exists instead of t.TempDir() for one concrete reason: a UNIX socket path
// is capped by the kernel (104 bytes of sun_path on darwin, 108 on Linux), and
// t.TempDir() derives its path from the TEST NAME, so a descriptive test name on
// a macOS TMPDIR ("/var/folders/../T/") pushes the socket beyond the cap and the
// listener fails with a bind error that has nothing to do with what is being
// tested. The proxy's own root is the caller's to choose for exactly this
// reason; tests choose a short one.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "fx")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// recordingHooks collects refusals so a test can assert what the OPERATOR is
// told, which is deliberately more than what the client is told.
type recordingHooks struct {
	mu       sync.Mutex
	refusals []RefusalInfo
}

func (r *recordingHooks) hooks() ProxyHooks {
	return ProxyHooks{Refused: func(info RefusalInfo) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.refusals = append(r.refusals, info)
	}}
}

func (r *recordingHooks) snapshot() []RefusalInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]RefusalInfo(nil), r.refusals...)
}

// newTestProxy builds a Proxy rooted in a short directory, closed at test end.
func newTestProxy(t *testing.T, opts ...proxyOption) *Proxy {
	t.Helper()
	opts = append([]proxyOption{withProxyRoot(shortDir(t))}, opts...)
	p, err := NewProxy(opts...)
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// allowHostPort builds an enforce policy declaring exactly the given
// "host:port" destinations, THROUGH THE REAL LOADER, so the proxy is matching
// against the same AllowEntry values an operator's file would produce
// (including the load-time canonicalization and the bare-IP-as-full-mask-CIDR
// rule).
func allowHostPort(t *testing.T, hostports ...string) Egress {
	t.Helper()
	entries := make([]string, 0, len(hostports))
	for _, hp := range hostports {
		host, port, err := net.SplitHostPort(hp)
		if err != nil {
			t.Fatalf("SplitHostPort(%q): %v", hp, err)
		}
		entries = append(entries, fmt.Sprintf("    - host: %s\n      port: %s\n", host, port))
	}
	return loadEgress(t, entries...)
}

// connectVia opens a client connection on sock and issues one CONNECT for
// target. Any extra headers are sent verbatim — tests use them to prove that
// nothing a client SAYS can influence which policy it is served under.
func connectVia(t *testing.T, sock, target string, extraHeaders ...string) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial %s: %v", sock, err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	for _, h := range extraHeaders {
		req += h + "\r\n"
	}
	req += "\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}

	br := bufio.NewReader(conn)
	// The request is handed to ReadResponse so a 2xx tunnel response is not
	// mistaken for a message with a body (net/http's own CONNECT convention).
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	return conn, br, resp
}

// getThroughTunnel performs one plain HTTP GET over an established tunnel and
// returns the body.
func getThroughTunnel(t *testing.T, conn net.Conn, br *bufio.Reader, hostport string) string {
	t.Helper()
	if _, err := fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", hostport); err != nil {
		t.Fatalf("write GET: %v", err)
	}
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read GET response: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

// upstream starts an HTTP server on loopback that answers every request with
// marker, and returns its "host:port".
func upstream(t *testing.T, marker string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, marker)
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// Only CONNECT is spoken. Everything else is refused with 405 — the proxy does
// not fall back to forwarding an absolute-URI request, because a forward proxy
// path would be a second, differently-shaped way to reach the network.
func TestProxyRefusesNonConnectMethods(t *testing.T) {
	rec := &recordingHooks{}
	p := newTestProxy(t, withProxyHooks(rec.hooks()))
	sock, err := p.Listen(principal("acme", "alice"), allowHostPort(t, "127.0.0.1:9"))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	for _, line := range []string{
		"GET http://127.0.0.1:9/ HTTP/1.1\r\nHost: 127.0.0.1:9\r\n\r\n",
		"POST http://127.0.0.1:9/ HTTP/1.1\r\nHost: 127.0.0.1:9\r\nContent-Length: 0\r\n\r\n",
		"OPTIONS * HTTP/1.1\r\nHost: 127.0.0.1:9\r\n\r\n",
	} {
		conn, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		if _, err := io.WriteString(conn, line); err != nil {
			t.Fatalf("write: %v", err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want %d for %q", resp.StatusCode, http.StatusMethodNotAllowed, line)
		}
		_ = resp.Body.Close()
		_ = conn.Close()
	}

	for _, info := range rec.snapshot() {
		if info.Reason != RefusedNonConnect {
			t.Errorf("refusal reason = %q, want %q", info.Reason, RefusedNonConnect)
		}
	}
}

// An undeclared destination is refused with 403, the destination is recorded
// for the OPERATOR, and the client is told nothing beyond a generic denial.
func TestProxyRefusesUndeclaredDestination(t *testing.T) {
	rec := &recordingHooks{}
	p := newTestProxy(t, withProxyHooks(rec.hooks()))
	sock, err := p.Listen(principal("acme", "alice"), allowHostPort(t, "pkg.example.com:443"))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	_, _, resp := connectVia(t, sock, "evil.example.net:443")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()

	// The denial must not hand the destination back: the client is the model's
	// command, and the refusal is not a probe oracle it gets to read.
	if strings.Contains(string(body), "evil.example.net") {
		t.Errorf("denial body echoed the destination: %q", body)
	}
	for _, v := range resp.Header {
		for _, hv := range v {
			if strings.Contains(hv, "evil.example.net") {
				t.Errorf("denial header echoed the destination: %q", hv)
			}
		}
	}

	// The operator, by contrast, gets the whole destination.
	refusals := rec.snapshot()
	if len(refusals) != 1 {
		t.Fatalf("refusals = %+v, want exactly 1", refusals)
	}
	got := refusals[0]
	if got.Host != "evil.example.net" || got.Port != 443 {
		t.Errorf("recorded destination = %s:%d, want evil.example.net:443", got.Host, got.Port)
	}
	if got.Reason != RefusedNotDeclared {
		t.Errorf("reason = %q, want %q", got.Reason, RefusedNotDeclared)
	}
	if got.Principal.Subject != "alice" {
		t.Errorf("recorded principal = %+v, want subject alice", got.Principal)
	}
}

// The deny-all state. A listener with nothing declared refuses everything,
// including a destination that is trivially reachable from the host.
func TestProxyEmptyAllowlistRefusesEverything(t *testing.T) {
	reachable := upstream(t, "should never be reached")
	p := newTestProxy(t)
	sock, err := p.Listen(principal("acme", "alice"), Egress{Mode: EgressEnforce})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	for _, target := range []string{reachable, "pkg.example.com:443", "127.0.0.1:443", "[2001:db8::1]:443"} {
		_, _, resp := connectVia(t, sock, target)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status for %s = %d, want %d", target, resp.StatusCode, http.StatusForbidden)
		}
		_ = resp.Body.Close()
	}
}

// A declared destination is tunnelled end to end: CONNECT, 200, then real bytes
// both ways.
func TestProxyDeclaredDestinationReachesUpstream(t *testing.T) {
	addr := upstream(t, "hello from upstream")
	p := newTestProxy(t)
	sock, err := p.Listen(principal("acme", "alice"), allowHostPort(t, addr))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	conn, br, resp := connectVia(t, sock, addr)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := getThroughTunnel(t, conn, br, addr); got != "hello from upstream" {
		t.Errorf("body = %q, want %q", got, "hello from upstream")
	}
}

// An entry that declares a #52 credential audience is REFUSED while no
// credential source is wired (task 5 supplies one). A declared-identity entry
// that quietly downgrades to an unauthenticated allow-through is the fail-open
// shape this change exists to prevent.
func TestProxyRefusesCredentialEntryWithoutSource(t *testing.T) {
	addr := upstream(t, "unreachable without identity")
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	policy := loadEgress(t, fmt.Sprintf("    - host: %s\n      port: %s\n      credential: internal-api\n", host, portStr))

	rec := &recordingHooks{}
	p := newTestProxy(t, withProxyHooks(rec.hooks()))
	sock, err := p.Listen(principal("acme", "alice"), policy)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	_, _, resp := connectVia(t, sock, addr)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
	_ = resp.Body.Close()

	refusals := rec.snapshot()
	if len(refusals) != 1 || refusals[0].Reason != RefusedCredentialUnavailable {
		t.Fatalf("refusals = %+v, want one %q", refusals, RefusedCredentialUnavailable)
	}
}

// THE LOAD-BEARING TEST (learning: shared-server-broadcast-needs-per-session-
// routing). Two principals are driven CONCURRENTLY through ONE Proxy with
// DIFFERENT allowlists, and each must see only its own policy. A sequential
// switch between the two would pass against a proxy that holds a single
// "current" policy, which is exactly the defect class this guards.
//
// The clients also send headers naming the other principal, because the
// invariant is that a connection's principal comes from the LISTENER it arrived
// on and from nothing the client says.
func TestProxyConcurrentPrincipalsSeeOnlyTheirOwnPolicy(t *testing.T) {
	addrA := upstream(t, "A")
	addrB := upstream(t, "B")

	rec := &recordingHooks{}
	p := newTestProxy(t, withProxyHooks(rec.hooks()))

	sockA, err := p.Listen(principal("acme", "alice"), allowHostPort(t, addrA))
	if err != nil {
		t.Fatalf("Listen(alice): %v", err)
	}
	sockB, err := p.Listen(principal("acme", "bob"), allowHostPort(t, addrB))
	if err != nil {
		t.Fatalf("Listen(bob): %v", err)
	}
	if sockA == sockB {
		t.Fatalf("both principals got the same socket %q", sockA)
	}

	const rounds = 25
	var wg sync.WaitGroup
	drive := func(sock, mine, theirs, marker, otherSubject string) {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			// Own declared destination: tunnelled.
			conn, br, resp := connectVia(t, sock, mine, "X-Fuse-Principal: "+otherSubject)
			if resp.StatusCode != http.StatusOK {
				t.Errorf("%s: own destination %s got %d, want 200", marker, mine, resp.StatusCode)
			} else if got := getThroughTunnel(t, conn, br, mine); got != marker {
				t.Errorf("%s: tunnel body = %q, want %q", marker, got, marker)
			}
			_ = conn.Close()

			// The OTHER principal's declared destination: refused here, even
			// though it is declared — by the other principal, on another
			// listener, which this connection has no access to.
			conn2, _, resp2 := connectVia(t, sock, theirs, "X-Fuse-Principal: "+otherSubject)
			if resp2.StatusCode != http.StatusForbidden {
				t.Errorf("%s: other principal's destination %s got %d, want 403", marker, theirs, resp2.StatusCode)
			}
			_ = resp2.Body.Close()
			_ = conn2.Close()
		}
	}
	wg.Add(2)
	go drive(sockA, addrA, addrB, "A", "bob")
	go drive(sockB, addrB, addrA, "B", "alice")
	wg.Wait()

	// Every refusal must be attributed to the principal whose LISTENER it
	// arrived on — never to the one the client named in a header.
	byPrincipal := map[string]int{}
	for _, info := range rec.snapshot() {
		if info.Reason != RefusedNotDeclared {
			t.Errorf("unexpected refusal %+v", info)
		}
		byPrincipal[info.Principal.Subject]++
	}
	if byPrincipal["alice"] != rounds || byPrincipal["bob"] != rounds {
		t.Errorf("refusals by principal = %v, want %d each for alice and bob", byPrincipal, rounds)
	}
}

// The socket path IS the identity: one per principal, under a 0700 fuse-owned
// root, with an unguessable component that is not derived from the principal.
func TestProxySocketsArePerPrincipalAndPrivate(t *testing.T) {
	p := newTestProxy(t)

	sockA, err := p.Listen(principal("acme", "alice"), Egress{Mode: EgressEnforce})
	if err != nil {
		t.Fatalf("Listen(alice): %v", err)
	}
	sockB, err := p.Listen(principal("acme", "bob"), Egress{Mode: EgressEnforce})
	if err != nil {
		t.Fatalf("Listen(bob): %v", err)
	}
	if sockA == sockB {
		t.Fatal("two principals share one socket path")
	}

	// A second Listen for the same principal is the same listener, not a
	// second one: several concurrent sandboxes for one principal share it.
	again, err := p.Listen(principal("acme", "alice"), Egress{Mode: EgressEnforce})
	if err != nil {
		t.Fatalf("re-Listen(alice): %v", err)
	}
	if again != sockA {
		t.Errorf("re-Listen gave %q, want the existing %q", again, sockA)
	}

	for _, sock := range []string{sockA, sockB} {
		// Nothing about the principal is spelled into the path: a container
		// that can read one path must not be able to compute another.
		if strings.Contains(sock, "alice") || strings.Contains(sock, "bob") || strings.Contains(sock, "acme") {
			t.Errorf("socket path %q leaks the principal", sock)
		}
		dir := filepath.Dir(sock)
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf("dir %s mode = %o, want 700", dir, perm)
		}
	}

	rootInfo, err := os.Stat(p.Root())
	if err != nil {
		t.Fatalf("stat root: %v", err)
	}
	if perm := rootInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("root %s mode = %o, want 700", p.Root(), perm)
	}
}

// Release closes exactly one principal's listener; Close tears down every
// remaining one and removes the sockets from the filesystem.
func TestProxyReleaseAndCloseTearDownListeners(t *testing.T) {
	p := newTestProxy(t)
	sockA, err := p.Listen(principal("acme", "alice"), Egress{Mode: EgressEnforce})
	if err != nil {
		t.Fatalf("Listen(alice): %v", err)
	}
	sockB, err := p.Listen(principal("acme", "bob"), Egress{Mode: EgressEnforce})
	if err != nil {
		t.Fatalf("Listen(bob): %v", err)
	}

	if err := p.Release(principal("acme", "alice")); err != nil {
		t.Fatalf("Release(alice): %v", err)
	}
	if _, err := os.Stat(sockA); !os.IsNotExist(err) {
		t.Errorf("stat(%s) after Release = %v, want not-exist", sockA, err)
	}
	if c, err := net.Dial("unix", sockA); err == nil {
		_ = c.Close()
		t.Errorf("dial %s succeeded after Release", sockA)
	}
	// bob is untouched.
	if c, err := net.Dial("unix", sockB); err != nil {
		t.Errorf("dial %s after releasing alice: %v", sockB, err)
	} else {
		_ = c.Close()
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(sockB); !os.IsNotExist(err) {
		t.Errorf("stat(%s) after Close = %v, want not-exist", sockB, err)
	}
	if _, err := os.Stat(p.Root()); !os.IsNotExist(err) {
		t.Errorf("stat(root) after Close = %v, want not-exist", err)
	}
	if _, err := p.Listen(principal("acme", "carol"), Egress{Mode: EgressEnforce}); err == nil {
		t.Error("Listen succeeded after Close")
	}
	// Close is idempotent: teardown paths run it more than once.
	if err := p.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// A malformed CONNECT target is refused rather than guessed at. In particular a
// target with no port never resolves to a default port: the port is exact and
// required, so a missing one cannot match anything.
func TestProxyRefusesMalformedTarget(t *testing.T) {
	rec := &recordingHooks{}
	p := newTestProxy(t, withProxyHooks(rec.hooks()))
	sock, err := p.Listen(principal("acme", "alice"), allowHostPort(t, "pkg.example.com:443"))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	for _, target := range []string{"pkg.example.com", "pkg.example.com:https", "pkg.example.com:0", ":443"} {
		_, _, resp := connectVia(t, sock, target)
		if resp.StatusCode == http.StatusOK {
			t.Errorf("target %q was tunnelled, want refusal", target)
		}
		_ = resp.Body.Close()
	}
	for _, info := range rec.snapshot() {
		if info.Reason != RefusedMalformedTarget && info.Reason != RefusedNotDeclared {
			t.Errorf("refusal %+v: unexpected reason", info)
		}
	}
}

// The host is canonicalized exactly ONCE, at the proxy's entry, so alternate
// spellings of a declared destination reach the same decision (ADR-0048 rule 3).
func TestProxyCanonicalizesTargetAtEntry(t *testing.T) {
	addr := upstream(t, "canonical")
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("Atoi: %v", err)
	}

	// "localhost" is declared by NAME (not as a literal), and it resolves to
	// the loopback address the upstream is listening on — so each alternate
	// spelling can be asserted end to end rather than only at the decision.
	policy := loadEgress(t, fmt.Sprintf("    - host: localhost\n      port: %d\n", port))
	rec := &recordingHooks{}
	p := newTestProxy(t, withProxyHooks(rec.hooks()))
	sock, err := p.Listen(principal("acme", "alice"), policy)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	for _, spelling := range []string{"localhost", "LocalHost", "localhost."} {
		target := fmt.Sprintf("%s:%d", spelling, port)
		conn, br, resp := connectVia(t, sock, target)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("spelling %q got %d, want 200", spelling, resp.StatusCode)
			_ = resp.Body.Close()
			continue
		}
		if got := getThroughTunnel(t, conn, br, target); got != "canonical" {
			t.Errorf("spelling %q: body = %q, want %q", spelling, got, "canonical")
		}
		_ = conn.Close()
	}
	for _, info := range rec.snapshot() {
		t.Errorf("declared host refused: %+v", info)
	}
}
