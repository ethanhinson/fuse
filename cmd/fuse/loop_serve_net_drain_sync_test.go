package main

import (
	"context"
	"net"
	"testing"
	"time"

	"connectrpc.com/connect"

	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
)

// TestDrainCompletesBeforeServeReturns is the regression test for a drain that was a
// no-op in production.
//
// The drain runs in a goroutine that NOTHING waited on. srv.Shutdown closes the
// listeners as its very first act, so the blocked srv.Serve(ln) returned
// http.ErrServerClosed immediately and serveNetWithOptions returned nil while
// Shutdown was still inside its budget waiting on in-flight requests. Every caller
// above it then unwound: runLoopServeNet returned 0 and main called
// os.Exit(run(...)) (cmd/fuse/main.go), and os.Exit does not wait for goroutines.
// The in-flight unary calls the drain exists to protect were killed with the
// process — the drain was observable only in a test binary that happened to stay
// alive past the return.
//
// Every other test in this package runs serveNetWithOptions in a goroutine whose
// process outlives it, so none of them can see this: the function's RETURN never
// mattered. This one makes it matter. A handler is held in flight across the
// cancellation and released a known interval AFTER the drain has begun; the
// assertion is that serveNetWithOptions had not yet returned at that moment.
//
// Before the fix the serve return lands within microseconds of the cancel, long
// before the handler is released, and this fails on the ordering assertion. After
// the fix the return blocks on the drain's completion channel.
func TestDrainCompletesBeforeServeReturns(t *testing.T) {
	rt := newBlockingRuntime()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// draining is closed from the afterDraining seam, so releaseAfter is measured
	// from INSIDE the drain window rather than from the cancel — the handler must
	// finish while Shutdown is genuinely waiting on it.
	draining := make(chan struct{})
	// The budget comfortably exceeds releaseAfter: otherwise Shutdown's own deadline,
	// not the missing wait, would be what ends the drain and the test would pass for
	// the wrong reason.
	const releaseAfter = 750 * time.Millisecond
	opts := serveNetOptions{
		drainTimeout:  10 * time.Second,
		afterDraining: func() { close(draining) },
	}

	// handlerDoneAt and serveReturnedAt are each written once, and read only after
	// both writes are ordered behind a channel receive, so they are race-free.
	var handlerDoneAt, serveReturnedAt time.Time

	served := make(chan error, 1)
	go func() {
		err := serveNetWithOptions(ctx, ln, rt, testVerifier(), nil, &pingStore{}, nil, opts)
		serveReturnedAt = time.Now()
		served <- err
	}()

	base := "http://" + ln.Addr().String()
	waitServing(t, base+"/healthz")

	res := make(chan error, 1)
	go func() {
		_, err := bearerClient(base, "tkn").StartLoop(context.Background(),
			connect.NewRequest(&loopv1.StartLoopRequest{Task: "inflight", Model: "cloud/x"}))
		res <- err
	}()
	rt.awaitEntered(t)

	released := make(chan struct{})
	go func() {
		defer close(released)
		<-draining
		time.Sleep(releaseAfter)
		handlerDoneAt = time.Now()
		close(rt.release)
	}()
	cancel()

	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serveNetWithOptions returned %v, want nil", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("serveNetWithOptions never returned")
	}
	<-released

	if serveReturnedAt.Before(handlerDoneAt) {
		t.Fatalf("serveNetWithOptions returned %s BEFORE the in-flight handler finished — "+
			"the drain goroutine is spawned and never waited on, so in production main's "+
			"os.Exit kills the drain and the graceful shutdown is a no-op",
			handlerDoneAt.Sub(serveReturnedAt))
	}
	if err := <-res; err != nil {
		t.Fatalf("the in-flight call the drain was protecting failed: %v", err)
	}
}
