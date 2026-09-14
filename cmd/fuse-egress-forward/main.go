// Command fuse-egress-forward is the IN-CONTAINER half of fuse's egress control
// (change 0064).
//
// # What it is for
//
// A sandboxed bash container runs with `--network none`: loopback is up and
// there is no route off the box. The one hole fuse opens is a UNIX socket,
// bind-mounted read-only from the host, on which fuse's own egress proxy speaks
// HTTP CONNECT against the operator's allowlist. But nothing a command actually
// runs can USE a UNIX-socket proxy — curl's `--unix-socket` names the TARGET, not
// a proxy, and git/pip/go have no such notion at all. Every one of them can use
// an `http://host:port` proxy.
//
// So this program is the adapter: it listens on 127.0.0.1 inside the container
// and relays each accepted connection, byte for byte, to the mounted socket. It
// understands nothing about HTTP — the proxy on the far side does all the
// deciding — which is the point: no policy lives on the container side of the
// boundary, where the model's command could reach it.
//
// It is bind-mounted in from the host rather than installed in the image because
// the image is not asked to cooperate. The pinned default (alpine:3.20) has no
// socat, and an operator's own image is no likelier to. Built with CGO_ENABLED=0
// (`make egress-forwarder`) it is a static binary with no libc, no interpreter,
// and no runtime dependency on anything in the image.
//
// # Usage
//
//	fuse-egress-forward -listen 127.0.0.1:3128 -socket /run/fuse/egress.sock \
//	    -- /bin/sh -c "<command>"
//
// # The two relay targets (change 0075)
//
// On Kubernetes there is no socket to bind-mount: the sandbox is a Pod in a
// tenant namespace and fuse is another Pod, so the only hole the NetworkPolicy
// opens is one TCP destination. The forwarder therefore also accepts
//
//	fuse-egress-forward -listen 127.0.0.1:3128 -upstream tls://10.0.0.7:3129 \
//	    -tls-cert /etc/fuse/egress-tls/tls.crt \
//	    -tls-key  /etc/fuse/egress-tls/tls.key \
//	    -tls-ca   /etc/fuse/egress-tls/ca.crt
//
// which relays each accepted loopback connection over MUTUAL TLS to fuse's own
// listener instead. The relay is still byte-for-byte and this program still
// understands no HTTP: the client certificate is how the far side learns which
// principal's policy to serve, so no policy lives Pod-side in either mode.
//
// `-socket` and `-upstream` are MUTUALLY EXCLUSIVE, and the exclusion is enforced
// here rather than left to whoever renders the argv: this program is the thing
// that would have to choose between two relay targets, and a forwarder that
// silently preferred one would send a sandbox's traffic somewhere its operator
// did not configure. A conflict, a missing target, or an incomplete TLS triple is
// exit 2 — a usage error, before anything has been attempted.
//
// With a child command it becomes that command's PARENT: it binds the listener
// FIRST, then execs the child, so the very first thing the command does can be a
// network call with no readiness race. It exits with the child's exit status, and
// relays SIGINT/SIGTERM to it. With no child it serves until killed.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
)

// maxRelays bounds how many loopback connections this forwarder relays at
// once.
//
// Every accepted connection costs two file descriptors here and, on the far
// side of the boundary, a host-side goroutine and possibly a host-side upstream
// socket in the fuse process. The client is a shell command the MODEL wrote, so
// `for i in $(seq 100000); do curl -s https://declared.example & done` is one
// line, and without a bound the only ceiling is the container's own pids/nofile
// limits — which are sized to keep a command from wedging its container, not to
// keep it from spending the host's descriptor table.
//
// A connection over the bound is REFUSED, never queued: parking it would hold
// the descriptor the bound exists to save, and the host-side proxy makes the
// same choice for the same reason.
//
// The value matches the host proxy's per-principal ceiling. That is not a
// coincidence and not an assumption of authority: this bound protects the
// CONTAINER's descriptor budget and fails fast locally, while the host-side
// ceiling is the security-relevant one and stays authoritative — several
// containers can share one principal's listener, so the host still refuses
// beyond its own share regardless of what any single forwarder allows.
const maxRelays = 128

func main() {
	os.Exit(run(os.Args[1:]))
}

// relayMode is which of the two mutually exclusive relay targets was named.
type relayMode int

const (
	// modeSocket relays to a bind-mounted UNIX socket (the local container
	// substrate, change 0064).
	modeSocket relayMode = iota
	// modeTLS relays to fuse's own TLS listener over mutual TLS (the Kubernetes
	// sidecar, change 0075).
	modeTLS
)

// proxyServerName is the name the proxy's leaf certificate carries and the name
// this forwarder verifies.
//
// It is PINNED rather than taken from -upstream's host, and that is the point:
// the upstream address is an IP (a Pod IP, never a Service ClusterIP — a
// load-balanced address would send a principal to an instance that does not hold
// its policy), and an IP changes every time fuse is rescheduled. Verifying a
// fixed name against the mounted CA means the certificate proves WHAT was
// reached while the address only says where to look. It must agree with the
// proxy's own proxyTLSServerName; internal/tools/sandbox/kubernetes asserts that.
const proxyServerName = "fuse-egress-proxy"

// upstreamTarget is the validated relay target.
type upstreamTarget struct {
	mode relayMode
	// address is the socket path (modeSocket) or "host:port" (modeTLS).
	address string
	// The TLS triple, populated only in modeTLS.
	certPEM []byte
	keyPEM  []byte
	caPEM   []byte
}

// options is everything parseFlags resolved.
type options struct {
	listen string
	target upstreamTarget
	child  []string
}

// parseFlags is the whole of this binary's usage contract, split out of run so
// the matrix of accepted and refused combinations is testable without binding a
// port or starting a process.
//
// Every return of a non-nil error becomes exit 2. Nothing has been attempted at
// that point: no listener, no dial, no child.
func parseFlags(argv []string) (options, error) {
	fs := flag.NewFlagSet("fuse-egress-forward", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	listen := fs.String("listen", "", "loopback address to listen on (e.g. 127.0.0.1:3128)")
	socket := fs.String("socket", "", "path of the mounted UNIX socket to relay to (mutually exclusive with -upstream)")
	upstreamFlag := fs.String("upstream", "", "tls://host:port of fuse's own egress listener (mutually exclusive with -socket)")
	tlsCert := fs.String("tls-cert", "", "PEM client certificate for -upstream")
	tlsKey := fs.String("tls-key", "", "PEM private key for -upstream")
	tlsCA := fs.String("tls-ca", "", "PEM CA bundle that -upstream's certificate is verified against")
	if err := fs.Parse(argv); err != nil {
		return options{}, err
	}

	if *listen == "" {
		return options{}, errors.New("-listen is required")
	}

	// EXACTLY ONE TARGET. Both is a conflict the forwarder must not resolve on
	// its own, and neither leaves it with nowhere to relay to.
	switch {
	case *socket != "" && *upstreamFlag != "":
		return options{}, errors.New("-socket and -upstream are mutually exclusive; name exactly one relay target")
	case *socket == "" && *upstreamFlag == "":
		return options{}, errors.New("one of -socket or -upstream is required")
	}

	opts := options{listen: *listen, child: fs.Args()}

	if *socket != "" {
		// TLS material in socket mode is a CONFIGURATION MISTAKE, not a harmless
		// extra: it means whoever rendered this argv believed they were setting up
		// the TLS datapath. Accepting it silently would run the sandbox over a
		// socket that may not be the one they meant.
		if *tlsCert != "" || *tlsKey != "" || *tlsCA != "" {
			return options{}, errors.New("-tls-cert/-tls-key/-tls-ca apply to -upstream only; they are meaningless with -socket")
		}
		opts.target = upstreamTarget{mode: modeSocket, address: *socket}
		return opts, nil
	}

	addr, err := parseUpstream(*upstreamFlag)
	if err != nil {
		return options{}, err
	}
	// ALL THREE OR NONE. A partial triple cannot produce a working mutual-TLS
	// connection, and every partial combination is named separately so the
	// operator is told which piece is missing rather than "TLS failed".
	if *tlsCert == "" || *tlsKey == "" || *tlsCA == "" {
		missing := make([]string, 0, 3)
		if *tlsCert == "" {
			missing = append(missing, "-tls-cert")
		}
		if *tlsKey == "" {
			missing = append(missing, "-tls-key")
		}
		if *tlsCA == "" {
			missing = append(missing, "-tls-ca")
		}
		return options{}, fmt.Errorf("-upstream requires -tls-cert, -tls-key and -tls-ca; missing %s", strings.Join(missing, ", "))
	}

	cert, err := os.ReadFile(*tlsCert)
	if err != nil {
		return options{}, fmt.Errorf("read -tls-cert: %w", err)
	}
	key, err := os.ReadFile(*tlsKey)
	if err != nil {
		return options{}, fmt.Errorf("read -tls-key: %w", err)
	}
	ca, err := os.ReadFile(*tlsCA)
	if err != nil {
		return options{}, fmt.Errorf("read -tls-ca: %w", err)
	}

	opts.target = upstreamTarget{mode: modeTLS, address: addr, certPEM: cert, keyPEM: key, caPEM: ca}
	return opts, nil
}

// parseUpstream requires the `tls://` scheme and an explicit host:port.
//
// The scheme is REQUIRED and is the only one accepted, because this program
// relays a sandbox's whole egress: a bare "host:port" would be ambiguous about
// whether the hop is encrypted, and silently choosing plaintext would put every
// principal's traffic — and the client certificate that identifies it — on the
// wire in the clear. The port is required for the same reason the proxy never
// defaults one: a guessed port authorizes against a listener nobody configured.
func parseUpstream(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("-upstream %q is not a URL: %w", raw, err)
	}
	if u.Scheme != "tls" {
		return "", fmt.Errorf("-upstream %q must be tls://host:port (this forwarder speaks no other upstream scheme)", raw)
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil || host == "" || port == "" {
		return "", fmt.Errorf("-upstream %q must name an explicit host and port", raw)
	}
	return u.Host, nil
}

// dialUpstream opens one connection to the relay target.
type dialUpstream func() (net.Conn, error)

// newUpstreamDialer turns a validated target into the dialer the relay uses.
//
// The TLS configuration is built ONCE, here, rather than per connection: the
// client certificate and the CA pool are fixed for the Pod's life, and parsing
// them per connection would turn a mounted-Secret problem into a per-request
// failure the operator sees as intermittent.
func newUpstreamDialer(t upstreamTarget) (dialUpstream, error) {
	if t.mode == modeSocket {
		return socketDialer(t.address), nil
	}

	cert, err := tls.X509KeyPair(t.certPEM, t.keyPEM)
	if err != nil {
		return nil, fmt.Errorf("the -tls-cert/-tls-key pair is unusable: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(t.caPEM) {
		return nil, errors.New("-tls-ca contains no certificate")
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		// The PINNED name, not the address. See proxyServerName.
		ServerName: proxyServerName,
		// Both ends are fuse's own code, so there is no legacy peer to
		// accommodate and the floor can be the current one.
		MinVersion: tls.VersionTLS13,
	}
	addr := t.address
	return func() (net.Conn, error) { return tls.Dial("tcp", addr, cfg) }, nil
}

// run is main's body, returning the process exit code so it is testable.
func run(argv []string) int {
	opts, err := parseFlags(argv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fuse-egress-forward: %v\n", err)
		return 2
	}

	dialer, err := newUpstreamDialer(opts.target)
	if err != nil {
		// The TLS material was named but is unusable. This is exit 2 as well: it
		// is still a statement about the arguments, and nothing has been
		// attempted — no listener is bound and no child has started.
		fmt.Fprintf(os.Stderr, "fuse-egress-forward: %v\n", err)
		return 2
	}

	// Bound BEFORE the child is started. This ordering is the whole reason the
	// forwarder wraps the command instead of being backgrounded next to it: when
	// the child's first instruction runs, the listener is already accepting.
	ln, err := net.Listen("tcp", opts.listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fuse-egress-forward: listen on %s: %v\n", opts.listen, err)
		return 1
	}
	defer func() { _ = ln.Close() }()

	go serve(ln, dialer)

	child := opts.child
	if len(child) == 0 {
		// No command to wrap: serve until the container is torn down. Nothing
		// else can end this process, which is correct — a forwarder that exited
		// on its own would silently close the only path out.
		select {}
	}
	return runChild(child)
}

// serve accepts loopback connections and relays each to the mounted socket, up
// to maxRelays at once.
//
// An accept error ends the loop rather than retrying: the only expected cause is
// the listener being closed at teardown, and spinning on a broken listener would
// burn a core inside a cgroup-capped container for the rest of the command's run.
func serve(ln net.Listener, dial dialUpstream) {
	serveBoundedWith(ln, dial, maxRelays)
}

// socketDialer is the UNIX-socket relay target as a dialUpstream. It exists so
// the socket path is expressed once, in one place both serve and serveBounded
// reach for.
func socketDialer(socket string) dialUpstream {
	return func() (net.Conn, error) { return net.Dial("unix", socket) }
}

// serveBounded is serveBoundedWith over a UNIX socket path, kept because the
// socket path is the original datapath and most of this file's tests drive it.
func serveBounded(ln net.Listener, socket string, limit int) {
	serveBoundedWith(ln, socketDialer(socket), limit)
}

// serveBoundedWith is serve with the bound and the dialer both made explicit, so
// a test can drive it with two connections instead of manufacturing a hundred and
// can substitute either relay target.
//
// The slot is taken ON THE ACCEPT LOOP, before the relay goroutine exists,
// because the accepting side is the only place the cost can still be declined —
// a bound checked inside the goroutine has already paid for it. The take is
// non-blocking: a full bound refuses immediately and the loop goes straight
// back to accepting, so nothing accumulates either as goroutines or as a queue
// of parked connections.
func serveBoundedWith(ln net.Listener, dial dialUpstream, limit int) {
	if limit < 1 {
		limit = 1
	}
	slots := make(chan struct{}, limit)
	// The warning is announced ONCE. A line per refusal would be its own
	// unbounded thing — the runaway loop this bound exists to survive would
	// write a hundred thousand of them into the command's stderr, burying the
	// output the model is actually reading.
	var announce sync.Once

	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		select {
		case slots <- struct{}{}:
		default:
			// REFUSED: closed with nothing written, exactly as an unreachable
			// socket is. This program speaks no HTTP, and a fabricated error
			// response would be the one message a container-side component
			// authored — the far side does all the deciding, including how a
			// refusal is phrased.
			announce.Do(func() {
				fmt.Fprintf(os.Stderr, "fuse-egress-forward: at capacity (%d concurrent connections); further connections are refused until one ends\n", limit)
			})
			_ = conn.Close()
			continue
		}
		go func() {
			defer func() { <-slots }()
			relay(conn, dial)
		}()
	}
}

// relay joins one accepted loopback connection to a fresh connection on the
// mounted UNIX socket.
//
// One upstream connection PER accepted connection, never a shared or pooled one:
// the far side speaks HTTP CONNECT, and a CONNECT tunnel owns its connection for
// its whole life. Multiplexing would splice two commands' tunnels together.
//
// A dial failure closes the client connection with nothing written. There is no
// error page and no fallback path, because any fallback here would be a way out
// of the container that did not pass the proxy.
func relay(client net.Conn, dial dialUpstream) {
	defer func() { _ = client.Close() }()

	upstream, err := dial()
	if err != nil {
		// Closed with NOTHING written — the same answer an unreachable socket
		// gets, whether the failure was a refused dial or a refused handshake.
		// There is no error page and no fallback, because any fallback here is a
		// way out of the sandbox that did not pass the proxy.
		return
	}
	defer func() { _ = upstream.Close() }()

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, client)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		done <- struct{}{}
	}()

	// Closing both halves as soon as either direction ends is what unblocks the
	// other copy: a half-closed tunnel has no reader left to consume anything the
	// remaining half sends.
	<-done
	_ = client.Close()
	_ = upstream.Close()
	<-done
}

// runChild execs the wrapped command with this process's stdio and returns its
// exit status.
//
// Signals are relayed rather than ignored. This process is PID 1 in the
// container, and PID 1 has no default disposition for SIGTERM — a `docker stop`
// against an unhandled PID 1 waits out the full grace period and then SIGKILLs
// the whole container, which would look to a caller like every command hanging on
// cancellation.
func runChild(argv []string) int {
	cmd := exec.Command(argv[0], argv[1:]...) // #nosec G204 -- argv is fuse-supplied; see package doc
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "fuse-egress-forward: %v\n", err)
		return 127
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)
	relayed := make(chan struct{})
	go func() {
		for {
			select {
			case sig := <-sigs:
				_ = cmd.Process.Signal(sig)
			case <-relayed:
				return
			}
		}
	}()

	err := cmd.Wait()
	close(relayed)
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return code
		}
		// Killed by a signal: the shell convention, so a caller reading only the
		// status still sees a failure.
		return 128
	}
	fmt.Fprintf(os.Stderr, "fuse-egress-forward: %v\n", err)
	return 1
}
