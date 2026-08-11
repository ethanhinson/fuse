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

	"github.com/coder/websocket"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/loopserver"
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
// networked (WebSocket + HTTP) loop-control server. It exposes the SAME policy-free
// multi-loop runtime.Runtime that binding #2 (loop-server) serves over stdio — the
// composition root (buildLoopServerRuntimeDeps + runtime.New) is REUSED verbatim, not
// re-wired — over two network transports:
//
//   - WS /ws            — the full loop.* JSON-RPC session (loop.start / loop.send /
//     loop.observe + server-push loop.event tail), one session per connection.
//   - GET /loops/{id}/events — the thin stateless HTTP replay endpoint (Attach
//     catch-up); read-only, no live tail, no connection state.
//
// Its documented policy is AUTO-APPROVE (permissions.AlwaysApprove): there is no human
// on a TTY to gate tool calls over the network, exactly as binding #2. That is THIS
// binding's CHOICE, wired here at the composition root (ADR-0028 stance: the
// binding owns policy, the Runtime seam stays policy-free), not a property of the
// Runtime seam.
func runLoopServeNet(args []string, cfg config.Config, reg *model.Registry, _ io.Writer, stderr io.Writer) int {
	fs := flag.NewFlagSet("loop-serve-net", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "127.0.0.1:8787", "TCP address to serve the WS+HTTP loop-control endpoints on")
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

// serveNet mounts the WS accept handler and the HTTP replay handler on one
// http.ServeMux and serves them over ln until ctx is cancelled, then shuts the server
// down. It is the testable core runLoopServeNet calls: a test starts it on a
// 127.0.0.1:0 listener with a fake Runtime, learns the chosen port off the listener,
// and cancels ctx to shut down — no real signal, no live model.
//
// The mux mounts two routes over the SAME runtime.Runtime:
//   - /ws                 — WS accept -> loopserver.ServeWS (full loop.* session).
//   - GET /loops/{id}/events — delegated to loopserver.NewReplayHandler (self-contained
//     for its own pattern; the replay handler owns stateless Attach catch-up).
func serveNet(ctx context.Context, ln net.Listener, rt runtime.Runtime) error {
	mux := http.NewServeMux()

	// WS accept: upgrade, then run one full loop.* session over the connection. ServeWS
	// closes the connection when it returns; the request context is derived from the
	// server's base context (below), so a shutdown cancels in-flight sessions.
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			// Accept already wrote the failure response.
			return
		}
		_ = loopserver.ServeWS(r.Context(), conn, rt)
	})

	// GET /loops/{id}/events — the self-contained stateless replay handler owns its own
	// pattern registration, so delegate the whole subtree to it.
	mux.Handle("/loops/", loopserver.NewReplayHandler(rt))

	srv := &http.Server{
		Handler: mux,
		// BaseContext threads the serve/shutdown ctx into every request (incl. the WS
		// session's r.Context()), so a ctx cancel tears down in-flight observe pumps.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	// Shut the server down when ctx is cancelled (signal or test). Close() unblocks
	// Serve immediately; in-flight WS sessions observe the cancelled BaseContext and
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
