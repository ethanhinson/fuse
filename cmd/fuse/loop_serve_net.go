package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/loopconnect"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/runtime"
	"github.com/ethanhinson/fuse/internal/skills"
)

// netListen is the seam runLoopServeNet binds its listener through. It is net.Listen
// in production; a test swaps it for a 127.0.0.1:0 ephemeral listener so it can learn
// the chosen port without racing the default :8787. Mirrors the stdinForLoopServer
// override pattern in loop_server.go.
var netListen = net.Listen

// serveNetContext is the seam runLoopServeNet derives its serve/shutdown context
// from. In production it installs signal.NotifyContext(os.Interrupt) so Ctrl-C tears
// the servers down cleanly; a test swaps it for an already-cancelled context so the
// dispatch/registration test exits without a real signal.
var serveNetContext = func() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}

// runLoopServeNet implements the `fuse loop-serve-net` subcommand (binding #3): the
// networked Connect/protobuf (fuse.loop.v1) loop-control server — successor to the
// JSON-over-WebSocket wire (#48, ADR-0032 superseded). It exposes the SAME policy-free
// multi-loop runtime.Runtime that binding #2 (loop-server) serves over stdio — the
// composition root (buildLoopServerRuntimeDeps + runtime.New) is REUSED verbatim, not
// re-wired — over the Connect service:
//
//   - StartLoop / Send (unary) and Observe (server-streaming history-then-live) at
//     /fuse.loop.v1.LoopService/*, browser-reachable over HTTP/2 with no proxy.
//     Observe(from_seq) subsumes the WS-era live tail AND the HTTP replay/Attach
//     catch-up, so there is no separate /ws or /loops/ route.
//
// The handler is served over h2c (HTTP/2 cleartext) so gRPC and gRPC-Web clients work
// over a plain TCP listener; Connect and Connect's HTTP/1.1 fallback also work.
//
// Its documented policy is AUTO-APPROVE (permissions.AlwaysApprove): there is no human
// on a TTY to gate tool calls over the network, exactly as binding #2. That is THIS
// binding's CHOICE, wired here at the composition root (ADR-0028 stance: the
// binding owns policy, the Runtime seam stays policy-free), not a property of the
// Runtime seam.
func runLoopServeNet(args []string, cfg config.Config, reg *model.Registry, _ io.Writer, stderr io.Writer) int {
	fs := flag.NewFlagSet("loop-serve-net", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "127.0.0.1:8787", "TCP address to serve the Connect/protobuf loop-control endpoints on")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Auto-approve is THIS binding's policy (documented, ADR-0028): headless networked
	// loop control has no human on a TTY. It is not a property of the Runtime seam.
	approve := permissions.AlwaysApprove

	// Load skills + tools EXACTLY as runLoopServer does so the two bindings serve an
	// identically-wired Runtime.
	skillSet, serr := skills.LoadWithEmbedded(skills.DefaultDirs())
	if serr != nil {
		fmt.Fprintf(stderr, "skills error: %v\n", serr)
		return 1
	}
	systemBlock := skillSet.SystemPromptBlock() + spawnAgentBlock
	toolReg := defaultToolRegistry(cfg.Research, skillSet.Lookup)

	// REUSE the shared composition root — the exact deps wiring binding #2 uses. Do not
	// re-wire it here.
	deps := buildLoopServerRuntimeDeps(cfg, reg, reg.Default, toolReg, systemBlock, approve, sessionRateGate(cfg))
	rt := runtime.New(deps)

	ln, err := netListen("tcp", *addr)
	if err != nil {
		fmt.Fprintf(stderr, "loop-serve-net: listen %s: %v\n", *addr, err)
		return 1
	}

	ctx, stop := serveNetContext()
	defer stop()

	if err := serveNet(ctx, ln, rt); err != nil {
		fmt.Fprintf(stderr, "loop-serve-net: %v\n", err)
		return 1
	}
	return 0
}

// serveNet mounts the connect-go LoopService handler on one http.ServeMux and serves
// it over ln (h2c) until ctx is cancelled, then shuts the server down. It is the
// testable core runLoopServeNet calls: a test starts it on a 127.0.0.1:0 listener with
// a fake Runtime, learns the chosen port off the listener, and cancels ctx to shut
// down — no real signal, no live model.
//
// The mux mounts the single Connect service over the SAME runtime.Runtime. The handler
// is given the serve/shutdown ctx as its loop-lifetime context (WithBaseContext) so a
// unary StartLoop's returning request context does NOT kill the loop, but a server
// shutdown does.
func serveNet(ctx context.Context, ln net.Listener, rt runtime.Runtime) error {
	mux := http.NewServeMux()

	handler := loopconnect.NewHandler(rt).WithBaseContext(ctx)
	path, connectHandler := loopv1connect.NewLoopServiceHandler(handler)
	mux.Handle(path, connectHandler)

	srv := &http.Server{
		// h2c lets gRPC / gRPC-Web / Connect clients negotiate HTTP/2 over cleartext on
		// this plain TCP listener; Connect's HTTP/1.1 fallback also works.
		Handler: h2c.NewHandler(mux, &http2.Server{}),
		// BaseContext threads the serve/shutdown ctx into every request, so a ctx cancel
		// tears down in-flight Observe streams (their handler selects on ctx.Done()).
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	// Shut the server down when ctx is cancelled (signal or test). Close() unblocks
	// Serve immediately; in-flight Observe streams observe the cancelled BaseContext and
	// return, releasing their subscriptions.
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	err := srv.Serve(ln)
	// A ctx-driven Close is a clean shutdown, not a serve error.
	if err == http.ErrServerClosed || ctx.Err() != nil {
		return nil
	}
	return err
}
