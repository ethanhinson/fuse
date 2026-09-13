package main

import (
	"bufio"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// unixEchoServer stands in for the host-side egress proxy: it accepts on a UNIX
// socket and echoes each line back with a prefix, so a test can prove that bytes
// went in one end and the far side's answer came back out the other.
func unixEchoServer(t *testing.T) string {
	t.Helper()
	// Kept short deliberately: sun_path is capped at 104 bytes on darwin, and
	// t.TempDir() alone can spend most of that budget.
	dir, err := os.MkdirTemp("", "fx")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	path := filepath.Join(dir, "egress.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				br := bufio.NewReader(conn)
				for {
					line, err := br.ReadString('\n')
					if line != "" {
						if _, werr := io.WriteString(conn, "upstream:"+line); werr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return path
}

// The load-bearing property: a TCP connection to the loopback listener is relayed
// to the mounted UNIX socket in BOTH directions. This is the only thing that
// makes the `--network none` container's injected HTTP_PROXY mean anything, and
// it is asserted against a real listener and a real socket rather than a stub.
func TestRelayJoinsLoopbackToUnixSocket(t *testing.T) {
	socket := unixEchoServer(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go serve(ln, socketDialer(socket))

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := io.WriteString(conn, "CONNECT example.com:443 HTTP/1.1\r\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if want := "upstream:CONNECT example.com:443 HTTP/1.1\r\n"; got != want {
		t.Fatalf("relayed round trip = %q, want %q", got, want)
	}
}

// Two concurrent clients each get their OWN upstream connection. A pooled or
// shared upstream would splice two commands' CONNECT tunnels together, which is
// the same defect class as a shared server holding a single current peer.
func TestRelayGivesEachClientItsOwnUpstreamConnection(t *testing.T) {
	socket := unixEchoServer(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go serve(ln, socketDialer(socket))

	open := func(payload string) (net.Conn, *bufio.Reader) {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.WriteString(c, payload); err != nil {
			t.Fatalf("write: %v", err)
		}
		return c, bufio.NewReader(c)
	}

	_, ra := open("alpha\n")
	_, rb := open("beta\n")

	gotA, err := ra.ReadString('\n')
	if err != nil {
		t.Fatalf("read a: %v", err)
	}
	gotB, err := rb.ReadString('\n')
	if err != nil {
		t.Fatalf("read b: %v", err)
	}
	if gotA != "upstream:alpha\n" || gotB != "upstream:beta\n" {
		t.Fatalf("crossed streams: a = %q, b = %q", gotA, gotB)
	}
}

// A client that arrives when the socket is unreachable gets its connection
// CLOSED, with nothing written. There is no error page and no fallback: any
// answer other than "closed" would be a path out of the container that did not
// pass the proxy.
func TestRelayClosesClientWhenSocketIsUnreachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go serve(ln, socketDialer(filepath.Join(t.TempDir(), "absent.sock")))

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := io.ReadAll(conn); err != nil {
		t.Fatalf("read: %v", err)
	}
	// io.ReadAll returning no error and no bytes IS the assertion: EOF with an
	// empty body means the connection was closed without a reply.
}

// The listener is bound BEFORE the wrapped child starts. That ordering is the
// reason this program wraps the command rather than being backgrounded beside it,
// and without it the first network call of every enforced command would race the
// forwarder's startup.
func TestRunBindsListenerBeforeStartingTheChild(t *testing.T) {
	socket := unixEchoServer(t)

	// A free port, released immediately so run can take it. The wrapped child is
	// this test binary re-invoked as helperDialTest, which dials that address the
	// moment it starts — so a green run means the listener was already accepting
	// before the child's first instruction, with no sleep and no retry to paper
	// over a race.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	t.Setenv(dialEnv, addr)
	code := run([]string{"-listen", addr, "-socket", socket, "--", os.Args[0], "-test.run=^TestHelperDialsForwarder$"})
	if code != 0 {
		t.Fatalf("child exit code = %d, want 0 — the listener was not up when the child ran", code)
	}
}

// dialEnv activates the helper below. It is inert in an ordinary run.
const dialEnv = "FUSE_EGRESS_FORWARD_TEST_DIAL"

// TestHelperDialsForwarder is not a test: it is the child process
// TestRunBindsListenerBeforeStartingTheChild wraps. It exits 0 if it can reach
// the forwarder's listener immediately and 9 if it cannot.
func TestHelperDialsForwarder(t *testing.T) {
	addr := os.Getenv(dialEnv)
	if addr == "" {
		t.Skip("helper process; only runs as a child of TestRunBindsListenerBeforeStartingTheChild")
	}
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		os.Exit(9)
	}
	_ = conn.Close()
	os.Exit(0)
}

// run propagates the wrapped child's exit status. The container's exit code is
// the command's exit code, enforcement or not.
func TestRunPropagatesChildExitCode(t *testing.T) {
	socket := unixEchoServer(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	if code := run([]string{"-listen", addr, "-socket", socket, "--", "/bin/sh", "-c", "exit 42"}); code != 42 {
		t.Fatalf("exit code = %d, want 42", code)
	}
}

// Both flags are required. A forwarder that defaulted either would be guessing at
// the boundary it exists to implement.
func TestRunRequiresListenAndSocket(t *testing.T) {
	for name, argv := range map[string][]string{
		"no socket": {"-listen", "127.0.0.1:0"},
		"no listen": {"-socket", "/run/fuse/egress.sock"},
		"neither":   {},
	} {
		t.Run(name, func(t *testing.T) {
			if code := run(argv); code != 2 {
				t.Fatalf("exit code = %d, want 2", code)
			}
		})
	}
}

// The forwarder bounds how many connections it relays at once, and a
// connection over that bound is REFUSED — closed immediately — rather than
// queued.
//
// It has the same shape as the host-side proxy's accept loop and therefore the
// same hazard: one `go relay(...)` per accepted connection, driven by a shell
// command the model wrote. Unbounded, `for i in $(seq 100000); do curl & done`
// is capped only by the container's pids/nofile limits, and every one of those
// relays costs two file descriptors and a socket on the far side of the
// boundary as well.
//
// Refusing by closing with nothing written is the same answer the unreachable-
// socket path gives, deliberately: this program understands no HTTP, and a
// fabricated 503 would be the one message a container-side component authored.
func TestServeRefusesConnectionsOverTheBound(t *testing.T) {
	socket := unixEchoServer(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go serveBounded(ln, socket, 1)

	// The first connection takes the only slot and HOLDS it: a completed round
	// trip proves the relay is live, and leaving the connection open keeps it
	// that way for the rest of the test.
	held, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = held.Close() })
	_ = held.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(held, "alpha\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got, err := bufio.NewReader(held).ReadString('\n'); err != nil || got != "upstream:alpha\n" {
		t.Fatalf("held relay round trip = %q, %v", got, err)
	}

	// The second connection is over the bound. It is closed with nothing
	// written, which is what io.ReadAll returning no bytes and no error means.
	over, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = over.Close() }()
	_ = over.SetDeadline(time.Now().Add(5 * time.Second))
	body, err := io.ReadAll(over)
	if err != nil {
		t.Fatalf("connection over the bound was neither refused nor served: %v", err)
	}
	if len(body) != 0 {
		t.Fatalf("refused connection got %q, want nothing written", body)
	}

	// The bound is a bound, not a brick: ending the held connection returns its
	// slot and the next connection is relayed again.
	_ = held.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if tryRelay(t, ln.Addr().String()) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the relay slot was never returned; the bound is a one-shot brick")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// tryRelay reports whether one round trip through the forwarder succeeds,
// without failing the test when it does not.
func tryRelay(t *testing.T, addr string) bool {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, "beta\n"); err != nil {
		return false
	}
	got, err := bufio.NewReader(conn).ReadString('\n')
	return err == nil && got == "upstream:beta\n"
}

// The bound holds when the connections arrive ALL AT ONCE, which is how a
// runaway `curl ... &` loop arrives. Under -race this also covers the accept
// loop taking slots concurrently with the relay goroutines returning them.
func TestServeBoundHoldsUnderConcurrentDials(t *testing.T) {
	socket := unixEchoServer(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	const bound, dials = 4, 64
	go serveBounded(ln, socket, bound)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var relayed int
	for i := 0; i < dials; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				return
			}
			// Held for the whole test, never closed here: the bound is on
			// CONCURRENT relays, so a connection that finished would hand its
			// slot to the next one and every dial would eventually succeed —
			// correctly, and while proving nothing.
			t.Cleanup(func() { _ = conn.Close() })
			_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
			if _, err := io.WriteString(conn, "alpha\n"); err != nil {
				return
			}
			// A refused connection is closed with nothing written, so this read
			// ends in EOF rather than a line. The far side's answer is the only
			// proof a relay actually happened.
			if got, err := bufio.NewReader(conn).ReadString('\n'); err == nil && got == "upstream:alpha\n" {
				mu.Lock()
				relayed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	// Exact, not approximate: nothing releases a slot during this test, so once
	// every dial has been answered exactly `bound` connections are relayed and
	// every other one has been refused.
	if relayed != bound {
		t.Fatalf("%d of %d simultaneous connections were relayed against a bound of %d", relayed, dials, bound)
	}
}

// THE FLAG MATRIX for the upstream mode (change 0075, task 8).
//
// `-socket` and `-upstream` are the two mutually exclusive relay targets: a UNIX
// socket bind-mounted from the host (the local container substrate) and a TLS
// connection to fuse's own listener (the Kubernetes sidecar). Exactly one must be
// named.
//
// The exclusion is enforced HERE, in this binary's own flag validation, rather
// than being left to whoever renders the argv. This program is the thing that
// would have to choose between two targets, and a forwarder that silently
// preferred one would relay a sandbox's traffic somewhere its operator did not
// configure. Exit 2 — a usage error, not a runtime failure — because nothing has
// been attempted yet.
func TestForwarderFlagMatrix(t *testing.T) {
	// A file that exists so the TLS-triple cases fail on the FLAG grammar and not
	// on a missing file: the validation under test is "was the triple supplied",
	// which must be answered before anything is read.
	dir := t.TempDir()
	pem := filepath.Join(dir, "x.pem")
	if err := os.WriteFile(pem, []byte("not a real certificate\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tls3 := []string{"-tls-cert", pem, "-tls-key", pem, "-tls-ca", pem}
	withTLS := func(argv ...string) []string { return append(argv, tls3...) }

	for name, tc := range map[string]struct {
		argv    []string
		wantErr bool
	}{
		"socket alone": {
			argv: []string{"-listen", "127.0.0.1:0", "-socket", "/run/fuse/egress.sock"},
		},
		"upstream with the full TLS triple": {
			argv: withTLS("-listen", "127.0.0.1:0", "-upstream", "tls://10.0.0.1:3129"),
		},
		"socket AND upstream": {
			argv:    withTLS("-listen", "127.0.0.1:0", "-socket", "/run/fuse/egress.sock", "-upstream", "tls://10.0.0.1:3129"),
			wantErr: true,
		},
		"neither socket nor upstream": {
			argv:    []string{"-listen", "127.0.0.1:0"},
			wantErr: true,
		},
		"upstream with no TLS material at all": {
			argv:    []string{"-listen", "127.0.0.1:0", "-upstream", "tls://10.0.0.1:3129"},
			wantErr: true,
		},
		"upstream missing the cert": {
			argv:    []string{"-listen", "127.0.0.1:0", "-upstream", "tls://10.0.0.1:3129", "-tls-key", pem, "-tls-ca", pem},
			wantErr: true,
		},
		"upstream missing the key": {
			argv:    []string{"-listen", "127.0.0.1:0", "-upstream", "tls://10.0.0.1:3129", "-tls-cert", pem, "-tls-ca", pem},
			wantErr: true,
		},
		"upstream missing the CA": {
			argv:    []string{"-listen", "127.0.0.1:0", "-upstream", "tls://10.0.0.1:3129", "-tls-cert", pem, "-tls-key", pem},
			wantErr: true,
		},
		"TLS material supplied in socket mode": {
			argv:    withTLS("-listen", "127.0.0.1:0", "-socket", "/run/fuse/egress.sock"),
			wantErr: true,
		},
		"upstream in a scheme this program does not speak": {
			argv:    withTLS("-listen", "127.0.0.1:0", "-upstream", "tcp://10.0.0.1:3129"),
			wantErr: true,
		},
		"upstream with no scheme at all": {
			argv:    withTLS("-listen", "127.0.0.1:0", "-upstream", "10.0.0.1:3129"),
			wantErr: true,
		},
		"upstream with no port": {
			argv:    withTLS("-listen", "127.0.0.1:0", "-upstream", "tls://10.0.0.1"),
			wantErr: true,
		},
		"no listen": {
			argv:    []string{"-socket", "/run/fuse/egress.sock"},
			wantErr: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseFlags(tc.argv)
			if tc.wantErr && err == nil {
				t.Fatalf("parseFlags(%v) accepted; want a usage error", tc.argv)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("parseFlags(%v) = %v, want acceptance", tc.argv, err)
			}
		})
	}
}

// Every rejection parseFlags makes must reach the process as exit 2, and never as
// exit 1 (a runtime failure) or 0. run is what the container's entrypoint calls,
// so this is the assertion that actually pins the contract.
func TestRunExitsTwoOnEveryFlagConflict(t *testing.T) {
	dir := t.TempDir()
	pem := filepath.Join(dir, "x.pem")
	if err := os.WriteFile(pem, []byte("not a real certificate\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	for name, argv := range map[string][]string{
		"socket and upstream together": {
			"-listen", "127.0.0.1:0", "-socket", "/run/fuse/egress.sock",
			"-upstream", "tls://10.0.0.1:3129", "-tls-cert", pem, "-tls-key", pem, "-tls-ca", pem,
		},
		"an incomplete TLS triple": {
			"-listen", "127.0.0.1:0", "-upstream", "tls://10.0.0.1:3129", "-tls-cert", pem,
		},
		"neither target": {"-listen", "127.0.0.1:0"},
	} {
		t.Run(name, func(t *testing.T) {
			if code := run(argv); code != 2 {
				t.Fatalf("exit code = %d, want 2", code)
			}
		})
	}
}

// THE RELAY IS BYTE-FOR-BYTE IN UPSTREAM MODE TOO.
//
// The whole value of this program is that no policy lives Pod-side. Swapping the
// transport from a UNIX socket to a TLS connection must not change that: the same
// bytes go up and the same bytes come back, and this program still understands no
// HTTP. The far side does all the deciding.
func TestRelayToTLSUpstreamIsByteForByte(t *testing.T) {
	up, cred := tlsEchoServer(t)

	dialer, err := newUpstreamDialer(upstreamTarget{
		mode:    modeTLS,
		address: up,
		certPEM: cred.certPEM,
		keyPEM:  cred.keyPEM,
		caPEM:   cred.caPEM,
	})
	if err != nil {
		t.Fatalf("newUpstreamDialer: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go serveBoundedWith(ln, dialer, 4)

	conn, err := net.DialTimeout("tcp", ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial forwarder: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := io.WriteString(conn, "hello\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != "upstream:hello\n" {
		t.Fatalf("relayed = %q, want %q", got, "upstream:hello\n")
	}
}

// An upstream that refuses the client certificate closes the loopback connection
// with NOTHING written — the same answer an unreachable socket gets. There is no
// error page and no fallback, because any fallback here is a way out of the Pod
// that did not pass the proxy.
func TestRelayToTLSUpstreamClosesClientOnHandshakeFailure(t *testing.T) {
	up, cred := tlsEchoServer(t)

	// A key that does not match the certificate's CA: the handshake fails.
	other := newTestCredential(t)
	dialer, err := newUpstreamDialer(upstreamTarget{
		mode:    modeTLS,
		address: up,
		certPEM: other.certPEM,
		keyPEM:  other.keyPEM,
		caPEM:   cred.caPEM,
	})
	if err != nil {
		t.Fatalf("newUpstreamDialer: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go serveBoundedWith(ln, dialer, 4)

	conn, err := net.DialTimeout("tcp", ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial forwarder: %v", err)
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, werr := io.WriteString(conn, "hello\n"); werr == nil {
		buf := make([]byte, 1)
		if n, rerr := conn.Read(buf); rerr == nil {
			t.Fatalf("read %d bytes (%q) after a failed handshake; want the connection closed with nothing written", n, buf[:n])
		}
	}
}

// The other half of the cross-binary name contract; see
// internal/tools/sandbox's TestProxyTLSServerNameIsThePinnedLiteral.
func TestProxyServerNameIsThePinnedLiteral(t *testing.T) {
	const pinned = "fuse-egress-proxy"
	if proxyServerName != pinned {
		t.Fatalf("proxyServerName = %q, want %q — internal/tools/sandbox's proxyTLSServerName pins the same literal and the two must agree", proxyServerName, pinned)
	}
}
