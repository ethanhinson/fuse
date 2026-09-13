package sandbox

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
)

// tlsProxy builds a Proxy with a TLS listener on loopback and returns it with
// the listener's address and its CA.
//
// The proxy's UNIX root is still a short directory, because ListenTLS does not
// replace the socket path — the two listeners coexist and several tests below
// assert exactly that.
func tlsProxy(t *testing.T, opts ...ProxyOption) (*Proxy, string, *CA) {
	t.Helper()
	p := newTestProxy(t, opts...)
	ca, err := NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	if err := p.ListenTLS("127.0.0.1:0", ca); err != nil {
		t.Fatalf("ListenTLS: %v", err)
	}
	addr := p.TLSAddr()
	if addr == "" {
		t.Fatal("TLSAddr is empty after ListenTLS")
	}
	return p, addr, ca
}

// clientTLS turns a minted credential into the tls.Config a sidecar would use:
// the client certificate it was issued, and the CA it must verify the proxy
// against.
func clientTLS(t *testing.T, cred ClientCredential) *tls.Config {
	t.Helper()
	cert, err := tls.X509KeyPair(cred.CertPEM, cred.KeyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(cred.CAPEM) {
		t.Fatal("AppendCertsFromPEM: no CA certificate in the credential")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   proxyTLSServerName,
		MinVersion:   tls.VersionTLS13,
	}
}

// dialTLS connects to the proxy's TLS listener with cred's material and
// completes the handshake.
func dialTLS(t *testing.T, addr string, cred ClientCredential) *tls.Conn {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, clientTLS(t, cred))
	if err != nil {
		t.Fatalf("tls.Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.Handshake(); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	return conn
}

// connectOverTLS issues one CONNECT over an established TLS connection and
// returns the response.
func connectOverTLS(t *testing.T, conn net.Conn, target string) (*bufio.Reader, *http.Response) {
	t.Helper()
	req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	return br, resp
}

// getOverTLS issues one absolute-form request over an established TLS
// connection and returns the response and body.
func getOverTLS(t *testing.T, conn net.Conn, rawURL, host string) (*http.Response, string) {
	t.Helper()
	req := "GET " + rawURL + " HTTP/1.1\r\nHost: " + host + "\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatalf("write request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
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

// tlsPrincipal names a principal for the TLS-path tests. The socket-path tests
// have tlsPrincipal() (host_test.go), which takes no arguments; these tests
// need two DISTINCT principals in one process, which is the whole point.
func tlsPrincipal(tenant, subject string) loopauth.Principal {
	return loopauth.Principal{Tenant: event.TenantID(tenant), Subject: subject}
}

// TWO ENROLLED PRINCIPALS, TWO POLICIES, ONE LISTENER.
//
// This is the TLS-path analogue of TestProxyConcurrentPrincipalsSeeOnlyTheirOwn
// Policy, and it is the whole reason the serial — not the connection, not a
// header, not the SNI — selects the policy. Both principals share ONE TCP
// listener, so the structural per-socket identity the UNIX path relies on is
// simply not available here; the certificate is what is left, and it must be
// enough.
func TestProxyTLSEnrolledPrincipalsGetTheirOwnPolicy(t *testing.T) {
	upA := upstream(t, "A")
	upB := upstream(t, "B")

	p, addr, _ := tlsProxy(t)

	pa := tlsPrincipal("tenant-a", "subject-a")
	pb := tlsPrincipal("tenant-b", "subject-b")

	credA, err := p.Enroll(pa, allowHostPort(t, upA))
	if err != nil {
		t.Fatalf("Enroll A: %v", err)
	}
	credB, err := p.Enroll(pb, allowHostPort(t, upB))
	if err != nil {
		t.Fatalf("Enroll B: %v", err)
	}

	// Each reaches its OWN upstream.
	if _, body := getOverTLS(t, dialTLS(t, addr, credA), "http://"+upA+"/", upA); body != "A" {
		t.Fatalf("A through its own policy: body = %q, want %q", body, "A")
	}
	if _, body := getOverTLS(t, dialTLS(t, addr, credB), "http://"+upB+"/", upB); body != "B" {
		t.Fatalf("B through its own policy: body = %q, want %q", body, "B")
	}

	// And neither reaches the OTHER's, which is the part that would pass anyway
	// if the two policies had been merged, so it is asserted with a distinct
	// refusal check.
	resp, _ := getOverTLS(t, dialTLS(t, addr, credA), "http://"+upB+"/", upB)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("A reaching B's upstream: status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
	resp, _ = getOverTLS(t, dialTLS(t, addr, credB), "http://"+upA+"/", upA)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("B reaching A's upstream: status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}

// CROSS-PRINCIPAL CERTIFICATE USE IS REFUSED.
//
// The spec's acceptance names this case directly: "two principals' Pods cannot
// use each other's certificates". There is nothing to assert about A "presenting
// B's certificate" beyond this: the certificate IS the identity, so presenting
// B's material makes you B, and what must hold is that it gets B's POLICY and
// never A's. A Pod that stole B's key therefore gains B's allowlist, not a merge
// of the two and not a bypass — and it cannot reach A's destinations at all.
func TestProxyTLSCrossPrincipalCertificateGetsOnlyTheOtherPolicy(t *testing.T) {
	upA := upstream(t, "A")
	upB := upstream(t, "B")

	p, addr, _ := tlsProxy(t)

	pa := tlsPrincipal("tenant-a", "subject-a")
	pb := tlsPrincipal("tenant-b", "subject-b")

	if _, err := p.Enroll(pa, allowHostPort(t, upA)); err != nil {
		t.Fatalf("Enroll A: %v", err)
	}
	credB, err := p.Enroll(pb, allowHostPort(t, upB))
	if err != nil {
		t.Fatalf("Enroll B: %v", err)
	}

	// A's sandbox somehow holds B's credential. It reaches B's destination (it
	// IS B as far as the proxy can tell) and A's is refused.
	resp, _ := getOverTLS(t, dialTLS(t, addr, credB), "http://"+upA+"/", upA)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("B's cert reaching A's upstream: status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
}

// A CERTIFICATE FROM ANOTHER CA IS REFUSED AT THE HANDSHAKE.
//
// The serial space is per-CA, so a cert minted by a DIFFERENT in-process CA can
// trivially carry a serial this proxy has registered. mTLS verification against
// the proxy's own CA pool is what stops that, and it must happen before the
// serial is even looked up.
func TestProxyTLSForeignCAIsRefusedAtHandshake(t *testing.T) {
	up := upstream(t, "A")
	p, addr, _ := tlsProxy(t)

	pa := tlsPrincipal("tenant-a", "subject-a")
	credA, err := p.Enroll(pa, allowHostPort(t, up))
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	// A second, unrelated CA mints a credential carrying THE REGISTERED SERIAL.
	// This is the whole attack the serial-keyed registry invites: the serial is
	// an opaque integer in a per-process map, so an attacker who learns one (it
	// is in a certificate handed to a sandbox) can put it in a certificate of
	// their own. Only verification against the proxy's CA stops it — reading the
	// serial from PeerCertificates rather than VerifiedChains, or weakening
	// ClientAuth below RequireAndVerifyClientCert, serves this connection.
	other, err := NewCA()
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	registered, ok := new(big.Int).SetString(credA.Serial, 16)
	if !ok {
		t.Fatalf("parse serial %q", credA.Serial)
	}
	forged, err := other.issueWithSerial(pa, "deadbeef", registered)
	if err != nil {
		t.Fatalf("issueWithSerial: %v", err)
	}
	// The client must still trust the REAL proxy's CA to get as far as sending
	// its own certificate, so the CA bundle is the proxy's and only the client
	// certificate is forged.
	cfg := clientTLS(t, forged)
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(credA.CAPEM) {
		t.Fatal("AppendCertsFromPEM")
	}
	cfg.RootCAs = pool

	conn, err := tls.Dial("tcp", addr, cfg)
	if err == nil {
		// TLS 1.3 defers the server's certificate verdict to its first flight
		// after the handshake, so a Dial that "succeeded" must still fail on
		// the first read or write.
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, werr := io.WriteString(conn, "GET http://"+up+"/ HTTP/1.1\r\nHost: "+up+"\r\n\r\n"); werr == nil {
			buf := make([]byte, 1)
			if _, rerr := conn.Read(buf); rerr == nil {
				t.Fatal("a certificate from a foreign CA was served")
			}
		}
		return
	}
	if !strings.Contains(err.Error(), "certificate") && !strings.Contains(err.Error(), "bad certificate") {
		t.Fatalf("foreign CA: err = %v, want a certificate rejection", err)
	}
}

// THE POLICY IS SELECTED BEFORE ANY APPLICATION BYTE IS READ.
//
// The assertion is made by CONNECTING AND CLOSING WITHOUT WRITING: an unknown
// serial's connection must be closed by the proxy on the strength of the
// handshake alone. If the lookup happened after the first request were read, a
// client that sends nothing would be held until the header timeout instead — so
// this test also pins the failure mode a later "read the request first, then
// decide" refactor would introduce.
func TestProxyTLSUnknownSerialIsClosedWithZeroApplicationBytes(t *testing.T) {
	up := upstream(t, "A")
	p, addr, _ := tlsProxy(t)

	// A REGISTERED principal is live throughout, so "closed" cannot be an
	// accident of there being no policy to serve under: a lookup that fell back
	// to "whatever listener exists" would find this one and the read below would
	// succeed. That fallback is precisely the mutation this test must catch.
	if _, err := p.Enroll(tlsPrincipal("tenant-live", "subject-live"), allowHostPort(t, up)); err != nil {
		t.Fatalf("Enroll live: %v", err)
	}

	// A credential minted by the proxy's OWN CA whose serial is not registered:
	// the CA is the trust anchor, so this is the case that proves the SERIAL is
	// consulted and not merely the signature.
	unregistered, err := p.testCA().issue(tlsPrincipal("tenant-a", "subject-a"), "nonce")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	conn, err := tls.Dial("tcp", addr, clientTLS(t, unregistered))
	if err != nil {
		// Refusing inside the handshake is an even stronger form of the same
		// property.
		return
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	// NOT ONE BYTE IS WRITTEN. The close must come from the serial lookup.
	buf := make([]byte, 1)
	n, rerr := conn.Read(buf)
	if rerr == nil {
		t.Fatalf("unregistered serial: read %d bytes with no refusal", n)
	}
	if errors.Is(rerr, io.EOF) || strings.Contains(rerr.Error(), "reset") ||
		strings.Contains(rerr.Error(), "closed") || strings.Contains(rerr.Error(), "certificate") {
		return
	}
	t.Fatalf("unregistered serial: err = %v, want the connection closed", rerr)
}

// A KNOWN serial, by contrast, is held open with zero application bytes read —
// the mirror of the test above, so "closed" is shown to be a decision about the
// serial rather than what the proxy does to every silent client.
func TestProxyTLSKnownSerialIsHeldOpenWithZeroApplicationBytes(t *testing.T) {
	p, addr, _ := tlsProxy(t)
	cred, err := p.Enroll(tlsPrincipal("tenant-a", "subject-a"), Egress{})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	conn := dialTLS(t, addr, cred)
	_ = conn.SetReadDeadline(time.Now().Add(750 * time.Millisecond))
	buf := make([]byte, 1)
	_, rerr := conn.Read(buf)
	var ne net.Error
	if !errors.As(rerr, &ne) || !ne.Timeout() {
		t.Fatalf("known serial with no bytes written: err = %v, want a read timeout (the proxy waiting for a request)", rerr)
	}
}

// A RELEASED SERIAL IS CLOSED.
//
// Release revokes: the credential a torn-down sandbox holds must stop working
// the moment the sandbox is released, because the Pod may outlive the release by
// the length of one delete call and its sidecar is still holding a key.
func TestProxyTLSReleasedSerialIsClosed(t *testing.T) {
	up := upstream(t, "A")
	p, addr, _ := tlsProxy(t)

	pa := tlsPrincipal("tenant-a", "subject-a")
	cred, err := p.Enroll(pa, allowHostPort(t, up))
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	// A second principal stays enrolled for the whole test, with the SAME
	// allowlist, so a revoked serial that fell back to "some live listener"
	// would still reach the upstream — and the assertion below would catch it.
	if _, err := p.Enroll(tlsPrincipal("tenant-live", "subject-live"), allowHostPort(t, up)); err != nil {
		t.Fatalf("Enroll live: %v", err)
	}

	// It works first, so the close below is demonstrably the revocation.
	if _, body := getOverTLS(t, dialTLS(t, addr, cred), "http://"+up+"/", up); body != "A" {
		t.Fatalf("before release: body = %q, want %q", body, "A")
	}

	if err := p.Release(pa); err != nil {
		t.Fatalf("Release: %v", err)
	}

	conn, err := tls.Dial("tcp", addr, clientTLS(t, cred))
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, werr := io.WriteString(conn, "GET http://"+up+"/ HTTP/1.1\r\nHost: "+up+"\r\n\r\n"); werr == nil {
		buf := make([]byte, 1)
		if n, rerr := conn.Read(buf); rerr == nil {
			t.Fatalf("released serial: read %d bytes, want the connection closed", n)
		}
	}
}

// ENROLL IS LEASE-COUNTED LIKE LISTEN.
//
// Several concurrent sandboxes for one principal each enroll, and one of them
// finishing must not revoke the others' certificates. The lease count is shared
// with Listen's, because it is the same listener object holding the same policy.
func TestProxyTLSEnrollIsLeaseCountedWithListen(t *testing.T) {
	up := upstream(t, "A")
	p, addr, _ := tlsProxy(t)

	pa := tlsPrincipal("tenant-a", "subject-a")
	policy := allowHostPort(t, up)

	credOne, err := p.Enroll(pa, policy)
	if err != nil {
		t.Fatalf("Enroll one: %v", err)
	}
	credTwo, err := p.Enroll(pa, policy)
	if err != nil {
		t.Fatalf("Enroll two: %v", err)
	}
	if credOne.Serial == credTwo.Serial {
		t.Fatal("two enrollments of one principal share a serial; each sandbox must be revocable on its own terms")
	}

	// One lease drops. The other's credential must still work.
	if err := p.Release(pa); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, body := getOverTLS(t, dialTLS(t, addr, credTwo), "http://"+up+"/", up); body != "A" {
		t.Fatalf("after one of two releases: body = %q, want %q", body, "A")
	}

	// The last lease drops and now nothing works.
	if err := p.Release(pa); err != nil {
		t.Fatalf("Release: %v", err)
	}
	conn, err := tls.Dial("tcp", addr, clientTLS(t, credTwo))
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, werr := io.WriteString(conn, "GET http://"+up+"/ HTTP/1.1\r\nHost: "+up+"\r\n\r\n"); werr == nil {
		buf := make([]byte, 1)
		if n, rerr := conn.Read(buf); rerr == nil {
			t.Fatalf("after the last release: read %d bytes, want the connection closed", n)
		}
	}
}

// AN EMPTY ALLOWLIST UNDER `enforce` REFUSES — IT DOES NOT BLACK OUT.
//
// ADR-0053's salvaged posture. The listener still runs, the handshake still
// completes, and the proxy answers 403: deny-all is a PROXY DECISION the
// operator can observe in the refusal hook, distinguishable from a missing
// datapath (which presents as a connection error and no refusal at all).
func TestProxyTLSEnforceWithEmptyAllowlistRefusesRatherThanBlackingOut(t *testing.T) {
	up := upstream(t, "A")

	hooks := &recordingHooks{}
	p, addr, _ := tlsProxy(t, WithProxyHooks(hooks.hooks()))

	pa := tlsPrincipal("tenant-a", "subject-a")
	// Enforce with NO entries. loadEgress with no entries is exactly the
	// salvaged posture: the mode is enforcement and the allowlist is empty.
	cred, err := p.Enroll(pa, loadEgress(t))
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	resp, body := getOverTLS(t, dialTLS(t, addr, cred), "http://"+up+"/", up)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("empty allowlist: status = %d, want %d (a refusal, not a blackout)", resp.StatusCode, http.StatusForbidden)
	}
	if body != egressDenialBody {
		t.Fatalf("empty allowlist: body = %q, want %q", body, egressDenialBody)
	}

	refusals := hooks.snapshot()
	if len(refusals) != 1 {
		t.Fatalf("refusals = %d, want 1 (the operator must SEE the deny-all decision)", len(refusals))
	}
	if refusals[0].Reason != RefusedNotDeclared {
		t.Fatalf("reason = %q, want %q", refusals[0].Reason, RefusedNotDeclared)
	}
	if refusals[0].Principal != pa {
		t.Fatalf("principal = %+v, want %+v", refusals[0].Principal, pa)
	}
}

// THE CEILINGS ARE THE SAME SEMAPHORE, WITH THE SAME REASONS.
//
// Not a new bound and not new configuration: the TLS listener charges the SAME
// per-principal semaphore the UNIX socket does, so a principal cannot double its
// share by reaching the proxy over both transports.
func TestProxyTLSChargesThePerPrincipalConnectionCeiling(t *testing.T) {
	hooks := &recordingHooks{}
	p, addr, _ := tlsProxy(t, WithProxyHooks(hooks.hooks()), withConnectionLimits(1, 16))

	pa := tlsPrincipal("tenant-a", "subject-a")
	cred, err := p.Enroll(pa, loadEgress(t))
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	// The first connection takes the only slot and is held open (no bytes
	// written, so the proxy is parked reading the request line).
	first := dialTLS(t, addr, cred)
	_ = first

	// The second must be refused at capacity.
	deadline := time.Now().Add(5 * time.Second)
	for {
		refusals := hooks.snapshot()
		if len(refusals) > 0 {
			if refusals[0].Reason != RefusedPrincipalConnLimit {
				t.Fatalf("reason = %q, want %q", refusals[0].Reason, RefusedPrincipalConnLimit)
			}
			return
		}
		conn, derr := tls.Dial("tcp", addr, clientTLS(t, cred))
		if derr == nil {
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			buf := make([]byte, 64)
			_, _ = conn.Read(buf)
			_ = conn.Close()
		}
		if time.Now().After(deadline) {
			t.Fatal("the second connection was never refused at the per-principal ceiling")
		}
	}
}

// A CONNECT TO A CREDENTIALED DESTINATION IS STILL REFUSED ON THIS TRANSPORT.
//
// ADR-0052's refusal to terminate TLS for a credentialed DESTINATION is a
// different hop from this listener's transport TLS, and it must survive
// unchanged: the sandbox→proxy leg being encrypted says nothing about whether
// the proxy can put an Authorization header into a tunnel it does not terminate.
func TestProxyTLSStillRefusesConnectToCredentialedDestination(t *testing.T) {
	up := upstream(t, "A")
	hooks := &recordingHooks{}
	p, addr, _ := tlsProxy(t, WithProxyHooks(hooks.hooks()))

	host, port, err := net.SplitHostPort(up)
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	policy := loadEgress(t, fmt.Sprintf("    - host: %s\n      port: %s\n      credential: some-audience\n", host, port))

	cred, err := p.Enroll(tlsPrincipal("tenant-a", "subject-a"), policy)
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	_, resp := connectOverTLS(t, dialTLS(t, addr, cred), up)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("CONNECT to a credentialed destination: status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
	refusals := hooks.snapshot()
	if len(refusals) != 1 || refusals[0].Reason != RefusedCredentialTunnel {
		t.Fatalf("refusals = %+v, want exactly one %q", refusals, RefusedCredentialTunnel)
	}
}

// THE TWO TRANSPORTS COEXIST.
//
// ListenTLS does not replace the UNIX socket: a local container handler and a
// remote sandbox Pod may be live in one process at once, and one principal may
// legitimately hold both.
func TestProxyTLSAndSocketServeTheSamePrincipalConcurrently(t *testing.T) {
	up := upstream(t, "A")
	p, addr, _ := tlsProxy(t)

	pa := tlsPrincipal("tenant-a", "subject-a")
	policy := allowHostPort(t, up)

	sock, err := p.Listen(pa, policy)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	cred, err := p.Enroll(pa, policy)
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	if _, body := proxyGet(t, sock, "http://"+up+"/"); body != "A" {
		t.Fatalf("over the socket: body = %q, want %q", body, "A")
	}
	if _, body := getOverTLS(t, dialTLS(t, addr, cred), "http://"+up+"/", up); body != "A" {
		t.Fatalf("over TLS: body = %q, want %q", body, "A")
	}
}

// ENROLL AFTER Close REFUSES, and Close tears the TLS listener down.
func TestProxyTLSCloseStopsTheListenerAndEnroll(t *testing.T) {
	p, addr, _ := tlsProxy(t)
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := p.Enroll(tlsPrincipal("tenant-a", "subject-a"), Egress{}); !errors.Is(err, ErrProxyClosed) {
		t.Fatalf("Enroll after Close: err = %v, want %v", err, ErrProxyClosed)
	}
	if _, err := net.DialTimeout("tcp", addr, 2*time.Second); err == nil {
		t.Fatal("the TLS listener is still accepting after Close")
	}
}

// ListenTLS IS IDEMPOTENT-ONCE: a second call refuses rather than opening a
// second listener nobody tracks.
func TestProxyListenTLSTwiceRefuses(t *testing.T) {
	p, _, ca := tlsProxy(t)
	if err := p.ListenTLS("127.0.0.1:0", ca); err == nil {
		t.Fatal("a second ListenTLS opened another listener; want a refusal")
	}
}

// A NIL CA IS A REFUSAL, not a listener with no trust anchor.
func TestProxyListenTLSWithoutCARefuses(t *testing.T) {
	p := newTestProxy(t)
	if err := p.ListenTLS("127.0.0.1:0", nil); err == nil {
		t.Fatal("ListenTLS(nil CA) succeeded; a listener with no trust anchor must be refused")
	}
}

// Enroll BEFORE ListenTLS refuses: a credential for a datapath that does not
// exist would make a sandbox's sidecar dial nothing forever.
func TestProxyEnrollWithoutListenerRefuses(t *testing.T) {
	p := newTestProxy(t)
	if _, err := p.Enroll(tlsPrincipal("tenant-a", "subject-a"), Egress{}); err == nil {
		t.Fatal("Enroll without a TLS listener succeeded; want a refusal")
	}
}

// THE SAN URI CARRIES THE PRINCIPAL KEY AND A NONCE — spec §5's
// `fuse://sandbox/<principalKey>/<nonce>`. It is legibility for an operator
// reading a certificate, NOT the selector: the serial is.
func TestProxyEnrollSANURIShape(t *testing.T) {
	p, _, _ := tlsProxy(t)
	pa := tlsPrincipal("tenant-a", "subject-a")
	cred, err := p.Enroll(pa, Egress{})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	leaf := parseLeaf(t, cred.CertPEM)
	if len(leaf.URIs) != 1 {
		t.Fatalf("URIs = %v, want exactly one", leaf.URIs)
	}
	got := leaf.URIs[0].String()
	if !strings.HasPrefix(got, "fuse://sandbox/") {
		t.Fatalf("SAN URI = %q, want the fuse://sandbox/ prefix", got)
	}
	// The principal key is hashed rather than spelled: a certificate is handed
	// to a sandbox, and a raw tenant id in it is an identity disclosure.
	if strings.Contains(got, "tenant-a") || strings.Contains(got, "subject-a") {
		t.Fatalf("SAN URI = %q, must not spell the raw principal", got)
	}
	if leaf.SerialNumber == nil || leaf.SerialNumber.Sign() <= 0 {
		t.Fatalf("serial = %v, want a positive serial", leaf.SerialNumber)
	}
	if leaf.SerialNumber.Text(16) != cred.Serial {
		t.Fatalf("cred.Serial = %q, want the certificate's own serial %q", cred.Serial, leaf.SerialNumber.Text(16))
	}
	if !leaf.NotAfter.After(time.Now()) {
		t.Fatalf("NotAfter = %v, want a future expiry", leaf.NotAfter)
	}
	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Fatalf("ExtKeyUsage = %v, want client-auth only", leaf.ExtKeyUsage)
	}
}

func parseLeaf(t *testing.T, pemBytes []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("no CERTIFICATE block in %q", string(pemBytes))
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	return leaf
}

// THE PINNED SERVER NAME IS A CROSS-BINARY CONTRACT.
//
// The sidecar verifies a FIXED name against the mounted CA rather than the
// upstream IP, because the upstream is a Pod IP that changes on every reschedule.
// If this spelling and cmd/fuse-egress-forward's `proxyServerName` drift apart,
// every sandbox's egress fails its handshake — and the symptom inside the sandbox
// is an unexplained connection error, not a name-mismatch anyone can read.
//
// The forwarder is a dependency-free static binary that imports nothing of fuse's
// own (CGO_ENABLED=0, no libc, no interpreter), so the two constants cannot be one
// constant. Pinning the literal on both sides is the substitute, and this is the
// half that lives here.
func TestProxyTLSServerNameIsThePinnedLiteral(t *testing.T) {
	const pinned = "fuse-egress-proxy"
	if proxyTLSServerName != pinned {
		t.Fatalf("proxyTLSServerName = %q, want %q — cmd/fuse-egress-forward's proxyServerName pins the same literal and the two must agree", proxyTLSServerName, pinned)
	}
}
