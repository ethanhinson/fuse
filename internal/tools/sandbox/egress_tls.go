package sandbox

// THE SANDBOX→PROXY TRANSPORT (change 0075, task 7; spec §5).
//
// # What this file adds, and what it deliberately does not touch
//
// The UNIX-socket path in egress_proxy.go carries the principal STRUCTURALLY:
// there is one socket per principal, in a 0700 directory with an unguessable
// name, and a connection is served under P's policy because of WHICH FILE it
// arrived through. Nothing a client sends takes part in that decision, which is
// what makes the invariant hold under concurrency.
//
// A remote sandbox cannot be given a file. A Pod in a tenant namespace reaches
// this process over the network, on ONE listener shared by every principal, so
// the structural identity is simply unavailable and something has to stand in
// for it. The substitute is a CLIENT CERTIFICATE, and the property that makes it
// an acceptable substitute is that the selector is read from the VERIFIED PEER
// CHAIN at handshake time — before a single application byte has been read — and
// never from anything the client says afterwards. No header, no SNI value, no
// request target participates. The certificate's SERIAL is the map key.
//
// The serial rather than the SAN: the SAN URI is legibility for an operator
// reading a certificate with openssl, and it is derived from data (a principal
// key) that a later edit could be tempted to parse. A serial is an opaque
// integer minted by this process's own CA, registered in this process's own map,
// and meaningful nowhere else — there is nothing in it to parse wrongly. The SAN
// is therefore informational and the serial is load-bearing, which is the
// inverse of the arrangement that invites a lookup-by-name bug.
//
// # What is NOT changed here
//
// ADR-0052's RefusedCredentialTunnel is a decision about a DIFFERENT HOP: the
// proxy will not terminate TLS toward a credentialed DESTINATION, because it
// cannot put an Authorization header into a stream it does not terminate. This
// file terminates the sandbox→proxy leg, which says nothing whatsoever about the
// proxy→destination leg. Every policy decision, every refusal reason, and both
// connection ceilings are the socket path's, reached through the SAME
// principalListener object: a TLS connection is handed to pl.handle exactly as
// an accepted socket connection is, so there is one authorization implementation
// and not two.
//
// # The CA is ephemeral, per process, and never persisted
//
// An instance restart invalidates every outstanding certificate. That is correct
// rather than unfortunate: the sandboxes those certificates belong to are
// orphans the moment the instance holding their policy dies, and a credential
// that outlived its policy would be a credential nobody can authorize. The
// reaper collects the Pods; the CA dying with the process collects their
// credentials.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"sync"
	"time"

	"github.com/ethanhinson/fuse/internal/loopauth"
)

const (
	// proxyTLSServerName is the name the proxy's own leaf certificate carries and
	// the name a sidecar verifies. It is FIXED rather than derived from the
	// advertise address because the advertise address is an IP (spec §5: a Pod IP,
	// never a Service ClusterIP), and pinning a name lets the sidecar verify the
	// certificate without the proxy having to re-mint a leaf every time its Pod IP
	// changes. The IP is how you reach it; this is what proves what you reached.
	proxyTLSServerName = "fuse-egress-proxy"

	// proxyCALifetime bounds the ephemeral CA. It is far longer than any process
	// is expected to live, because the CA's real lifetime is the process's — the
	// expiry exists so a certificate that somehow escaped the process is not
	// valid forever, not as an operational rotation schedule.
	proxyCALifetime = 30 * 24 * time.Hour

	// proxyCredentialLifetime bounds one sandbox's client certificate. It
	// comfortably exceeds a sandbox's idle TTL: a warm Pod that outlives its
	// certificate would start failing egress mid-loop for a reason no refusal
	// reason describes, and revocation (Release) is the mechanism that actually
	// ends a credential's life.
	proxyCredentialLifetime = 24 * time.Hour

	// proxyTLSHandshakeTimeout bounds one handshake. It runs on the accepted
	// connection's own goroutine, but a peer that opens a connection and never
	// speaks TLS would otherwise hold a connection slot indefinitely — and the
	// slot is the resource the ceilings exist to protect.
	proxyTLSHandshakeTimeout = 15 * time.Second
)

// ErrProxyNoTLSListener reports that Enroll was called on a Proxy with no TLS
// listener.
//
// It is a refusal rather than a lazily-opened listener: a credential handed to a
// sandbox for a datapath that does not exist makes that sandbox's sidecar dial
// nothing forever, which presents as an unexplained network hang inside the
// sandbox rather than as a configuration error the operator can read.
var ErrProxyNoTLSListener = errors.New("sandbox: egress proxy has no TLS listener; call ListenTLS first")

// CA is the in-process ephemeral certificate authority the sandbox→proxy
// transport is anchored on.
//
// It is created per fuse process, held only in memory, and NEVER persisted.
// There is no loader, no path, and no option to supply one, which is deliberate:
// a persisted CA would be a durable secret whose compromise outlives the process
// that made it, in exchange for a property (certificates surviving a restart)
// this design does not want.
type CA struct {
	// cert is the self-signed CA certificate, and pool is the same certificate as
	// a verification pool. Both are fixed at construction and only read
	// afterwards, so no lock guards them.
	cert *x509.Certificate
	pem  []byte
	key  *ecdsa.PrivateKey
	pool *x509.CertPool

	// serverCert is the proxy's own leaf, minted once at construction from this
	// CA and presented on every handshake.
	serverCert tls.Certificate
}

// NewCA mints a fresh ephemeral CA and the proxy's own server leaf.
//
// P-256 ECDSA rather than RSA: the handshake happens on every sandbox's egress
// connection setup, the keys are minted per sandbox, and neither the CA nor the
// leaves are ever shown to anything outside this process, so there is no
// interoperability constraint pushing toward RSA and a real cost to paying for
// its key generation per Enroll.
func NewCA() (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("sandbox: generate egress CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "fuse egress proxy CA"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(proxyCALifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		// A CA that can sign an intermediate is a CA that can be persuaded to
		// delegate. There is exactly one issuer in this design and it signs only
		// leaves.
		MaxPathLen:     0,
		MaxPathLenZero: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("sandbox: create egress CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("sandbox: parse egress CA certificate: %w", err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	ca := &CA{
		cert: cert,
		pem:  caPEM,
		key:  key,
		pool: x509.NewCertPool(),
	}
	ca.pool.AddCert(cert)

	srv, err := ca.issueServer()
	if err != nil {
		return nil, err
	}
	ca.serverCert = srv
	return ca, nil
}

// CAPEM is the PEM the sidecar verifies the proxy against. It is public
// material: the whole point of handing it to a sandbox is that the sandbox can
// tell the real proxy from anything else that answers on that address.
func (c *CA) CAPEM() []byte {
	out := make([]byte, len(c.pem))
	copy(out, c.pem)
	return out
}

// issueServer mints the proxy's own leaf, valid for proxyTLSServerName and for
// loopback, so a test (and a laptop dev loop) can reach it by address while a
// Pod reaches it by the pinned name.
func (c *CA) issueServer() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("sandbox: generate egress server key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: proxyTLSServerName},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(proxyCALifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{proxyTLSServerName},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("sandbox: create egress server certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("sandbox: parse egress server certificate: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// ClientCredential is everything a sandbox's forwarder needs to reach the proxy,
// and nothing more.
//
// It carries PEM bytes rather than parsed objects because its destination is a
// Kubernetes Secret mounted into a sidecar: the consumer is a file path and an
// `openssl`-shaped CLI, not Go code. Serial is returned so the caller can name
// the credential in a log without carrying the certificate around; it is NOT a
// bearer token — presenting a serial proves nothing without the key.
type ClientCredential struct {
	// Serial is the credential's identity in the proxy's registry, lowercase hex.
	Serial string
	// CertPEM is the client certificate the sidecar presents.
	CertPEM []byte
	// KeyPEM is that certificate's private key, PKCS#8. It is the only secret
	// here, and it never leaves the Secret the sidecar mounts.
	KeyPEM []byte
	// CAPEM is the CA the sidecar verifies the PROXY against — the other
	// direction of the mutual authentication.
	CAPEM []byte
}

// issue mints one client certificate for p with the given nonce in its SAN URI.
//
// The SAN URI is `fuse://sandbox/<principalKey>/<nonce>` per spec §5, where the
// principal key is HASHED. A certificate is handed to a sandbox, so spelling a
// raw tenant id in it would disclose one tenant's identity to whoever can read
// the Secret — and the value is legibility only, since the serial is what
// selects the policy.
func (c *CA) issue(p loopauth.Principal, nonce string) (ClientCredential, error) {
	serial, err := randomSerial()
	if err != nil {
		return ClientCredential{}, err
	}
	return c.issueWithSerial(p, nonce, serial)
}

// issueWithSerial is issue with the serial supplied rather than drawn.
//
// It exists so a test can mint, from a FOREIGN CA, a certificate carrying a
// serial this proxy has registered — the one attack the serial-keyed registry is
// exposed to, and one that cannot be staged if the serial is always random. No
// production path calls it: issue draws its own.
func (c *CA) issueWithSerial(p loopauth.Principal, nonce string, serial *big.Int) (ClientCredential, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return ClientCredential{}, fmt.Errorf("sandbox: generate egress client key: %w", err)
	}
	san := &url.URL{
		Scheme: "fuse",
		Host:   "sandbox",
		Path:   "/" + principalDigestKey(p) + "/" + nonce,
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		// The subject is deliberately uninformative. Identity lives in the
		// registry keyed by serial, not in a field a later edit could parse.
		Subject:     pkix.Name{CommonName: "fuse sandbox egress client"},
		NotBefore:   now.Add(-time.Minute),
		NotAfter:    now.Add(proxyCredentialLifetime),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:        []*url.URL{san},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return ClientCredential{}, fmt.Errorf("sandbox: create egress client certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return ClientCredential{}, fmt.Errorf("sandbox: marshal egress client key: %w", err)
	}
	return ClientCredential{
		Serial:  serialKey(serial),
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		CAPEM:   c.CAPEM(),
	}, nil
}

// randomSerial draws a positive 128-bit serial.
//
// Random rather than a counter: a counter is state, and state shared between the
// issuing path and the registry is one more thing that can disagree. 128 bits of
// randomness makes a collision within one process's lifetime not worth
// modelling, and a collision would be detected anyway — registerSerial refuses to
// overwrite an existing entry.
func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	for range 8 {
		n, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return nil, fmt.Errorf("sandbox: generate certificate serial: %w", err)
		}
		if n.Sign() > 0 {
			return n, nil
		}
	}
	return nil, errors.New("sandbox: could not generate a positive certificate serial")
}

// serialKey is a serial's registry key: lowercase hex of the integer, which is
// how big.Int.Text(16) spells it and therefore how the verified peer chain's
// own serial will spell it too.
func serialKey(n *big.Int) string { return n.Text(16) }

// principalDigestKey is a short, non-reversible stand-in for a principal, used
// only inside a SAN URI for operator legibility.
func principalDigestKey(p loopauth.Principal) string {
	sum := sha256.Sum256([]byte(principalKey(p)))
	return hex.EncodeToString(sum[:8])
}

// tlsListener is the Proxy's one TLS listener and its serial registry.
//
// The registry maps a certificate serial onto the principalListener that holds
// the policy — the SAME object the UNIX socket path serves through — so there is
// one policy per principal and not one per transport. An entry's absence is the
// revocation: Release deletes every serial belonging to the principal.
type tlsListener struct {
	ln   net.Listener
	ca   *CA
	addr string

	mu sync.Mutex
	// serials maps serial → the principal key whose listener holds the policy.
	// It is a key and not a *principalListener pointer so a released-and-
	// re-listened principal cannot be reached through a stale pointer: the
	// lookup goes through p.listeners every time.
	serials map[string]string
}

// ListenTLS opens the sandbox→proxy TLS listener beside the UNIX sockets.
//
// It may be called ONCE. A second call refuses rather than replacing or adding a
// listener: two listeners would mean two accept loops charging the same
// ceilings, and a replaced one would silently strand every sidecar already
// dialling the old address. The composition root opens it at startup or not at
// all.
//
// addr may name port 0, in which case TLSAddr reports the port the kernel
// chose — which is what the advertise address must then carry.
func (p *Proxy) ListenTLS(addr string, ca *CA) error {
	if ca == nil {
		// A listener with no trust anchor would accept any client certificate and
		// then fail to find its serial, which looks like a policy decision and is
		// not. Refuse at construction instead.
		return errors.New("sandbox: ListenTLS needs a CA; a listener with no trust anchor authenticates nothing")
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrProxyClosed
	}
	if p.tls != nil {
		p.mu.Unlock()
		return fmt.Errorf("sandbox: egress proxy already has a TLS listener on %s", p.tls.addr)
	}
	p.mu.Unlock()

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("sandbox: listen on egress TLS address %q: %w", addr, err)
	}

	tl := &tlsListener{
		ca:      ca,
		addr:    ln.Addr().String(),
		serials: make(map[string]string),
	}
	// The verified peer chain is what the serial is read from, so verification is
	// REQUIRED and the pool is the proxy's own CA alone. RequireAndVerifyClientCert
	// is the load-bearing line in this struct: with ClientAuth any weaker, a
	// self-signed certificate carrying a registered serial would be served.
	tl.ln = tls.NewListener(ln, &tls.Config{
		Certificates: []tls.Certificate{ca.serverCert},
		ClientCAs:    ca.pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		// TLS 1.3 only. Both ends of this hop are fuse's own code, so there is no
		// legacy peer to accommodate, and pinning the floor here means the
		// cipher-suite question never arises.
		MinVersion: tls.VersionTLS13,
	})

	p.mu.Lock()
	if p.closed {
		// Close ran while the listener was being opened. Do not register it — the
		// proxy's teardown has already happened and nothing would ever close this.
		p.mu.Unlock()
		_ = tl.ln.Close()
		return ErrProxyClosed
	}
	p.tls = tl
	p.mu.Unlock()

	p.wg.Add(1)
	go p.serveTLS(tl)
	return nil
}

// TLSAddr reports the TLS listener's address, or "" when there is none. It is
// the value the advertise address must resolve to, and it exists because a
// listener opened on port 0 knows its port and the caller does not.
func (p *Proxy) TLSAddr() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tls == nil {
		return ""
	}
	return p.tls.addr
}

// testCA reports the TLS listener's CA, or nil.
//
// It is unexported and exists for ONE assertion tests cannot make otherwise: that
// a certificate signed by the proxy's own CA whose serial was never registered is
// still closed. Without it a test can only forge from a foreign CA, which the
// handshake rejects for a different reason and so would not exercise the serial
// lookup at all.
func (p *Proxy) testCA() *CA {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tls == nil {
		return nil
	}
	return p.tls.ca
}

// Enroll mints a client credential for p and registers its serial against p's
// policy.
//
// Like Listen, it TAKES A LEASE on the principal's listener, and the matching
// Release drops it. That is what makes the credential's life the life of the
// sandbox that holds it: a principal with two live sandboxes has two serials and
// two leases, and one sandbox finishing revokes only its own certificate. Also
// like Listen, an existing listener KEEPS ITS EXISTING POLICY — several
// sandboxes of one principal share one policy object, and a live connection's
// policy must not change underneath it.
//
// Every enrollment gets a FRESH serial even for the same principal, so
// revocation is per-sandbox rather than per-principal. That is why the registry
// maps serial → principal and not principal → serial.
func (p *Proxy) Enroll(principal loopauth.Principal, policy Egress) (ClientCredential, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ClientCredential{}, ErrProxyClosed
	}
	tl := p.tls
	if tl == nil {
		p.mu.Unlock()
		return ClientCredential{}, ErrProxyNoTLSListener
	}

	key := principalKey(principal)
	pl, ok := p.listeners[key]
	if ok {
		pl.refs++
	} else {
		// A TLS-only principal has NO socket: it is a remote sandbox, and there is
		// no container to bind-mount a path into. The listener object exists for
		// its policy, its connection semaphore, and its teardown set.
		pl = &principalListener{
			proxy:     p,
			principal: principal,
			policy:    policy,
			refs:      1,
			conns:     make(map[io.Closer]struct{}),
			connSlots: newSemaphore(p.maxConnsPerPrincipal),
		}
		p.listeners[key] = pl
	}
	p.mu.Unlock()

	cred, err := tl.ca.issue(principal, newEnrollNonce())
	if err != nil {
		// The lease is handed back: a failed enrollment must not leave a listener
		// alive that nobody will ever release.
		_ = p.Release(principal)
		return ClientCredential{}, err
	}

	tl.mu.Lock()
	if _, clash := tl.serials[cred.Serial]; clash {
		tl.mu.Unlock()
		_ = p.Release(principal)
		return ClientCredential{}, fmt.Errorf("sandbox: certificate serial %s is already registered", cred.Serial)
	}
	tl.serials[cred.Serial] = key
	tl.mu.Unlock()

	return cred, nil
}

// newEnrollNonce is the SAN URI's per-enrollment component. It distinguishes two
// credentials of one principal in an operator's reading of a certificate; it is
// not consulted by anything.
func newEnrollNonce() string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// A nonce that could not be drawn degrades legibility and nothing else,
		// so it must not fail an enrollment.
		return "unknown"
	}
	return hex.EncodeToString(raw[:])
}

// revokeSerials drops every serial registered to a principal key. It is called
// from Release's teardown path, at the same moment the listener leaves
// p.listeners, so the revocation and the policy's disappearance are one event.
func (p *Proxy) revokeSerials(key string) {
	p.mu.Lock()
	tl := p.tls
	p.mu.Unlock()
	if tl == nil {
		return
	}
	tl.mu.Lock()
	defer tl.mu.Unlock()
	for serial, owner := range tl.serials {
		if owner == key {
			delete(tl.serials, serial)
		}
	}
}

// serveTLS accepts on the TLS listener and serves each connection under the
// policy its CERTIFICATE selects.
func (p *Proxy) serveTLS(tl *tlsListener) {
	defer p.wg.Done()
	for {
		conn, err := tl.ln.Accept()
		if err != nil {
			// As on the socket path: the only expected error is the listener
			// closing, and every other one is equally terminal for this listener.
			return
		}
		go p.serveTLSConn(tl, conn)
	}
}

// serveTLSConn completes the handshake, resolves the serial to a principal's
// listener, and hands the connection to the SAME handler the socket path uses.
//
// THE ORDER HERE IS THE SECURITY PROPERTY. The handshake completes, the serial
// is read from the verified peer chain, and the policy is resolved — all before
// any application byte is read. A connection whose serial is unknown, released,
// or expired is CLOSED with nothing written and nothing read: there is no
// principal to attribute a refusal to, and answering an unauthenticated peer
// with a bounded reason would tell it which serials exist.
func (p *Proxy) serveTLSConn(tl *tlsListener, raw net.Conn) {
	tc, ok := raw.(*tls.Conn)
	if !ok {
		_ = raw.Close()
		return
	}

	// The handshake is bounded: an accepted connection that never speaks TLS
	// would otherwise sit here forever. It has NOT taken a connection slot yet —
	// the slot is charged once the principal is known, because a slot is a
	// principal's share and there is no principal before the handshake.
	_ = tc.SetDeadline(time.Now().Add(proxyTLSHandshakeTimeout))
	if err := tc.Handshake(); err != nil {
		_ = tc.Close()
		return
	}
	_ = tc.SetDeadline(time.Time{})

	serial, ok := peerSerial(tc)
	if !ok {
		_ = tc.Close()
		return
	}

	tl.mu.Lock()
	key, known := tl.serials[serial]
	tl.mu.Unlock()
	if !known {
		// UNKNOWN OR REVOKED. Closed, silently, with zero application bytes read.
		_ = tc.Close()
		return
	}

	p.mu.Lock()
	pl := p.listeners[key]
	p.mu.Unlock()
	if pl == nil {
		// The serial was registered but the listener is gone: Release removed the
		// listener and the serial in one step, so this is the narrow race where
		// the map read landed between the two. Fail closed.
		_ = tc.Close()
		return
	}

	// From here the connection is INDISTINGUISHABLE from one accepted on the
	// principal's UNIX socket: the same ceilings, the same capacity refusals, the
	// same handler, the same allowlist decision. That is the point — there is one
	// authorization implementation, and this file only supplies a transport and
	// an identity for it.
	reason, admitted := pl.track(tc)
	if !admitted {
		if reason == "" {
			_ = tc.Close()
			return
		}
		pl.refuseAtCapacity(tc, reason)
		return
	}
	defer pl.wg.Done()
	defer pl.releaseConnSlot()
	defer pl.untrack(tc)
	defer func() { _ = tc.Close() }()
	pl.handle(tc)
}

// peerSerial reads the serial from the VERIFIED peer chain.
//
// VerifiedChains rather than PeerCertificates: PeerCertificates is what the peer
// SENT, and reading the selector from it would make an unverified certificate's
// serial select a policy. The listener is configured RequireAndVerifyClientCert,
// so a populated VerifiedChains is the proof that the CA signed what is being
// read — and an empty one is a refusal rather than a fallback.
func peerSerial(tc *tls.Conn) (string, bool) {
	st := tc.ConnectionState()
	if len(st.VerifiedChains) == 0 || len(st.VerifiedChains[0]) == 0 {
		return "", false
	}
	leaf := st.VerifiedChains[0][0]
	if leaf.SerialNumber == nil {
		return "", false
	}
	return serialKey(leaf.SerialNumber), true
}
