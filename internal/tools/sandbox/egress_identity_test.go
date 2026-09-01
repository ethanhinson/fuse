package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/loopauth"
	"github.com/ethanhinson/fuse/internal/toolidentity"
)

// credentialCall is one observed resolution through the #52 seam. Only the
// PRINCIPAL and the TARGET are recorded: the resolved credential never enters a
// test record, for the same reason it never enters a log line.
type credentialCall struct {
	principal loopauth.Principal
	target    toolidentity.Target
}

// fakeCredentialSource stands in for the #52 broker. Its default behaviour is to
// FAIL: a test that wants a credential says so, so no test can pass by
// accidentally resolving one.
type fakeCredentialSource struct {
	mu    sync.Mutex
	calls []credentialCall
	fn    func(loopauth.Principal, toolidentity.Target) (toolidentity.Credential, error)
}

func (f *fakeCredentialSource) CredentialFor(_ context.Context, p loopauth.Principal, t toolidentity.Target) (toolidentity.Credential, error) {
	f.mu.Lock()
	f.calls = append(f.calls, credentialCall{principal: p, target: t})
	fn := f.fn
	f.mu.Unlock()
	if fn == nil {
		return toolidentity.Credential{}, errors.New("fake: no behaviour configured")
	}
	return fn(p, t)
}

func (f *fakeCredentialSource) snapshot() []credentialCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]credentialCall(nil), f.calls...)
}

// authUpstream is an upstream that records the Authorization header of every
// request it serves, so a test can assert what the DOWNSTREAM actually saw
// rather than what the proxy believes it sent.
func authUpstream(t *testing.T, marker string) (addr string, seen func() []string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Get("Authorization"))
		mu.Unlock()
		_, _ = io.WriteString(w, marker)
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String(), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

// allowHostPortCredential declares one destination carrying a #52 credential
// audience, THROUGH THE REAL LOADER, so the proxy matches the same AllowEntry an
// operator's file would produce.
func allowHostPortCredential(t *testing.T, hostport, credential string) Egress {
	t.Helper()
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", hostport, err)
	}
	return loadEgress(t, fmt.Sprintf("    - host: %s\n      port: %s\n      credential: %s\n", host, port, credential))
}

// An entry with NO credential audience is a plain allow-through, and the #52
// seam is not consulted at all. A proxy that resolved an identity for every
// declared destination would mint credentials nobody asked for.
func TestProxyPlainEntryDoesNotConsultCredentialSource(t *testing.T) {
	addr, seen := authUpstream(t, "plain")
	src := &fakeCredentialSource{fn: func(loopauth.Principal, toolidentity.Target) (toolidentity.Credential, error) {
		return toolidentity.NewCredential("Bearer", "should-never-be-minted", true), nil
	}}

	p := newTestProxy(t, WithProxyCredentialSource(src))
	sock, err := p.Listen(principal("acme", "alice"), allowHostPort(t, addr))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	resp, body := proxyGet(t, sock, "http://"+addr+"/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if body != "plain" {
		t.Errorf("body = %q, want %q", body, "plain")
	}
	if calls := src.snapshot(); len(calls) != 0 {
		t.Errorf("credential source was consulted %d times for an entry with no credential: %+v", len(calls), calls)
	}
	for _, auth := range seen() {
		if auth != "" {
			t.Errorf("upstream saw Authorization %q, want none", auth)
		}
	}
}

// A declared entry that names a credential audience reaches the upstream UNDER
// THAT IDENTITY, and the seam is asked using the LISTENER's principal and the
// entry's audience — never anything the client sent.
//
// Driven through the ABSOLUTE-FORM request an injected HTTP_PROXY actually
// produces for an `http://` destination, because that is the only client-emitted
// shape the proxy can inject a header into. A hand-crafted CONNECT-then-plaintext
// tunnel would exercise the header-setting line without proving any real command
// can reach it.
func TestProxyCredentialEntryDelegatesIdentityUpstream(t *testing.T) {
	addr, seen := authUpstream(t, "delegated")
	src := &fakeCredentialSource{fn: func(loopauth.Principal, toolidentity.Target) (toolidentity.Credential, error) {
		return toolidentity.NewCredential("Bearer", "alice-token", true), nil
	}}

	rec := &recordingHooks{}
	p := newTestProxy(t, WithProxyHooks(rec.hooks()), WithProxyCredentialSource(src))
	sock, err := p.Listen(principal("acme", "alice"), allowHostPortCredential(t, addr, "internal-api"))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	// The client names another principal in a header, and presents its OWN
	// Authorization. Neither takes part: the identity comes from the listener,
	// and the client's header is REPLACED, not merged, so the upstream cannot be
	// offered a credential the model chose.
	resp, body := proxyGet(t, sock, "http://"+addr+"/",
		"X-Fuse-Principal: bob",
		"Authorization: Bearer forged-by-the-model")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if body != "delegated" {
		t.Errorf("body = %q, want %q", body, "delegated")
	}
	// The delegated credential goes UPSTREAM only. It is never reflected back to
	// the client, which is the model's command.
	for name, values := range resp.Header {
		for _, v := range values {
			if strings.Contains(v, "alice-token") {
				t.Errorf("response header %s echoed the delegated credential: %q", name, v)
			}
		}
	}
	if strings.Contains(body, "alice-token") {
		t.Errorf("response body echoed the delegated credential: %q", body)
	}

	auths := seen()
	if len(auths) != 1 || auths[0] != "Bearer alice-token" {
		t.Fatalf("upstream saw Authorization %q, want one %q", auths, "Bearer alice-token")
	}

	calls := src.snapshot()
	if len(calls) != 1 {
		t.Fatalf("seam calls = %+v, want exactly 1", calls)
	}
	call := calls[0]
	if call.principal.Subject != "alice" || string(call.principal.Tenant) != "acme" {
		t.Errorf("seam principal = %+v, want the LISTENER's acme/alice", call.principal)
	}
	if call.target.Audience != "internal-api" || call.target.Name != "internal-api" {
		t.Errorf("seam target = %+v, want name and audience %q", call.target, "internal-api")
	}
	if call.target.Tier != toolidentity.TierOAuth {
		t.Errorf("seam tier = %v, want TierOAuth", call.target.Tier)
	}
	for _, info := range rec.snapshot() {
		t.Errorf("credentialed destination refused: %+v", info)
	}
}

// addressedUpstream records BOTH halves of what identifies a forwarded request:
// the Authorization header fuse minted, and the Host header — the VIRTUAL host —
// the request was addressed to. A gateway, CDN, or reverse proxy in front of the
// declared TCP destination routes on the latter, so the pair is what decides
// which backend actually receives fuse's delegated credential.
type addressedRequest struct {
	auth string
	host string
}

func addressedUpstream(t *testing.T, marker string) (addr string, seen func() []addressedRequest) {
	t.Helper()
	var mu sync.Mutex
	var got []addressedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, addressedRequest{auth: r.Header.Get("Authorization"), host: r.Host})
		mu.Unlock()
		_, _ = io.WriteString(w, marker)
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String(), func() []addressedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]addressedRequest(nil), got...)
	}
}

// rawProxyRequest writes request bytes to the proxy socket VERBATIM and reads one
// response.
//
// proxyRequest derives the Host header from the request target, which is exactly
// what an honest client does — so it cannot express the case this test is about,
// where the request target and the Host header disagree. Here the caller owns
// every byte of the request.
func rawProxyRequest(t *testing.T, sock, raw string) (*http.Response, string) {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial %s: %v", sock, err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := io.WriteString(conn, raw); err != nil {
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

// THE IDENTITY-CARRYING REQUEST IS ADDRESSED TO THE DESTINATION THE OPERATOR
// DECLARED — not to anything the model's command chose.
//
// The TCP peer was never in doubt: it is dialled from the canonical host the
// allowlist matched. What this pins is the VIRTUAL host, the Host header the
// request is written with, because a declared destination is very often a
// gateway, a CDN, or a reverse proxy that routes on exactly that. If the model
// can steer it, fuse's minted delegated credential is presented to a backend the
// model selected — steering, not exfiltration, but still the identity path's
// central claim ("the upstream sees a credential fuse minted for the LISTENER's
// principal rather than anything the model's command chose") failing on its
// destination half.
//
// Two model-controlled inputs are driven at once, both of which net/http would
// otherwise put on the wire:
//
//   - The Host HEADER names an unrelated authority. For an absolute-form request
//     ReadRequest ignores it (RFC 7230 §5.3), so this half is a regression guard
//     on that: it is the exact shape the pre-forward-proxy origin-form path put
//     verbatim onto the upstream request.
//   - The request TARGET's authority is a non-canonical spelling of the declared
//     destination — a trailing root dot. It matches the allowlist, because
//     canonicalDestination strips the dot before the match, and the dial is
//     therefore correct; but it is the RAW spelling that ReadRequest puts in
//     req.Host and Request.Write emits. A Host-routing frontend that does not
//     normalize the dot sends "example.com." somewhere other than "example.com"
//     — typically its default backend, which is not the one the operator
//     declared.
func TestProxyIdentityRequestIsAddressedToDeclaredDestination(t *testing.T) {
	addr, seen := addressedUpstream(t, "delegated")
	src := &fakeCredentialSource{fn: func(loopauth.Principal, toolidentity.Target) (toolidentity.Credential, error) {
		return toolidentity.NewCredential("Bearer", "alice-token", true), nil
	}}

	rec := &recordingHooks{}
	p := newTestProxy(t, WithProxyHooks(rec.hooks()), WithProxyCredentialSource(src))
	sock, err := p.Listen(principal("acme", "alice"), allowHostPortCredential(t, addr, "internal-api"))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", addr, err)
	}
	// "127.0.0.1.:PORT" — the same destination, spelled so a naive vhost router
	// disagrees with the operator's declaration.
	steered := net.JoinHostPort(host+".", port)

	resp, body := rawProxyRequest(t, sock,
		"GET http://"+steered+"/ HTTP/1.1\r\n"+
			"Host: gateway-backend.chosen-by-the-model.invalid\r\n"+
			"\r\n")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the destination IS declared)", resp.StatusCode)
	}
	if body != "delegated" {
		t.Errorf("body = %q, want %q", body, "delegated")
	}
	for _, info := range rec.snapshot() {
		t.Errorf("declared destination refused: %+v", info)
	}

	got := seen()
	if len(got) != 1 {
		t.Fatalf("upstream served %d requests, want exactly 1", len(got))
	}
	// The credential is fuse's, minted for the listener's principal...
	if got[0].auth != "Bearer alice-token" {
		t.Errorf("upstream saw Authorization %q, want %q", got[0].auth, "Bearer alice-token")
	}
	// ...and the request carrying it is addressed to the OPERATOR's destination.
	if got[0].host != addr {
		t.Errorf("upstream saw Host %q, want the declared destination %q — the delegated credential was presented to a virtual host the model chose", got[0].host, addr)
	}
}

// canonicalAuthority is the only place the pinned Host value is spelled, so its
// edge cases are pinned directly rather than through a socket: the `http`
// scheme's default port is omitted the way every ordinary client omits it, any
// other port is present, and an IPv6 literal is bracketed. Getting the shape
// wrong would break vhost routing for real destinations while still "passing"
// the socket-level test above, which only ever sees a loopback address on a
// non-default port.
func TestCanonicalAuthoritySpelling(t *testing.T) {
	for _, tc := range []struct {
		host string
		port int
		want string
	}{
		{"api.example.com", 80, "api.example.com"},
		{"api.example.com", 8080, "api.example.com:8080"},
		{"127.0.0.1", 80, "127.0.0.1"},
		{"127.0.0.1", 3128, "127.0.0.1:3128"},
		{"::1", 80, "[::1]"},
		{"::1", 8080, "[::1]:8080"},
	} {
		if got := canonicalAuthority(tc.host, tc.port); got != tc.want {
			t.Errorf("canonicalAuthority(%q, %d) = %q, want %q", tc.host, tc.port, got, tc.want)
		}
	}
}

// The second half of the principal-scoping acceptance: two principals driven
// CONCURRENTLY through ONE proxy, each with its OWN credentialed entry, and each
// upstream must only ever see its own principal's credential. A shared "current
// credential" would pass a sequential test and fail this one.
func TestProxyConcurrentPrincipalsGetTheirOwnCredential(t *testing.T) {
	addrA, seenA := authUpstream(t, "A")
	addrB, seenB := authUpstream(t, "B")

	src := &fakeCredentialSource{fn: func(p loopauth.Principal, _ toolidentity.Target) (toolidentity.Credential, error) {
		return toolidentity.NewCredential("Bearer", p.Subject+"-token", true), nil
	}}
	p := newTestProxy(t, WithProxyCredentialSource(src))

	sockA, err := p.Listen(principal("acme", "alice"), allowHostPortCredential(t, addrA, "api-a"))
	if err != nil {
		t.Fatalf("Listen(alice): %v", err)
	}
	sockB, err := p.Listen(principal("acme", "bob"), allowHostPortCredential(t, addrB, "api-b"))
	if err != nil {
		t.Fatalf("Listen(bob): %v", err)
	}

	const rounds = 25
	var wg sync.WaitGroup
	drive := func(sock, addr, marker string) {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			resp, body := proxyGet(t, sock, "http://"+addr+"/")
			if resp.StatusCode != http.StatusOK {
				t.Errorf("%s: status = %d, want 200", marker, resp.StatusCode)
				continue
			}
			if body != marker {
				t.Errorf("%s: body = %q, want %q", marker, body, marker)
			}
		}
	}
	wg.Add(2)
	go drive(sockA, addrA, "A")
	go drive(sockB, addrB, "B")
	wg.Wait()

	assertAll := func(name string, auths []string, want string) {
		if len(auths) != rounds {
			t.Errorf("%s upstream served %d requests, want %d", name, len(auths), rounds)
		}
		for _, auth := range auths {
			if auth != want {
				t.Errorf("%s upstream saw Authorization %q, want %q", name, auth, want)
			}
		}
	}
	assertAll("alice's", seenA(), "Bearer alice-token")
	assertAll("bob's", seenB(), "Bearer bob-token")

	// Each resolution asked for the audience of the entry that matched, under
	// the principal of the listener it arrived on.
	byPrincipal := map[string]string{}
	for _, call := range src.snapshot() {
		if prev, ok := byPrincipal[call.principal.Subject]; ok && prev != call.target.Audience {
			t.Errorf("principal %s resolved two audiences: %q and %q", call.principal.Subject, prev, call.target.Audience)
		}
		byPrincipal[call.principal.Subject] = call.target.Audience
	}
	if byPrincipal["alice"] != "api-a" || byPrincipal["bob"] != "api-b" {
		t.Errorf("audiences by principal = %v, want alice->api-a and bob->api-b", byPrincipal)
	}
}

// A seam that FAILS refuses the connection. There is no unauthenticated
// fallback: a destination the operator declared under a delegated identity is
// never reached without one.
func TestProxyCredentialResolutionErrorRefuses(t *testing.T) {
	addr, seen := authUpstream(t, "never reached")
	// The source returns a USABLE credential ALONGSIDE its error — a partially
	// filled result is a real shape for an exchange that failed late. The error
	// alone must decide: a proxy that only checked whether a token happened to
	// be present would sail past this.
	src := &fakeCredentialSource{fn: func(loopauth.Principal, toolidentity.Target) (toolidentity.Credential, error) {
		return toolidentity.NewCredential("Bearer", "stale-token-from-a-failed-exchange", true),
			errors.New("exchange rejected the delegation")
	}}

	rec := &recordingHooks{}
	p := newTestProxy(t, WithProxyHooks(rec.hooks()), WithProxyCredentialSource(src))
	sock, err := p.Listen(principal("acme", "alice"), allowHostPortCredential(t, addr, "internal-api"))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	resp, _ := proxyGet(t, sock, "http://"+addr+"/")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}

	if got := seen(); len(got) != 0 {
		t.Errorf("upstream served %d requests, want none", len(got))
	}
	refusals := rec.snapshot()
	if len(refusals) != 1 || refusals[0].Reason != RefusedCredentialUnavailable {
		t.Fatalf("refusals = %+v, want one %q", refusals, RefusedCredentialUnavailable)
	}
}

// A seam that succeeds but yields an EMPTY credential is the same failure
// wearing a success's clothes: Header() would be "", so the upstream would be
// reached with no identity at all. Refuse.
func TestProxyEmptyCredentialRefuses(t *testing.T) {
	addr, seen := authUpstream(t, "never reached")
	src := &fakeCredentialSource{fn: func(loopauth.Principal, toolidentity.Target) (toolidentity.Credential, error) {
		return toolidentity.NewCredential("Bearer", "", true), nil
	}}

	rec := &recordingHooks{}
	p := newTestProxy(t, WithProxyHooks(rec.hooks()), WithProxyCredentialSource(src))
	sock, err := p.Listen(principal("acme", "alice"), allowHostPortCredential(t, addr, "internal-api"))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	resp, _ := proxyGet(t, sock, "http://"+addr+"/")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}

	if got := seen(); len(got) != 0 {
		t.Errorf("upstream served %d requests, want none", len(got))
	}
	refusals := rec.snapshot()
	if len(refusals) != 1 || refusals[0].Reason != RefusedCredentialUnavailable {
		t.Fatalf("refusals = %+v, want one %q", refusals, RefusedCredentialUnavailable)
	}
}

// The token is reachable ONLY through Header. This is asserted here, at the
// consumer, because the proxy is the code that must never log, wrap, or format
// a credential — and a regression in the redaction would be silent otherwise.
func TestProxyCredentialRedactsItself(t *testing.T) {
	const secret = "s3cr3t-delegation-token"
	cred := toolidentity.NewCredential("Bearer", secret, true)

	for _, rendered := range []string{
		fmt.Sprintf("%v", cred),
		fmt.Sprintf("%s", cred),
		fmt.Sprintf("%+v", cred),
		fmt.Sprintf("%#v", cred),
		fmt.Sprint(cred),
		fmt.Sprintf("%v", fmt.Errorf("wrapped: %v", cred)),
	} {
		if strings.Contains(rendered, secret) {
			t.Errorf("rendering %q leaked the token", rendered)
		}
		if !strings.Contains(rendered, "<redacted>") {
			t.Errorf("rendering %q is not marked redacted", rendered)
		}
	}

	if got := cred.Header(); got != "Bearer "+secret {
		t.Errorf("Header() = %q, want the token via the only accessor that exposes it", got)
	}
}

// ---------------------------------------------------------------------------
// NON-HTTP BYTES: the fail-closed claim, and its counterweight.
//
// The three tests below pin one claim from three sides, because the dangerous
// refactor is a plausible-looking one: "the proxy can't do https to a
// `credential:` destination — let's just raw-copy the bytes through instead."
// That edit reaches a delegated-identity destination WITHOUT the identity, and
// every other test in this file drives well-formed plaintext HTTP, so none of
// them would notice.
//
//   - a CONNECT to a `credential:` destination sends NOTHING upstream, even when
//     the client goes on writing a ClientHello at it anyway;
//   - non-HTTP bytes arriving on the FORWARD path reach no upstream either, and
//     mint no credential;
//   - and a CONNECT to a destination declared WITHOUT a credential still splices
//     raw bytes in both directions, so "make it all refuse" is not a way to pass
//     the first two.
// ---------------------------------------------------------------------------

// tlsClientHelloPrefix is the opening of a real TLS 1.x handshake: content type
// 0x16 (handshake), version 0x0301, record length, then handshake type 0x01
// (client_hello). These are the bytes a client puts on the wire the instant a
// CONNECT tunnel is established for an `https://` destination.
//
// The trailing 0x0a is not decoration and not part of TLS: net/http reads a
// request LINE, so a payload with no newline in it would park in
// textproto.ReadLine until proxyHeaderTimeout (30s) rather than failing to
// parse. A genuine ClientHello is hundreds of bytes of key material and very
// nearly always contains one; supplying it explicitly makes the parse fail
// promptly and deterministically instead of resting on random bytes. What is
// under test is where the bytes GO, and that is unaffected.
var tlsClientHelloPrefix = []byte{0x16, 0x03, 0x01, 0x02, 0x00, 0x01, 0x00, 0x01, 0xfc, 0x03, 0x03, 0x0a}

// rawUpstream is a plain TCP server that RECORDS every byte sent to it and
// echoes each one straight back.
//
// It is deliberately not an HTTP server: the claim being tested is about bytes
// that are not HTTP, so an upstream that only counts requests could not tell
// "nothing arrived" from "something arrived that I could not parse". The echo
// is what lets the same helper also prove the ALLOWED tunnel still carries
// arbitrary bytes in both directions.
func rawUpstream(t *testing.T) (addr string, received func() []byte) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	var mu sync.Mutex
	var got []byte
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						mu.Lock()
						got = append(got, buf[:n]...)
						mu.Unlock()
						if _, werr := c.Write(buf[:n]); werr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}(c)
		}
	}()
	return ln.Addr().String(), func() []byte {
		mu.Lock()
		defer mu.Unlock()
		return append([]byte(nil), got...)
	}
}

// drainUntilClosed reads from conn until the peer closes it, reporting what it
// read and whether the close actually happened.
//
// It does NOT fail the test itself, and that is deliberate: a refactor that
// tunnels these bytes leaves the connection open AND delivers them upstream, and
// the upstream assertion is the more informative of the two. So this returns a
// flag and lets each caller assert the byte-level claim first. The deadline
// bounds a wrongly-open connection to a second instead of hanging the suite.
func drainUntilClosed(t *testing.T, conn net.Conn) (data []byte, closed bool) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		data = append(data, buf[:n]...)
		if err == nil {
			continue
		}
		return data, !errors.Is(err, os.ErrDeadlineExceeded)
	}
}

// A CONNECT to a `credential:` destination forwards NOTHING, even when the
// client ignores the refusal and writes a TLS ClientHello at the socket anyway.
//
// THIS IS THE MOST CONSEQUENTIAL SAFETY CLAIM ON THE IDENTITY PATH. The refusal
// itself is asserted elsewhere (TestProxyRefusesConnectToCredentialedDestination
// checks the status and the bounded reason); what is asserted HERE is the part a
// status code cannot show — that the upstream received ZERO BYTES and the client
// connection was closed rather than left spliced. A refactor that answered the
// CONNECT with a raw copy would satisfy every status assertion in the suite while
// delivering the model's traffic to a delegated-identity destination with no
// delegated identity on it.
func TestProxyConnectToCredentialedDestinationForwardsNoBytes(t *testing.T) {
	addr, received := rawUpstream(t)
	src := &fakeCredentialSource{fn: func(loopauth.Principal, toolidentity.Target) (toolidentity.Credential, error) {
		return toolidentity.NewCredential("Bearer", "must-never-be-minted", true), nil
	}}

	rec := &recordingHooks{}
	p := newTestProxy(t, WithProxyHooks(rec.hooks()), WithProxyCredentialSource(src))
	sock, err := p.Listen(principal("acme", "alice"), allowHostPortCredential(t, addr, "internal-api"))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial %s: %v", sock, err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", addr, addr); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	// The client behaves as a real one does: having asked for a tunnel, it starts
	// the TLS handshake without waiting to be told it has one. The write itself
	// may well succeed into a local socket buffer — that is not the assertion.
	// Where those bytes END UP is.
	_, _ = conn.Write(tlsClientHelloPrefix)

	answer, closed := drainUntilClosed(t, conn)
	if got := received(); len(got) != 0 {
		t.Fatalf("upstream received %d bytes (%q) through a refused credentialed CONNECT; it must receive none", len(got), got)
	}
	if !closed {
		t.Errorf("client connection was left open after the refusal; read %q", answer)
	}
	if !strings.HasPrefix(string(answer), "HTTP/1.1 403") {
		t.Errorf("proxy answered %q, want a 403 refusal", answer)
	}
	if calls := src.snapshot(); len(calls) != 0 {
		t.Errorf("credential source was consulted %d times for a refused tunnel: %+v", len(calls), calls)
	}
	refusals := rec.snapshot()
	if len(refusals) != 1 || refusals[0].Reason != RefusedCredentialTunnel {
		t.Fatalf("refusals = %+v, want one %q", refusals, RefusedCredentialTunnel)
	}
}

// Non-HTTP bytes sent straight at the proxy — no CONNECT first — reach no
// upstream and mint no credential.
//
// This is the same claim on the FORWARD path: a client (or a `curl` against a
// misconfigured HTTPS_PROXY) that opens the socket and immediately speaks TLS
// names no destination the allowlist can be consulted about. There is nothing to
// authorize, so nothing is dialled — not the declared destination that happens
// to be sitting in this principal's allowlist, and not anything else.
func TestProxyNonHTTPBytesOnForwardPathReachNoUpstream(t *testing.T) {
	addr, received := rawUpstream(t)

	src := &fakeCredentialSource{fn: func(loopauth.Principal, toolidentity.Target) (toolidentity.Credential, error) {
		return toolidentity.NewCredential("Bearer", "must-never-be-minted", true), nil
	}}
	rec := &recordingHooks{}
	p := newTestProxy(t, WithProxyHooks(rec.hooks()), WithProxyCredentialSource(src))
	// The destination IS declared, and declared under an identity: if these bytes
	// were ever going to be handed somewhere by a fallback, this is where they
	// would go, and it is the worst place for them to go.
	sock, err := p.Listen(principal("acme", "alice"), allowHostPortCredential(t, addr, "internal-api"))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial %s: %v", sock, err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write(tlsClientHelloPrefix); err != nil {
		t.Fatalf("write ClientHello: %v", err)
	}

	answer, closed := drainUntilClosed(t, conn)
	if got := received(); len(got) != 0 {
		t.Fatalf("upstream received %d bytes (%q) from a client that never sent an HTTP request", len(got), got)
	}
	if !closed {
		t.Errorf("client connection was left open after the refusal; read %q", answer)
	}
	if !strings.HasPrefix(string(answer), "HTTP/1.1 4") {
		t.Errorf("proxy answered %q, want a 4xx refusal", answer)
	}
	if calls := src.snapshot(); len(calls) != 0 {
		t.Errorf("credential source was consulted %d times for unparseable bytes: %+v", len(calls), calls)
	}
	refusals := rec.snapshot()
	if len(refusals) != 1 || refusals[0].Reason != RefusedMalformedTarget {
		t.Fatalf("refusals = %+v, want one %q", refusals, RefusedMalformedTarget)
	}
	// A refusal names no destination, because the request never produced one.
	if refusals[0].Host != "" || refusals[0].Port != 0 {
		t.Errorf("refusal %+v names a destination the client never asked for", refusals[0])
	}
}

// THE COUNTERWEIGHT. A CONNECT to a destination declared WITHOUT a credential
// still carries arbitrary, non-HTTP bytes in BOTH directions.
//
// The two tests above are satisfied by a proxy that refuses everything, and a
// careless hardening edit is exactly how a proxy becomes that. The tunnel is the
// only way an `https://` destination works at all, so this pins that the allowed
// path is a real raw splice and not an HTTP relay that happens to have worked in
// tests that only ever spoke HTTP through it.
func TestProxyTunnelToPlainDestinationSplicesRawBytes(t *testing.T) {
	addr, received := rawUpstream(t)

	rec := &recordingHooks{}
	p := newTestProxy(t, WithProxyHooks(rec.hooks()))
	sock, err := p.Listen(principal("acme", "alice"), allowHostPort(t, addr))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	conn, br, resp := connectVia(t, sock, addr)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a plain declared destination must still tunnel", resp.StatusCode)
	}
	_ = resp.Body.Close()

	if _, err := conn.Write(tlsClientHelloPrefix); err != nil {
		t.Fatalf("write ClientHello: %v", err)
	}
	// Read back through br, not conn: a byte the proxy already buffered would
	// otherwise be invisible here.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	echo := make([]byte, len(tlsClientHelloPrefix))
	if _, err := io.ReadFull(br, echo); err != nil {
		t.Fatalf("read echo through tunnel: %v", err)
	}
	if !bytes.Equal(echo, tlsClientHelloPrefix) {
		t.Errorf("echo = %x, want %x", echo, tlsClientHelloPrefix)
	}
	if got := received(); !bytes.Equal(got, tlsClientHelloPrefix) {
		t.Errorf("upstream received %x, want %x", got, tlsClientHelloPrefix)
	}
	if refusals := rec.snapshot(); len(refusals) != 0 {
		t.Errorf("refusals = %+v, want none", refusals)
	}
}
