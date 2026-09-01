package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

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

	p := newTestProxy(t, withCredentialSource(src))
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
	p := newTestProxy(t, withProxyHooks(rec.hooks()), withCredentialSource(src))
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
	p := newTestProxy(t, withCredentialSource(src))

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
	p := newTestProxy(t, withProxyHooks(rec.hooks()), withCredentialSource(src))
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
	p := newTestProxy(t, withProxyHooks(rec.hooks()), withCredentialSource(src))
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
