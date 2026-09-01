package sandbox

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/loopauth"
	"github.com/ethanhinson/fuse/internal/toolidentity"
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
// returns the body. Any extra headers are sent verbatim INSIDE the tunnel —
// tests use them to prove that a header the client sets cannot survive to the
// upstream when the proxy is supplying a delegated identity.
func getThroughTunnel(t *testing.T, conn net.Conn, br *bufio.Reader, hostport string, extraHeaders ...string) string {
	t.Helper()
	req := fmt.Sprintf("GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n", hostport)
	for _, h := range extraHeaders {
		req += h + "\r\n"
	}
	req += "\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
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

// proxyRequest issues one ABSOLUTE-FORM request through the proxy socket and
// returns the response and its body.
//
// This is the shape curl/git/pip ACTUALLY send for an `http://` destination when
// HTTP_PROXY names the proxy: `GET http://host/path HTTP/1.1`, target and all.
// It is the only client-produced shape that can carry a delegated identity, so
// every #52 assertion is driven through it rather than through a hand-crafted
// tunnel no real client opens.
func proxyRequest(t *testing.T, sock, method, rawURL string, extraHeaders ...string) (*http.Response, string) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial %s: %v", sock, err)
	}
	defer func() { _ = conn.Close() }()

	req := method + " " + rawURL + " HTTP/1.1\r\nHost: " + u.Host + "\r\n"
	for _, h := range extraHeaders {
		req += h + "\r\n"
	}
	req += "\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatalf("write request: %v", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: method})
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, string(body)
}

// proxyGet is proxyRequest for the common GET case.
func proxyGet(t *testing.T, sock, rawURL string, extraHeaders ...string) (*http.Response, string) {
	t.Helper()
	return proxyRequest(t, sock, http.MethodGet, rawURL, extraHeaders...)
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

// A request that names NO destination the allowlist could be consulted about is
// refused with 405. Origin-form ("GET /"), asterisk-form ("OPTIONS *") and a
// non-`http` absolute URI all fall here: the proxy serves CONNECT and
// absolute-form `http://` requests, and a shape outside those two cannot be
// authorized, so it is not served.
func TestProxyRefusesUnservableRequestShapes(t *testing.T) {
	rec := &recordingHooks{}
	p := newTestProxy(t, withProxyHooks(rec.hooks()))
	sock, err := p.Listen(principal("acme", "alice"), allowHostPort(t, "127.0.0.1:9"))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	for _, line := range []string{
		"GET / HTTP/1.1\r\nHost: 127.0.0.1:9\r\n\r\n",
		"POST / HTTP/1.1\r\nHost: 127.0.0.1:9\r\nContent-Length: 0\r\n\r\n",
		"OPTIONS * HTTP/1.1\r\nHost: 127.0.0.1:9\r\n\r\n",
		"GET ftp://127.0.0.1:9/ HTTP/1.1\r\nHost: 127.0.0.1:9\r\n\r\n",
		"GET https://127.0.0.1:9/ HTTP/1.1\r\nHost: 127.0.0.1:9\r\n\r\n",
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

// An absolute-form URL with no host names no destination either, and is refused
// as malformed rather than dialled at whatever the empty host resolves to.
func TestProxyRefusesAbsoluteFormWithoutHost(t *testing.T) {
	rec := &recordingHooks{}
	p := newTestProxy(t, withProxyHooks(rec.hooks()))
	sock, err := p.Listen(principal("acme", "alice"), allowHostPort(t, "127.0.0.1:9"))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, "GET http://:9/ HTTP/1.1\r\nHost: 127.0.0.1:9\r\n\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}

	refusals := rec.snapshot()
	if len(refusals) != 1 || refusals[0].Reason != RefusedMalformedTarget {
		t.Fatalf("refusals = %+v, want one %q", refusals, RefusedMalformedTarget)
	}
}

// An ABSOLUTE-FORM request to a declared destination is served as a real forward
// proxy: matched against the same allowlist, and relayed to the upstream. This is
// the shape an injected HTTP_PROXY produces for an `http://` URL, so without it
// the `http://` half of the allowlist is unreachable in practice.
func TestProxyForwardsAbsoluteFormToDeclaredDestination(t *testing.T) {
	addr := upstream(t, "hello from upstream")
	rec := &recordingHooks{}
	p := newTestProxy(t, withProxyHooks(rec.hooks()))
	sock, err := p.Listen(principal("acme", "alice"), allowHostPort(t, addr))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	resp, body := proxyGet(t, sock, "http://"+addr+"/some/path")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if body != "hello from upstream" {
		t.Errorf("body = %q, want %q", body, "hello from upstream")
	}
	for _, info := range rec.snapshot() {
		t.Errorf("declared destination refused: %+v", info)
	}
}

// The forward-proxy path is NOT a second, laxer gate. An absolute-form request to
// an UNDECLARED destination is refused exactly as a CONNECT to one is, the
// destination reaches the operator, and the client is told nothing.
func TestProxyRefusesAbsoluteFormUndeclaredDestination(t *testing.T) {
	rec := &recordingHooks{}
	p := newTestProxy(t, withProxyHooks(rec.hooks()))
	sock, err := p.Listen(principal("acme", "alice"), allowHostPort(t, "pkg.example.com:80"))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	resp, body := proxyGet(t, sock, "http://evil.example.net/payload")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
	if strings.Contains(body, "evil.example.net") {
		t.Errorf("denial body echoed the destination: %q", body)
	}

	refusals := rec.snapshot()
	if len(refusals) != 1 {
		t.Fatalf("refusals = %+v, want exactly 1", refusals)
	}
	got := refusals[0]
	// The scheme's default port is the port that is matched — an absolute-form
	// `http://` URL with no port means 80 by the URL grammar, not "any port".
	if got.Host != "evil.example.net" || got.Port != 80 {
		t.Errorf("recorded destination = %s:%d, want evil.example.net:80", got.Host, got.Port)
	}
	if got.Reason != RefusedNotDeclared {
		t.Errorf("reason = %q, want %q", got.Reason, RefusedNotDeclared)
	}
	if got.Principal.Subject != "alice" {
		t.Errorf("recorded principal = %+v, want subject alice", got.Principal)
	}
}

// A declared host on a NON-default port is not reachable through the scheme's
// default port, and vice versa. The forward path must not widen the exact-port
// rule the CONNECT path enforces.
func TestProxyForwardMatchesExactPort(t *testing.T) {
	addr := upstream(t, "declared")
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	p := newTestProxy(t)
	// Declared on the upstream's real port only.
	sock, err := p.Listen(principal("acme", "alice"), allowHostPort(t, addr))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	// No port in the URL means 80, which is not the declared port.
	resp, _ := proxyGet(t, sock, "http://127.0.0.1/")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("portless URL got %d, want %d (port %s is what was declared)", resp.StatusCode, http.StatusForbidden, portStr)
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

// An entry that declares a #52 credential audience is REFUSED when NO
// credential source is wired. The source is optional, but its absence is never
// permissive: a declared-identity entry that quietly downgrades to an
// unauthenticated allow-through is the fail-open shape this change exists to
// prevent, and a deployment that forgot to wire the seam must find out by
// losing the destination, not by silently losing the identity.
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

	resp, _ := proxyGet(t, sock, "http://"+addr+"/")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}

	refusals := rec.snapshot()
	if len(refusals) != 1 || refusals[0].Reason != RefusedCredentialUnavailable {
		t.Fatalf("refusals = %+v, want one %q", refusals, RefusedCredentialUnavailable)
	}
}

// A CONNECT to a destination declared WITH a credential audience is refused
// EXPLICITLY, with its own bounded reason.
//
// The proxy cannot inject a delegated credential into a tunnel it does not
// terminate: the client's next bytes are a TLS ClientHello, and intercepting
// them is out of scope. Dropping them silently would be fail-closed but
// undiagnosable — an operator would see a hang and no reason. So the refusal is
// stated, before the upstream is dialled, and named distinctly enough that
// "this destination needs TLS interception fuse does not do" is readable from
// the reason alone.
func TestProxyRefusesConnectToCredentialedDestination(t *testing.T) {
	addr := upstream(t, "must never be tunnelled")
	src := &fakeCredentialSource{fn: func(loopauth.Principal, toolidentity.Target) (toolidentity.Credential, error) {
		return toolidentity.NewCredential("Bearer", "alice-token", true), nil
	}}

	rec := &recordingHooks{}
	p := newTestProxy(t, withProxyHooks(rec.hooks()), withCredentialSource(src))
	sock, err := p.Listen(principal("acme", "alice"), allowHostPortCredential(t, addr, "internal-api"))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	_, _, resp := connectVia(t, sock, addr)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
	_ = resp.Body.Close()

	refusals := rec.snapshot()
	if len(refusals) != 1 || refusals[0].Reason != RefusedCredentialTunnel {
		t.Fatalf("refusals = %+v, want one %q", refusals, RefusedCredentialTunnel)
	}
	// The refusal happens BEFORE resolution: no credential is minted for a
	// request that is never going to carry one.
	if calls := src.snapshot(); len(calls) != 0 {
		t.Errorf("credential source was consulted %d times for a refused tunnel: %+v", len(calls), calls)
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

// A listener is LEASED, one lease per Listen, and is torn down when the LAST
// lease is dropped — not when the first holder happens to finish.
//
// This is the whole reason Release can be wired to a per-sandbox teardown at
// all. Listen is idempotent per principal, so several concurrent sandboxes for
// one principal share one socket; if the first of them to be released closed
// that socket, it would cut the others' live tunnels. Counting is what makes
// "this principal's sandbox usage ended" a statement the proxy can act on.
//
// The re-issued path is asserted DIFFERENT: a reclaimed socket path is never
// handed out again, so there is no window in which a stale mount could reach a
// listener bound to a policy other than the one it was created with.
func TestProxyListenerIsLeasedAndTornDownAtTheLastRelease(t *testing.T) {
	p := newTestProxy(t)
	alice := principal("acme", "alice")
	policy := Egress{Mode: EgressEnforce}

	first, err := p.Listen(alice, policy)
	if err != nil {
		t.Fatalf("Listen(alice) #1: %v", err)
	}
	second, err := p.Listen(alice, policy)
	if err != nil {
		t.Fatalf("Listen(alice) #2: %v", err)
	}
	if first != second {
		t.Fatalf("Listen is not idempotent per principal: %q then %q", first, second)
	}

	// Two leases, one release: the socket is still serving the holder that has
	// not finished.
	if err := p.Release(alice); err != nil {
		t.Fatalf("Release(alice) #1: %v", err)
	}
	c, err := net.Dial("unix", first)
	if err != nil {
		t.Fatalf("dial %s after one of two leases was dropped: %v", first, err)
	}
	_ = c.Close()

	// The last lease is what tears it down.
	if err := p.Release(alice); err != nil {
		t.Fatalf("Release(alice) #2: %v", err)
	}
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Errorf("stat(%s) after the last Release = %v, want not-exist", first, err)
	}
	if _, err := os.Stat(filepath.Dir(first)); !os.IsNotExist(err) {
		t.Errorf("socket directory survived the last Release")
	}

	// An unmatched Release is not an error: teardown paths call it
	// unconditionally, and a principal with no listener is a no-op.
	if err := p.Release(alice); err != nil {
		t.Errorf("Release(alice) with no listener = %v, want nil", err)
	}

	third, err := p.Listen(alice, policy)
	if err != nil {
		t.Fatalf("Listen(alice) after full release: %v", err)
	}
	if third == first {
		t.Errorf("re-issued socket path %q reuses the reclaimed one", third)
	}
}

// Release must be safe against connections that are LIVE at the moment it runs:
// an established tunnel, and clients arriving in the race with teardown. It must
// not panic, must not block on a tunnel that could be idle for hours, and must
// leave no accept or connection goroutine behind (which -race and the proxy's
// own wait groups together assert).
func TestProxyReleaseWithConnectionsInFlight(t *testing.T) {
	up := upstream(t, "marker")
	p := newTestProxy(t)
	alice := principal("acme", "alice")
	sock, err := p.Listen(alice, allowHostPort(t, up))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	// One tunnel already established and idle — the case a timeout would never
	// reach, since the header deadline is cleared once a tunnel exists.
	conn, br, resp := connectVia(t, sock, up)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}

	// And a crowd arriving while teardown runs. Every outcome is acceptable here
	// except a panic or a hang: a client may be served, refused, or find no
	// socket at all.
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, derr := net.Dial("unix", sock)
			if derr != nil {
				return
			}
			defer func() { _ = c.Close() }()
			_, _ = io.WriteString(c, "CONNECT "+up+" HTTP/1.1\r\nHost: "+up+"\r\n\r\n")
			_, _ = io.Copy(io.Discard, c)
		}()
	}

	released := make(chan error, 1)
	go func() { released <- p.Release(alice) }()
	select {
	case err := <-released:
		if err != nil {
			t.Fatalf("Release with connections in flight: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Release blocked on an in-flight connection")
	}
	wg.Wait()

	// The idle tunnel was CUT, not left dangling on a listener nobody enforces.
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := br.Read(make([]byte, 1)); err == nil {
		t.Error("tunnel still readable after Release")
	}
}

// The lease invariant under CONCURRENCY, which is the shape it will actually be
// used in: many sandboxes for one principal, opening and finishing in any order,
// from goroutines that know nothing about each other.
//
// The assertion is the one that matters and the one an unconditional Release
// breaks immediately: WHILE a holder's lease is outstanding, the listener it was
// given is still registered and its socket is still on disk. Another goroutine
// finishing must not take it away.
//
// Liveness is asserted through the proxy's own bookkeeping and the filesystem
// rather than by dialling, deliberately. A synthetic burst of hundreds of
// connects overruns the kernel's accept queue and is refused there, which is a
// property of the burst and not of the listener's lifetime; it would make this
// test flaky under -race while telling us nothing. Serving DURING teardown is
// covered by TestProxyReleaseWithConnectionsInFlight, over real connections.
func TestProxyLeasesAreSafeUnderConcurrentListenAndRelease(t *testing.T) {
	p := newTestProxy(t)
	alice := principal("acme", "alice")
	policy := Egress{Mode: EgressEnforce}

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 8; j++ {
				sock, err := p.Listen(alice, policy)
				if err != nil {
					t.Errorf("Listen: %v", err)
					return
				}
				// The lease is HELD right here.
				if _, serr := os.Stat(sock); serr != nil {
					t.Errorf("stat(%s) while holding a lease: %v", sock, serr)
				}
				p.mu.Lock()
				pl := p.listeners[principalKey(alice)]
				p.mu.Unlock()
				switch {
				case pl == nil:
					t.Errorf("listener for %q was torn down while a lease was held", sock)
				case pl.path != sock:
					t.Errorf("listener path = %q while a lease on %q was held", pl.path, sock)
				}
				if rerr := p.Release(alice); rerr != nil {
					t.Errorf("Release: %v", rerr)
				}
			}
		}()
	}
	wg.Wait()

	// Every lease was dropped, so nothing is left: no listener, and an empty
	// fuse-owned root rather than a directory per sandbox.
	p.mu.Lock()
	remaining := len(p.listeners)
	p.mu.Unlock()
	if remaining != 0 {
		t.Errorf("listeners after every lease was dropped = %d, want 0", remaining)
	}
	entries, err := os.ReadDir(p.Root())
	if err != nil {
		t.Fatalf("ReadDir(root): %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("%d socket director(ies) left under the root, want 0", len(entries))
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

	// The ABSOLUTE-FORM path canonicalizes at the SAME entry point — one
	// normalizer, two request shapes. A second normalizer that drifted from this
	// one is the ADR-0048 defect, so both shapes are asserted against the same
	// declared entry.
	for _, spelling := range []string{"localhost", "LocalHost", "localhost."} {
		rawURL := fmt.Sprintf("http://%s:%d/", spelling, port)
		resp, body := proxyGet(t, sock, rawURL)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("absolute-form spelling %q got %d, want 200", spelling, resp.StatusCode)
			continue
		}
		if body != "canonical" {
			t.Errorf("absolute-form spelling %q: body = %q, want %q", spelling, body, "canonical")
		}
	}

	for _, info := range rec.snapshot() {
		t.Errorf("declared host refused: %+v", info)
	}
}
