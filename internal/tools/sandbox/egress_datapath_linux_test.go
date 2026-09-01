//go:build egress_datapath

// End-to-end egress datapath verification against a REAL container runtime.
// Gated behind the `egress_datapath` build tag so it never runs in the normal
// suite: it starts an actual container and needs a working Docker/OCI daemon
// with host UNIX-socket bind-mounts that function inside the container.
//
// This is the check no unit test can reach and that Docker Desktop for macOS
// cannot satisfy — a host UNIX socket bind-mounted through the macOS→VM
// file-sharing layer is not a working socket inside the container. It DOES hold
// on a native-Linux Docker host (a Linux CI runner), where the socket is a real
// host socket the container shares directly. Run it there:
//
//	make egress-forwarder                     # build the arch-matched artifact
//	go test -tags egress_datapath -run TestEgressDatapathEndToEnd -v \
//	    -count=1 -timeout 300s ./internal/tools/sandbox/
//
// It settles the four "Verify (human)" items from change 0064's results file:
//   - a :ro bind-mounted UNIX socket still accepts connect(2)
//   - /run/fuse is created implicitly by the two file bind-mounts
//   - end-to-end enforcement: declared reachable, undeclared refused
//   - the arch-matched forwarder artifact resolves and is mounted
package sandbox

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/loopauth"
)

func TestEgressDatapathEndToEnd(t *testing.T) {
	// This file is GOOS=linux only (the _linux_test.go suffix): the datapath
	// bind-mounts a host UNIX socket into the container, which relays correctly
	// only where the socket is a genuine host socket — a native-Linux daemon,
	// not Docker Desktop's macOS→VM file-sharing layer.
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("egress datapath e2e requires a container runtime on PATH")
	}
	forwarder := forwarderArtifactForTest(t)

	// Two real host-side HTTP servers. The HOST-side proxy dials the declared
	// destination directly on the host (pl.proxy.dialer.DialContext), so the
	// declared host is a host-loopback address the proxy reaches — never
	// anything the --network none container can see. We declare the first port
	// and leave the second off the allowlist.
	declared := httpEcho(t, "DECLARED-OK")
	undeclared := httpEcho(t, "UNDECLARED-SHOULD-NEVER-SEE")
	_, declaredPort := listenerHostPort(t, declared)
	_, undeclaredPort := listenerHostPort(t, undeclared)
	const dialHost = "127.0.0.1"
	t.Logf("declared=%s:%d (allowed)  undeclared=%s:%d (not allowed)", dialHost, declaredPort, dialHost, undeclaredPort)

	var mu sync.Mutex
	var refusals []RefusalInfo
	proxy, err := NewProxy(WithProxyHooks(ProxyHooks{Refused: func(ri RefusalInfo) {
		mu.Lock()
		refusals = append(refusals, ri)
		mu.Unlock()
		t.Logf("proxy REFUSED host=%s port=%d reason=%s", ri.Host, ri.Port, ri.Reason)
	}}))
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	defer proxy.Close()

	// 127.0.0.1 is an IP literal, declared as a full-mask CIDR (/32) — the shape
	// Match compares IP destinations against (a bare-IP Host would never match).
	_, loopback32, err := net.ParseCIDR(dialHost + "/32")
	if err != nil {
		t.Fatalf("parse cidr: %v", err)
	}
	cfg := DefaultConfig()
	cfg.Egress = Egress{
		Mode:  EgressEnforce,
		Allow: []AllowEntry{{CIDR: loopback32, Port: declaredPort}},
	}

	// A trusted workspace root the container bind-mounts; working_dir lives here.
	workRoot, err := os.MkdirTemp("", "fuse-egress-e2e-")
	if err != nil {
		t.Fatalf("mkdir workroot: %v", err)
	}
	defer os.RemoveAll(workRoot)

	svc, err := NewService(cfg, WithEgressProxy(proxy, forwarder), WithTrustedRoot(workRoot))
	if err != nil {
		t.Fatalf("service: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	runner, err := svc.Acquire(ctx, loopauth.Principal{Tenant: "t1", Subject: "s1"})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer runner.Release(ctx)

	// The datapath is bind-mounted correctly: /run/fuse exists, holds a real
	// socket and the forwarder binary.
	diag, err := runner.Exec(ctx, "stat -c '%F' /run/fuse/egress.sock 2>&1; test -x /run/fuse/egress-forward && echo forwarder-present", "")
	if err != nil {
		t.Fatalf("exec diag: %v", err)
	}
	if got := string(diag.Combined); !strings.Contains(got, "socket") || !strings.Contains(got, "forwarder-present") {
		t.Fatalf("datapath not mounted as expected: %q", strings.TrimSpace(got))
	}

	// 1) Floor is on: --network none leaves no default interface.
	ifaces, err := runner.Exec(ctx, "ip -o addr show 2>&1 | awk '{print $2}' | sort -u", "")
	if err != nil {
		t.Fatalf("exec ip addr: %v", err)
	}
	if strings.Contains(string(ifaces.Combined), "eth0") {
		t.Fatalf("FLOOR NOT APPLIED: container has eth0 (--network none not emitted): %q", strings.TrimSpace(string(ifaces.Combined)))
	}

	// 2) Undeclared destination is refused (the floor + allowlist deny it).
	out1, err := runner.Exec(ctx, fmt.Sprintf("wget -T 5 -qO- http://%s:%d/ 2>&1 || echo REFUSED", dialHost, undeclaredPort), "")
	if err != nil {
		t.Fatalf("exec undeclared: %v", err)
	}
	t.Logf("undeclared -> %q", strings.TrimSpace(string(out1.Combined)))
	if strings.Contains(string(out1.Combined), "UNDECLARED-SHOULD-NEVER-SEE") {
		t.Fatal("FLOOR BREACH: reached an undeclared destination")
	}

	// 3) Declared destination is reachable THROUGH the datapath end to end.
	out2, err := runner.Exec(ctx, fmt.Sprintf("wget -T 8 -qO- http://%s:%d/ 2>&1", dialHost, declaredPort), "")
	if err != nil {
		t.Fatalf("exec declared: %v", err)
	}
	t.Logf("declared -> %q", strings.TrimSpace(string(out2.Combined)))
	if !strings.Contains(string(out2.Combined), "DECLARED-OK") {
		t.Fatalf("DATAPATH BROKEN: declared destination not reachable; got %q", strings.TrimSpace(string(out2.Combined)))
	}
}

func forwarderArtifactForTest(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("abs repo root: %v", err)
	}
	p := filepath.Join(root, "dist", fmt.Sprintf("fuse-egress-forward-linux-%s", runtime.GOARCH))
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("forwarder artifact missing (%s); run `make egress-forwarder` first: %v", p, err)
	}
	return p
}

func httpEcho(t *testing.T, body string) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return ln
}

func listenerHostPort(t *testing.T, ln net.Listener) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	return host, port
}
