package main

import (
	"bufio"
	"io"
	"net"
	"os"
	"path/filepath"
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
	go serve(ln, socket)

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
	go serve(ln, socket)

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
	go serve(ln, filepath.Join(t.TempDir(), "absent.sock"))

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
