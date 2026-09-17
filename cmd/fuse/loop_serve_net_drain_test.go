package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/ethanhinson/fuse/internal/event"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/runtime"
)

// blockingRuntime is a runtime.Runtime double whose StartLoop parks until it is
// released, so a test can hold a unary request IN FLIGHT across a shutdown and
// observe whether the drain waited for it. entered is closed on the first call.
type blockingRuntime struct {
	netFakeRuntime
	entered chan struct{}
	release chan struct{}
}

func newBlockingRuntime() *blockingRuntime {
	return &blockingRuntime{
		netFakeRuntime: netFakeRuntime{startID: "loop-drain"},
		entered:        make(chan struct{}),
		release:        make(chan struct{}),
	}
}

func (b *blockingRuntime) StartLoop(ctx context.Context, cfg runtime.LoopConfig) (runtime.LoopHandle, error) {
	close(b.entered)
	select {
	case <-b.release:
		return netFakeHandle{id: b.startID}, nil
	case <-time.After(30 * time.Second):
		return nil, errors.New("blockingRuntime: never released")
	}
}

func (b *blockingRuntime) awaitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-b.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("StartLoop was never entered")
	}
}

// startDrainServer stands the real network server up on a 127.0.0.1:0 listener with
// the supplied runtime and options, and returns its base URL plus the channel
// serveNetWithOptions's return value lands on. Auth is deliberately supplied (a real
// verifier) so the readiness gate's verifier condition is satisfied and /readyz
// answers on the store probe and the draining flag alone.
func startDrainServer(t *testing.T, rt runtime.Runtime, store event.DurableStore, opts serveNetOptions) (base string, done chan error, drain context.CancelFunc) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	// done is CLOSED rather than sent on, so both the test body and the Cleanup
	// below can read the serve result — a buffered one-shot send would let whichever
	// read first consume it and make the other block forever.
	done = make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		err := serveNetWithOptions(ctx, ln, rt, testVerifier(), nil, store, nil, opts)
		done <- err
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(30 * time.Second):
			t.Error("serveNetWithOptions did not return after cancel")
		}
	})
	base = "http://" + ln.Addr().String()
	waitServing(t, base+"/healthz")
	return base, done, cancel
}

// TestDrainFlipsReadinessBeforeTheListenerStopsAccepting is the load-bearing
// ordering assertion of the whole drain. Readiness MUST report 503 while the
// listener is STILL accepting new connections: that is the window in which a load
// balancer notices the instance is going away and stops routing to it. Reverse the
// two steps and the drain is worthless — the LB keeps sending requests to a socket
// that has already stopped accepting, and those requests are lost rather than
// drained.
//
// The assertion is made from inside the afterDraining seam because it is the only
// place from which it is observable: once Shutdown has begun the listener is closed
// by definition, so a 503 read after that moment could not distinguish "drained
// correctly" from "readiness never flipped and the connection merely failed".
// Inside the seam the server is fully live, so a 503 on a BRAND NEW connection
// proves both halves at once.
func TestDrainFlipsReadinessBeforeTheListenerStopsAccepting(t *testing.T) {
	var (
		codeWhileDraining int
		bodyWhileDraining string
		dialErr           error
		addr              string
	)
	observed := make(chan struct{})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr = ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	opts := serveNetOptions{
		drainTimeout: time.Second,
		afterDraining: func() {
			defer close(observed)
			// A fresh TCP connection: proves the listener is still accepting at
			// this instant, so the 503 below is the readiness gate talking and not
			// a dead socket.
			c, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if err != nil {
				dialErr = err
				return
			}
			_ = c.Close()
			// A fresh HTTP request over a fresh connection (no keep-alive reuse, so
			// this cannot be answered by a connection opened before the drain).
			tr := &http.Transport{DisableKeepAlives: true}
			defer tr.CloseIdleConnections()
			req, err := http.NewRequest(http.MethodGet, "http://"+addr+readyzPath, nil)
			if err != nil {
				dialErr = err
				return
			}
			resp, err := (&http.Client{Transport: tr, Timeout: 2 * time.Second}).Do(req)
			if err != nil {
				dialErr = err
				return
			}
			defer resp.Body.Close()
			codeWhileDraining = resp.StatusCode
			buf := make([]byte, 64)
			n, _ := resp.Body.Read(buf)
			bodyWhileDraining = string(buf[:n])
		},
	}
	go func() {
		done <- serveNetWithOptions(ctx, ln, &netFakeRuntime{startID: "loop-order"}, testVerifier(), nil, &pingStore{}, nil, opts)
	}()
	base := "http://" + addr
	waitServing(t, base+"/healthz")

	// Before the drain: ready.
	if code := mustCode(t, base, readyzPath); code != http.StatusOK {
		t.Fatalf("/readyz before shutdown = %d, want 200", code)
	}

	cancel()
	select {
	case <-observed:
	case <-time.After(10 * time.Second):
		t.Fatal("the afterDraining seam never ran — readiness was never flipped on shutdown")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveNetWithOptions returned %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return after shutdown")
	}

	if dialErr != nil {
		t.Fatalf("probing while draining failed: %v — the listener must still accept while readiness reports 503", dialErr)
	}
	if codeWhileDraining != http.StatusServiceUnavailable {
		t.Fatalf("/readyz while draining (listener still accepting) = %d, want 503 — readiness must flip BEFORE the listener stops accepting, or the load balancer keeps routing here", codeWhileDraining)
	}
	if bodyWhileDraining != "" {
		t.Fatalf("/readyz 503 body = %q, want empty", bodyWhileDraining)
	}
}

// TestDrainCompletesAnInFlightUnaryCall is the point of the drain: a unary request
// that was accepted before shutdown began must still get its real answer. A bare
// srv.Close() severs it and the client sees a transport error instead.
func TestDrainCompletesAnInFlightUnaryCall(t *testing.T) {
	rt := newBlockingRuntime()
	base, done, drain := startDrainServer(t, rt, &pingStore{}, serveNetOptions{drainTimeout: 20 * time.Second})

	type result struct {
		id  string
		err error
	}
	res := make(chan result, 1)
	go func() {
		r, err := bearerClient(base, "tkn").StartLoop(context.Background(),
			connect.NewRequest(&loopv1.StartLoopRequest{Task: "hi", Model: "cloud/x"}))
		if err != nil {
			res <- result{err: err}
			return
		}
		res <- result{id: r.Msg.LoopId}
	}()

	// The request is genuinely in flight inside the handler before we shut down.
	rt.awaitEntered(t)

	// Trigger the shutdown (via the same ctx cancel the signal path uses), then let
	// the handler finish while Shutdown is waiting on it.
	shutdown := make(chan struct{})
	go func() {
		defer close(shutdown)
		// Give Shutdown a moment to be entered and to stop accepting, so the
		// handler completes strictly INSIDE the drain window rather than before it.
		time.Sleep(50 * time.Millisecond)
		close(rt.release)
	}()
	drain()
	<-shutdown

	select {
	case r := <-res:
		if r.err != nil {
			t.Fatalf("in-flight StartLoop failed across the drain: %v — Shutdown must let an accepted request finish", r.err)
		}
		if r.id != "loop-drain" {
			t.Fatalf("loop_id = %q, want loop-drain", r.id)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("in-flight StartLoop never returned")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v, want nil", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("serve did not return after the drain")
	}
}

// TestDrainReturnsNilOnContextShutdown pins that a ctx-driven shutdown is reported
// as a clean exit, not a serve error: runLoopServeNet maps a non-nil return to exit
// code 1, so an ErrServerClosed leaking out would make every SIGTERM look like a
// crash to the orchestrator restarting the pod.
func TestDrainReturnsNilOnContextShutdown(t *testing.T) {
	_, done, drain := startDrainServer(t, &netFakeRuntime{startID: "loop-nil"}, &pingStore{}, serveNetOptions{drainTimeout: time.Second})
	drain()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v on a ctx-driven shutdown, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return after cancel")
	}
}

// TestDrainIsBoundedByTheTimeout is the budget's mutation-resistant assertion, and
// it is deliberately NOT "does serve() return quickly" — Shutdown closes the
// listener up front, so Serve returns ErrServerClosed promptly whether or not the
// budget is honoured, and a test watching only that return is green even with the
// drain unbounded.
//
// What the budget actually bounds is the CONNECTION. Shutdown waits for in-flight
// requests and does not forcibly end a streaming or hijacked response, so a handler
// that never finishes keeps its connection (and its goroutine) alive for as long as
// the process lives. The unconditional srv.Close() after Shutdown is what severs it,
// and that is what a container runtime's grace period depends on: past the budget
// the process must be able to exit rather than sit until SIGKILL. So the assertion
// is that the STUCK CLIENT is cut loose, shortly after the budget and long before
// the handler's own 30s escape hatch would release it.
func TestDrainIsBoundedByTheTimeout(t *testing.T) {
	rt := newBlockingRuntime()
	const budget = 200 * time.Millisecond
	base, done, drain := startDrainServer(t, rt, &pingStore{}, serveNetOptions{drainTimeout: budget})

	// Never released: this handler outlives the budget by a wide margin (30s).
	stuck := make(chan error, 1)
	go func() {
		_, err := bearerClient(base, "tkn").StartLoop(context.Background(),
			connect.NewRequest(&loopv1.StartLoopRequest{Task: "stuck", Model: "cloud/x"}))
		stuck <- err
	}()
	rt.awaitEntered(t)

	start := time.Now()
	drain()

	// The connection is severed once the budget expires. Without the unconditional
	// Close this waits out the blocking handler's full 30s and fails here.
	select {
	case err := <-stuck:
		if err == nil {
			t.Fatal("the handler blocked past the drain budget yet its call succeeded — the budget was not enforced")
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Fatalf("the stuck connection survived %s with a %s budget — the unconditional srv.Close() after Shutdown is missing, so a stuck handler can outlive the grace period", elapsed, budget)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("a handler blocking past the %s drain budget was never cut loose — the unconditional srv.Close() after Shutdown is missing, so a stuck stream outlives the budget", budget)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return after the drain")
	}
	close(rt.release)
}

// TestServeNetContextInstallsSIGTERM pins the signal set itself. SIGTERM is what a
// container runtime sends first (Kubernetes: SIGTERM, wait
// terminationGracePeriodSeconds, SIGKILL); with only os.Interrupt registered the Go
// default applies and the process dies immediately, making every drain property
// above unreachable in the one environment that needs them. The registration is
// asserted by SENDING the signal to this process and observing the context close —
// the production seam, not a fake.
func TestServeNetContextInstallsSIGTERM(t *testing.T) {
	// A guard registration held for the duration of this test. Without it, a seam
	// that does NOT register SIGTERM would let the Go default action terminate the
	// whole test binary instead of failing this one test — the RED signal has to be
	// a failure, not a dead process.
	guard := make(chan os.Signal, 1)
	signal.Notify(guard, syscall.SIGTERM)
	defer signal.Stop(guard)

	ctx, stop := serveNetContext()
	defer stop()

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("raise SIGTERM: %v", err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("serveNetContext's context did not close on SIGTERM — signal.NotifyContext must register syscall.SIGTERM, or a container runtime's first signal kills the process outright")
	}
}

// TestLoopServeNetDrainTimeoutFlag proves --drain-timeout is a real flag on the
// subcommand's flag set (the Helm chart derives terminationGracePeriodSeconds from
// it, so both the name and the default are a published contract) and that it is
// advertised with a 20s default.
func TestLoopServeNetDrainTimeoutFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// --help exits 0 via the flag package's ErrHelp path and prints the flag set.
	var out, errb strings.Builder
	_ = run([]string{"loop-serve-net", "--help"}, &out, &errb)
	usage := errb.String() + out.String()
	if !strings.Contains(usage, "drain-timeout") {
		t.Fatalf("loop-serve-net usage does not mention -drain-timeout:\n%s", usage)
	}
	if !strings.Contains(usage, "20s") {
		t.Fatalf("loop-serve-net usage does not advertise the 20s drain default:\n%s", usage)
	}
	if defaultDrainTimeout != 20*time.Second {
		t.Fatalf("defaultDrainTimeout = %s, want 20s — the chart's terminationGracePeriodSeconds is drainTimeout+10", defaultDrainTimeout)
	}

	// An unparseable value is rejected rather than silently defaulted.
	var o2, e2 strings.Builder
	if code := run([]string{"loop-serve-net", "--drain-timeout", "not-a-duration"}, &o2, &e2); code != 2 {
		t.Fatalf("bad --drain-timeout exit = %d, want 2; stderr:\n%s", code, e2.String())
	}
}

// TestDrainZeroTimeoutFallsBackToTheDefault guards the one way an options struct can
// silently break a caller: every pre-existing serveNetObserved/serveNet caller
// passes a ZERO drainTimeout, and a zero budget read literally would turn their
// graceful shutdown into an immediate Close. It must mean "the default" instead.
func TestDrainZeroTimeoutFallsBackToTheDefault(t *testing.T) {
	rt := newBlockingRuntime()
	base, done, drain := startDrainServer(t, rt, &pingStore{}, serveNetOptions{}) // zero drainTimeout

	res := make(chan error, 1)
	go func() {
		_, err := bearerClient(base, "tkn").StartLoop(context.Background(),
			connect.NewRequest(&loopv1.StartLoopRequest{Task: "hi", Model: "cloud/x"}))
		res <- err
	}()
	rt.awaitEntered(t)

	drain()
	// A literal zero budget would make Shutdown give up instantly and the following
	// Close would sever this request; the default budget lets it finish.
	time.Sleep(100 * time.Millisecond)
	close(rt.release)

	select {
	case err := <-res:
		if err != nil {
			t.Fatalf("in-flight call with a zero (⇒ default) drain budget failed: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("in-flight call never returned")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v, want nil", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("serve did not return")
	}
}
