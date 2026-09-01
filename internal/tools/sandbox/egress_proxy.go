package sandbox

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/ethanhinson/fuse/internal/loopauth"
	"github.com/ethanhinson/fuse/internal/permissions/reputation"
)

// ErrProxyClosed is returned by Listen after Close. A closed Proxy never opens
// another socket; it does not silently re-open.
var ErrProxyClosed = errors.New("sandbox: egress proxy is closed")

// Proxy is the HOST-SIDE egress proxy for change 0064: the single hole in the
// `--network none` floor, and the only place a contained command's traffic can
// leave the box.
//
// # The principal is the LISTENER, never the request
//
// Proxy does not have one socket that many principals share. It has ONE UNIX
// SOCKET PER PRINCIPAL, each in its own 0700 directory with an unguessable
// random path component, all under a 0700 fuse-owned root. The socket path IS
// the identity: a connection accepted on principal P's listener is served under
// P's policy because of WHICH FILE it arrived through, and nothing a client
// sends — no header, no request target, no authentication material — takes part
// in that decision. The invariant is structural rather than parsed, which is
// what makes it hold under concurrency.
//
// That shape is deliberate and load-bearing. The repo's recorded learning
// `shared-server-broadcast-needs-per-session-routing` is exactly this defect
// class: a shared server holding a single "current" peer serves the wrong one
// as soon as two are live at once, and a sequential switch between them cannot
// detect it. So the policy lives on the per-principal listener, is fixed when
// that listener is created, and is never reassigned. The regression test drives
// two principals CONCURRENTLY, with different allowlists, and asserts each sees
// only its own.
//
// # What it speaks
//
// HTTP CONNECT and nothing else (plan Q2). CONNECT is what the injected
// HTTP_PROXY/HTTPS_PROXY env vars mean to curl/git/pip, and it names the
// destination host and port IN THE CLEAR before any byte flows — which is what
// lets the allowlist be consulted before the connection exists. A non-CONNECT
// method is refused with 405 rather than being served as an ordinary forward
// proxy: a second request shape would be a second way to reach the network, and
// the whole point is that there is exactly one.
//
// Raw TCP (psql and friends) is a named follow-on, not built here.
//
// # Failure direction
//
// Every unrecognized shape denies. An unreadable request, a malformed target,
// an undeclared destination, and an entry whose declared identity cannot be
// supplied all end the same way: the connection is refused and closed. The
// destination is recorded for the OPERATOR through the refusal hook; the client
// is told only that policy denied it, because the client is the model's command
// and a detailed refusal is a probe oracle.
type Proxy struct {
	// root is the fuse-owned 0700 directory holding every per-principal socket
	// directory. The Proxy OWNS it: Close removes it.
	root string

	hooks  ProxyHooks
	dialer *net.Dialer

	mu        sync.Mutex
	closed    bool
	listeners map[string]*principalListener

	// wg tracks the accept loops so Close can be synchronous.
	wg sync.WaitGroup
}

// RefusalReason is the bounded reason a connection was refused. It is a closed
// enum so it is safe as an event or metric label, and so a refusal can be
// counted without carrying the destination into a dimension.
type RefusalReason string

const (
	// RefusedNonConnect means the client spoke a method other than CONNECT.
	RefusedNonConnect RefusalReason = "non_connect"
	// RefusedMalformedTarget means the CONNECT target was not a usable
	// host:port — no port, a non-numeric port, a port outside 1..65535, or an
	// empty host. A missing port is never defaulted: the port is exact and
	// required, so guessing one would authorize against an entry the operator
	// did not write.
	RefusedMalformedTarget RefusalReason = "malformed_target"
	// RefusedNotDeclared means the destination is not in this principal's
	// allowlist. This is the ordinary denial, and the one the deny-all floor
	// produces for every destination.
	RefusedNotDeclared RefusalReason = "not_declared"
	// RefusedCredentialUnavailable means the matched entry declares a #52
	// credential audience that could not be supplied. A declared-identity entry
	// is never downgraded to an unauthenticated allow-through.
	RefusedCredentialUnavailable RefusalReason = "credential_unavailable"
	// RefusedUpstreamUnreachable means the destination WAS declared but could
	// not be dialled. It is distinguished from a policy denial so an operator
	// can tell a misconfiguration from a network fault.
	RefusedUpstreamUnreachable RefusalReason = "upstream_unreachable"
)

// RefusalInfo describes one refused connection for the emission seam.
//
// Principal is the LISTENER's principal — the only principal there is. Host is
// the CANONICAL destination host (empty when the request never produced one)
// and Port its port. This is what the operator sees; the client sees a generic
// denial.
type RefusalInfo struct {
	Principal loopauth.Principal
	Host      string
	Port      int
	Reason    RefusalReason
}

// ProxyHooks is the internal observer seam, mirroring PoolHooks: this package
// stays a LEAF with respect to emission, reporting what happened in bounded
// terms and leaving the decision to turn that into events to the composition
// root.
//
// Hooks are invoked from the connection's own goroutine with no proxy lock
// held. They must not block indefinitely: a hook that hangs stalls one
// connection's teardown.
type ProxyHooks struct {
	// Refused fires for every connection the proxy did not tunnel.
	Refused func(RefusalInfo)
}

// proxyOption configures a Proxy at construction.
type proxyOption func(*Proxy)

// withProxyRoot supplies the directory the per-principal socket directories are
// created under, TRANSFERRING OWNERSHIP of it to the Proxy: it is chmodded to
// 0700 at construction and removed by Close. When unset, NewProxy creates one
// under os.TempDir().
//
// Callers supply one mainly to keep the path SHORT: a UNIX socket path is
// capped by the kernel (104 bytes of sun_path on darwin, 108 on Linux), and
// that budget is spent by the root, the random component, and the socket name
// together.
func withProxyRoot(dir string) proxyOption {
	return func(p *Proxy) {
		if dir != "" {
			p.root = dir
		}
	}
}

// withProxyHooks installs the observer seam.
func withProxyHooks(h ProxyHooks) proxyOption {
	return func(p *Proxy) { p.hooks = h }
}

const (
	// proxyDialTimeout bounds the upstream dial for a DECLARED destination, so
	// a black-holed host cannot pin a connection goroutine forever.
	proxyDialTimeout = 15 * time.Second

	// proxyHeaderTimeout bounds how long a connected client may take to send
	// its request line and headers. It applies only to the request; once a
	// tunnel is established the deadline is cleared, because a legitimate
	// tunnel is long-lived by design.
	proxyHeaderTimeout = 30 * time.Second

	// maxSocketPathLen is the portable floor for sun_path (104 on darwin,
	// including the terminator). Exceeding it fails inside bind with an
	// "invalid argument" that names nothing; checking here produces an error
	// that says what actually went wrong.
	maxSocketPathLen = 103

	// proxySocketName is the socket's leaf name. The unguessable component is
	// the DIRECTORY above it, so the leaf can stay short and predictable.
	proxySocketName = "egress.sock"
)

// NewProxy creates the fuse-owned root directory and returns an empty Proxy.
// No socket exists until a principal is registered with Listen.
func NewProxy(opts ...proxyOption) (*Proxy, error) {
	p := &Proxy{
		dialer:    &net.Dialer{Timeout: proxyDialTimeout},
		listeners: make(map[string]*principalListener),
	}
	for _, opt := range opts {
		opt(p)
	}

	if p.root == "" {
		dir, err := os.MkdirTemp("", "fuse-egress")
		if err != nil {
			return nil, fmt.Errorf("sandbox: create egress proxy root: %w", err)
		}
		p.root = dir
	} else if err := os.MkdirAll(p.root, 0o700); err != nil {
		return nil, fmt.Errorf("sandbox: create egress proxy root: %w", err)
	}
	// Asserted rather than assumed: an inherited directory may be group- or
	// world-readable, and the socket's containment rests on the directory
	// permissions as much as on the unguessable name.
	if err := os.Chmod(p.root, 0o700); err != nil {
		return nil, fmt.Errorf("sandbox: secure egress proxy root: %w", err)
	}
	return p, nil
}

// Root is the fuse-owned directory every per-principal socket lives under.
func (p *Proxy) Root() string { return p.root }

// Listen registers a principal and returns the path of the UNIX socket its
// containers reach the network through.
//
// The returned path is the principal's identity for the life of the listener.
// It is idempotent: a principal that already has a listener gets the same path
// back and KEEPS ITS EXISTING POLICY, because several concurrent sandboxes for
// one principal share one socket and the policy a live tunnel is being served
// under must not change underneath it. Policy is process-global configuration
// (Config.Egress), so this cannot silently drop a different policy in practice;
// the per-principal parameter exists so that policy is resolved from the
// PRINCIPAL rather than from a proxy-wide "current" value.
//
// A policy whose mode is not EgressEnforce denies everything, since the proxy
// only ever consults Allow and the loader leaves it nil outside enforcement.
// There is no path here that resolves to "allow anything".
func (p *Proxy) Listen(principal loopauth.Principal, policy Egress) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return "", ErrProxyClosed
	}
	key := principalKey(principal)
	if pl, ok := p.listeners[key]; ok {
		return pl.path, nil
	}

	dir, err := p.principalDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, proxySocketName)
	if len(path) > maxSocketPathLen {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("sandbox: egress socket path %q is %d bytes, over the %d-byte limit; configure a shorter proxy root", path, len(path), maxSocketPathLen)
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("sandbox: listen on egress socket: %w", err)
	}
	// The socket is reachable only through its 0700 directory, but its own mode
	// is tightened too: the container gets the socket bind-mounted, and nothing
	// else on the host has any business connecting to it.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("sandbox: secure egress socket: %w", err)
	}

	pl := &principalListener{
		proxy:     p,
		principal: principal,
		policy:    policy,
		dir:       dir,
		path:      path,
		ln:        ln,
		conns:     make(map[io.Closer]struct{}),
	}
	p.listeners[key] = pl

	p.wg.Add(1)
	go pl.serve()

	return path, nil
}

// Release closes one principal's listener and removes its socket. It is called
// when that principal's sandbox usage ends; a principal with no listener is not
// an error, so teardown paths can call it unconditionally.
func (p *Proxy) Release(principal loopauth.Principal) error {
	p.mu.Lock()
	key := principalKey(principal)
	pl := p.listeners[key]
	delete(p.listeners, key)
	p.mu.Unlock()

	if pl == nil {
		return nil
	}
	return pl.close()
}

// Close tears down every listener, closes every connection still in flight,
// removes every socket, and removes the fuse-owned root. It is idempotent.
//
// Teardown is deliberately total rather than graceful: a socket left behind
// after the proxy that served it is gone is a path into a policy nobody is
// enforcing any more.
func (p *Proxy) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	listeners := make([]*principalListener, 0, len(p.listeners))
	for _, pl := range p.listeners {
		listeners = append(listeners, pl)
	}
	p.listeners = nil
	p.mu.Unlock()

	var firstErr error
	for _, pl := range listeners {
		if err := pl.close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	p.wg.Wait()

	if err := os.RemoveAll(p.root); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// principalDir creates one unguessable 0700 directory under the root.
//
// The random component is what stops a container that holds one principal's
// mounted socket from NAMING another's: the path is not derived from the
// principal, so it cannot be computed from a tenant or a subject that leaked.
func (p *Proxy) principalDir() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("sandbox: generate egress socket path: %w", err)
	}
	dir := filepath.Join(p.root, hex.EncodeToString(raw[:]))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", fmt.Errorf("sandbox: create egress socket dir: %w", err)
	}
	return dir, nil
}

// principalKey is the map identity of a principal: its tenant and subject, and
// nothing else. The NUL separator makes the two fields unambiguous, so no pair
// of (tenant, subject) values can collide with another pair.
func principalKey(p loopauth.Principal) string {
	return string(p.Tenant) + "\x00" + p.Subject
}

// principalListener is ONE principal's socket, accept loop, and policy.
//
// policy is written once, at construction, and only ever read afterwards. That
// is the concurrency story in full: no shared mutable policy exists, so two
// principals served at the same instant cannot observe each other's.
type principalListener struct {
	proxy     *Proxy
	principal loopauth.Principal
	policy    Egress
	dir       string
	path      string
	ln        net.Listener

	mu     sync.Mutex
	closed bool
	// conns holds every client and upstream connection currently open on this
	// listener, so teardown can close them rather than waiting on a tunnel that
	// may be idle for hours.
	conns map[io.Closer]struct{}
	wg    sync.WaitGroup
}

func (pl *principalListener) serve() {
	defer pl.proxy.wg.Done()
	for {
		conn, err := pl.ln.Accept()
		if err != nil {
			// The only expected error is the listener being closed. Any other
			// is equally terminal for this socket: there is nothing to retry
			// against, and spinning on a broken listener would burn a core.
			return
		}
		if !pl.track(conn) {
			_ = conn.Close()
			return
		}
		go func() {
			defer pl.wg.Done()
			defer pl.untrack(conn)
			defer func() { _ = conn.Close() }()
			pl.handle(conn)
		}()
	}
}

// track registers c for teardown and accounts for its goroutine. It reports
// false once the listener is closed, so a connection accepted in the race with
// close is dropped rather than served under a policy that is going away.
func (pl *principalListener) track(c io.Closer) bool {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if pl.closed {
		return false
	}
	pl.conns[c] = struct{}{}
	pl.wg.Add(1)
	return true
}

// trackUpstream registers an upstream connection for teardown WITHOUT taking a
// goroutine reference: the client connection's goroutine already accounts for
// the tunnel.
func (pl *principalListener) trackUpstream(c io.Closer) bool {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if pl.closed {
		return false
	}
	pl.conns[c] = struct{}{}
	return true
}

func (pl *principalListener) untrack(c io.Closer) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if pl.conns != nil {
		delete(pl.conns, c)
	}
}

func (pl *principalListener) close() error {
	pl.mu.Lock()
	if pl.closed {
		pl.mu.Unlock()
		return nil
	}
	pl.closed = true
	conns := make([]io.Closer, 0, len(pl.conns))
	for c := range pl.conns {
		conns = append(conns, c)
	}
	pl.conns = nil
	pl.mu.Unlock()

	err := pl.ln.Close()
	for _, c := range conns {
		_ = c.Close()
	}
	pl.wg.Wait()

	// Go unlinks a socket it created on Close; removing the directory removes
	// it again if that ever stops being true, and takes the unguessable path
	// component out of the filesystem with it.
	if rmErr := os.RemoveAll(pl.dir); rmErr != nil && err == nil {
		err = rmErr
	}
	return err
}

// handle serves exactly one client connection: read one request, decide, and
// either tunnel it or refuse it.
func (pl *principalListener) handle(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(proxyHeaderTimeout))
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		// A client that connected and went away without speaking is not a
		// refusal and is not worth recording. Anything else got as far as
		// sending bytes that could not be read as a request — including a
		// CONNECT target malformed enough that net/http rejects it before this
		// code sees it (a non-numeric port, say) — and that IS a refusal the
		// operator should see, even though there is no destination to name.
		if !errors.Is(err, io.EOF) {
			pl.refuse(conn, http.StatusBadRequest, RefusalInfo{
				Principal: pl.principal,
				Reason:    RefusedMalformedTarget,
			}, "malformed request\n")
		}
		return
	}
	// The request body is never forwarded: CONNECT has none, and a non-CONNECT
	// request is refused rather than proxied.
	defer func() { _ = req.Body.Close() }()

	if req.Method != http.MethodConnect {
		pl.refuse(conn, http.StatusMethodNotAllowed, RefusalInfo{
			Principal: pl.principal,
			Reason:    RefusedNonConnect,
		}, "this proxy speaks HTTP CONNECT only\n")
		return
	}

	target := req.URL.Host
	if target == "" {
		target = req.Host
	}
	host, port, ok := splitConnectTarget(target)
	if !ok {
		pl.refuse(conn, http.StatusBadRequest, RefusalInfo{
			Principal: pl.principal,
			Host:      host,
			Port:      port,
			Reason:    RefusedMalformedTarget,
		}, "malformed CONNECT target\n")
		return
	}

	// THE MATCH. The host is canonicalized exactly once, here, at the entry
	// point, and the already-canonical value is what is matched, dialled, and
	// reported (ADR-0048 rule 3). Do not add a second normalization inside
	// Match: two normalizers that drift apart is the live bug that ADR records.
	entry, allowed := pl.policy.Match(host, port)
	if !allowed {
		pl.refuse(conn, http.StatusForbidden, RefusalInfo{
			Principal: pl.principal,
			Host:      host,
			Port:      port,
			Reason:    RefusedNotDeclared,
		}, egressDenialBody)
		return
	}

	// TODO(#0064 task 5): resolve entry.Credential through the #52
	// CredentialSource seam using pl.principal — the LISTENER's principal — and
	// bind it on the upstream connection.
	//
	// Until that source is wired, an entry that declares a credential audience
	// is REFUSED. It is not downgraded to a plain allow-through: the operator
	// asked for that destination to be reached under a delegated identity, and
	// reaching it without one is the fail-open shape this change exists to
	// prevent.
	if entry.Credential != "" {
		pl.refuse(conn, http.StatusForbidden, RefusalInfo{
			Principal: pl.principal,
			Host:      host,
			Port:      port,
			Reason:    RefusedCredentialUnavailable,
		}, egressDenialBody)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), pl.proxy.dialer.Timeout)
	defer cancel()
	upstream, err := pl.proxy.dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		pl.refuse(conn, http.StatusBadGateway, RefusalInfo{
			Principal: pl.principal,
			Host:      host,
			Port:      port,
			Reason:    RefusedUpstreamUnreachable,
		}, "upstream unavailable\n")
		return
	}
	if !pl.trackUpstream(upstream) {
		_ = upstream.Close()
		return
	}
	defer pl.untrack(upstream)
	defer func() { _ = upstream.Close() }()

	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	// The tunnel is long-lived by design, so the request-header deadline is
	// cleared. Teardown is by connection close (Release/Close), not by timeout.
	_ = conn.SetReadDeadline(time.Time{})

	// br, not conn, is the client-side reader: a client that pipelined bytes
	// after its CONNECT already has them sitting in the buffer, and reading
	// from the raw connection would silently drop them.
	splice(conn, br, upstream)
}

// egressDenialBody is what a REFUSED client is told: that policy denied it, and
// nothing else. The destination goes to the operator through the refusal hook.
// Echoing it back would turn every refusal into a confirmation that the proxy
// saw the request, which is a probe oracle for the model's command.
const egressDenialBody = "egress denied by policy\n"

// refuse reports the refusal to the operator and writes the generic denial to
// the client. The hook fires first so a refusal is recorded even if the client
// has already gone away.
func (pl *principalListener) refuse(conn net.Conn, status int, info RefusalInfo, body string) {
	if pl.proxy.hooks.Refused != nil {
		pl.proxy.hooks.Refused(info)
	}
	_ = conn.SetWriteDeadline(time.Now().Add(proxyHeaderTimeout))
	fmt.Fprintf(conn,
		"HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, http.StatusText(status), len(body), body)
}

// splitConnectTarget parses a CONNECT request-target into a CANONICAL host and
// a port.
//
// The port is exact and required. A target with no port is refused rather than
// defaulted to 443: defaulting would authorize against an entry the operator
// did not write. A port outside 1..65535 is refused for the same reason — the
// loader will not store one, so nothing could legitimately match it.
//
// The host is canonicalized here and ONLY here, through the shared
// reputation.CanonicalHost. Everything downstream — the match, the dial, and
// the refusal record — uses this one value.
func splitConnectTarget(target string) (host string, port int, ok bool) {
	rawHost, rawPort, err := net.SplitHostPort(target)
	if err != nil {
		return "", 0, false
	}
	host = reputation.CanonicalHost(rawHost)
	if host == "" {
		return "", 0, false
	}
	port, err = strconv.Atoi(rawPort)
	if err != nil || port < 1 || port > 65535 {
		return host, 0, false
	}
	return host, port, true
}

// splice joins an established tunnel's two halves and returns when either
// direction ends.
//
// Both connections are closed as soon as the first copy finishes, which is what
// unblocks the other one: a half-closed tunnel has no reader left that could
// ever consume what the remaining half sends.
func splice(client net.Conn, clientReader io.Reader, upstream net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, clientReader)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		done <- struct{}{}
	}()

	<-done
	_ = client.Close()
	_ = upstream.Close()
	<-done
}
