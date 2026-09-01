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
	"syscall"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
	"github.com/ethanhinson/fuse/internal/loopconnect"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/runtime"
	"github.com/ethanhinson/fuse/internal/skills"
)

// devToken is the built-in bearer token synthesized when loop_server.auth is
// empty in config. It keeps local `loop-serve-net` usable WITHOUT running
// unauthenticated: every request still MUST present this token, and it maps to
// the _default tenant with the "dev" subject. Configure real tokens under
// loop_server.auth in ~/.fuse/config.yml for any shared or deployed server.
const devToken = "fuse-dev-token"

// buildLoopVerifier constructs the loopauth.Verifier for binding #3 from
// cfg.LoopServer.Auth (a static token→principal map). Auth is REQUIRED for the
// networked binding, so the verifier is NEVER empty: when config supplies no
// tokens we synthesize a single built-in dev token (→ _default tenant) so local
// use keeps working while every request still authenticates. usedDefault reports
// whether the dev fallback was synthesized so the caller can log it loudly.
//
// Posture note (change 0049): this is fail-USABLE, not fail-open — the server
// never accepts an unauthenticated request. We deliberately do NOT hard-fail on
// an empty config because that would break `fuse loop-serve-net` with a bare
// zero-config for local development (and the dispatch smoke test that runs it),
// yet a bearer token is still mandatory on the wire.
func buildLoopVerifier(cfg config.Config) (loopauth.Verifier, bool) {
	tokens := map[string]loopauth.Principal{}
	for _, a := range cfg.LoopServer.Auth {
		if a.Token == "" {
			continue // an entry with no token is unusable; skip it
		}
		tokens[a.Token] = loopauth.Principal{
			Tenant:                event.TenantID(a.Tenant), // "" normalizes to _default at the store
			Subject:               a.Subject,
			ObservabilityOperator: a.ObservabilityOperator,
		}
	}
	usedDefault := false
	if len(tokens) == 0 {
		tokens[devToken] = loopauth.Principal{Tenant: event.DefaultTenant, Subject: "dev", ObservabilityOperator: true}
		usedDefault = true
	}
	return loopauth.NewStaticVerifier(tokens), usedDefault
}

// loopLeaseTTL resolves the owner-liveness lease TTL for binding #3 from
// cfg.LoopServer.LeaseTTL. An empty/unset value returns 0 so runtime.Deps applies
// its own built-in default; a parse error can't reach here because Config.Validate
// rejects a malformed lease_ttl at startup, but we defensively return 0 anyway.
func loopLeaseTTL(cfg config.Config) time.Duration {
	if cfg.LoopServer.LeaseTTL == "" {
		return 0
	}
	d, err := time.ParseDuration(cfg.LoopServer.LeaseTTL)
	if err != nil || d < 0 {
		return 0
	}
	return d
}

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

// newLoopServeNetObservability is the composition seam for the network command.
// Production uses newObservability; acceptance tests replace it with an in-memory
// trace exporter while still exercising the CLI's real configuration and wiring.
var newLoopServeNetObservability = newObservability

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
//
// AUTH (change 0049): unlike the stdio + local CLI bindings, EVERY request to this
// networked binding MUST present an `Authorization: Bearer <token>` credential. The
// bearer token→principal map (and the owner-liveness lease TTL) come from the trusted
// ~/.fuse/config.yml `loop_server:` block — a credential surface, so a repo-plantable
// .fuse.local.yml cannot mint tokens. Identity + authorization live at the Connect
// edge (the auth interceptor + the registry-backed handler), never in the runtime
// seam (ADR-0030). When `loop_server.auth` is empty a single built-in dev token
// (→ _default tenant) is synthesized so local use stays usable — the server still
// authenticates every request; it never runs open. Config knobs:
//
//	loop_server:
//	  lease_ttl: "30s"          # owner-liveness lease; empty ⇒ runtime default (30s)
//	  auth:
//	    - token: <bearer-token> # required per request
//	      tenant: <tenant-id>   # isolation boundary; empty ⇒ _default
//	      subject: <subject>    # recorded as a loop's owner
func runLoopServeNet(args []string, cfg config.Config, reg *model.Registry, stdout io.Writer, stderr io.Writer) int {
	fs := flag.NewFlagSet("loop-serve-net", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "127.0.0.1:8787", "TCP address to serve the Connect/protobuf loop-control endpoints on")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: fuse loop-serve-net [--addr host:port]")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Serves the networked Connect/protobuf (fuse.loop.v1) loop-control endpoints.")
		fmt.Fprintln(stderr, "EVERY request must present an `Authorization: Bearer <token>` credential.")
		fmt.Fprintln(stderr, "Configure the token→principal map and lease TTL under `loop_server:` in")
		fmt.Fprintln(stderr, "~/.fuse/config.yml (a trusted, credential-bearing surface):")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "  loop_server:")
		fmt.Fprintln(stderr, "    lease_ttl: \"30s\"           # owner-liveness lease; empty ⇒ 30s default")
		fmt.Fprintln(stderr, "    auth:")
		fmt.Fprintln(stderr, "      - token: <bearer-token>  # required per request")
		fmt.Fprintln(stderr, "        tenant: <tenant-id>    # isolation boundary; empty ⇒ _default")
		fmt.Fprintln(stderr, "        subject: <subject>     # recorded as a loop's owner")
		fmt.Fprintln(stderr, "        observability_operator: false # required for global logging reload/reopen")
		fmt.Fprintln(stderr)
		fmt.Fprintf(stderr, "With no loop_server.auth configured, a built-in dev token %q (tenant _default)\n", devToken)
		fmt.Fprintln(stderr, "is used so local development works; a bearer token is still required on the wire.")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "flags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Auto-approve is THIS binding's policy (documented, ADR-0028): headless networked
	// loop control has no human on a TTY. It is not a property of the Runtime seam.
	approve := permissions.AlwaysApprove
	markPolicyApproval()

	// Load skills + tools EXACTLY as runLoopServer does so the two bindings serve an
	// identically-wired Runtime.
	skillSet, serr := skills.LoadWithEmbedded(skills.DefaultDirs())
	if serr != nil {
		fmt.Fprintf(stderr, "skills error: %v\n", serr)
		return 1
	}
	systemBlock := skillSet.SystemPromptBlock() + spawnAgentBlock
	// Sandbox substrate (ADR-0044, change 0063), resolved ONCE. hosted=TRUE: this
	// binding serves remote principals over the network, so no local file may
	// authorize running their commands uncontained on this host.
	sb, closeEgress := newSandboxService(true, stderr)
	defer closeEgress()
	toolReg := defaultToolRegistry(sb, cfg.Research, skillSet.Lookup)

	// REUSE the shared composition root — the exact deps wiring binding #2 uses. Do not
	// re-wire it here. Thread the owner-liveness lease TTL (change 0049) into the deps so
	// the runtime's heartbeat renewer + reap/re-own operate on the configured TTL.
	// Identity + authorization live at the Connect edge (ADR-0030): build the bearer-
	// token Verifier from config and hand the edge the durable registry (deps.Registry)
	// so it can authorize per-loop ownership. The runtime seam never learns any of this.
	verifier, usedDefault := buildLoopVerifier(cfg)
	if usedDefault {
		fmt.Fprintf(stderr, "loop-serve-net: no loop_server.auth configured — using the built-in dev token %q (tenant %q); set loop_server.auth in ~/.fuse/config.yml for a shared server\n",
			devToken, event.DefaultTenant)
	}

	obs, err := newLoopServeNetObservability(context.Background(), cfg, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "loop-serve-net: observability: %v\n", err)
		return 1
	}
	shutdownObservability := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := obs.Close(ctx); err != nil {
			fmt.Fprintf(stderr, "loop-serve-net: observability shutdown: %v\n", err)
		}
	}
	defer shutdownObservability()
	deps := buildLoopServerRuntimeDepsWithObserver(sb, cfg, reg, reg.Default, toolReg, systemBlock, approve, sessionRateGate(cfg), obs.observer)
	deps.LeaseTTL = loopLeaseTTL(cfg)
	if obs.projection != nil && deps.DurableStore != nil {
		store, ok := deps.DurableStore.(event.CommittedDurableStore)
		if !ok {
			fmt.Fprintln(stderr, "loop-serve-net: observability requires a durable store that returns committed event envelopes")
			return 1
		}
		deps.DurableStore = projectingDurableStore{CommittedDurableStore: store, projection: obs.projection}
	}
	rt := runtime.New(deps)

	ln, err := netListen("tcp", *addr)
	if err != nil {
		fmt.Fprintf(stderr, "loop-serve-net: listen %s: %v\n", *addr, err)
		return 1
	}

	ctx, stop := serveNetContext()
	defer stop()
	if err := obs.startMetricsEndpoint(ctx, verifier); err != nil {
		_ = ln.Close()
		fmt.Fprintf(stderr, "loop-serve-net: %v\n", err)
		return 1
	}
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				obs.handleSIGHUP(ctx, cfg.Observability.Logging, stderr)
			}
		}
	}()

	if err := serveNetObserved(ctx, ln, rt, verifier, deps.Registry, obs); err != nil {
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
//
// Auth (change 0049): the auth interceptor authenticates the bearer token on every
// unary + streaming request (missing/invalid → CodeUnauthenticated) against verifier,
// and the handler is given the durable registry (WithRegistry) so it authorizes per-
// loop ownership at the edge — the runtime seam imports no auth (ADR-0030). A nil
// verifier or nil registry degrades gracefully (interceptor omitted / authz skipped),
// which the pure-transport E2E test relies on for a subset of its assertions; the
// production runLoopServeNet always supplies both.
func serveNet(ctx context.Context, ln net.Listener, rt runtime.Runtime, verifier loopauth.Verifier, registry event.LoopRegistry) error {
	return serveNetObserved(ctx, ln, rt, verifier, registry, nil)
}

func serveNetObserved(ctx context.Context, ln net.Listener, rt runtime.Runtime, verifier loopauth.Verifier, registry event.LoopRegistry, obs *observabilityService) error {
	mux := http.NewServeMux()

	handler := loopconnect.NewHandler(rt).WithBaseContext(ctx).WithRegistry(registry)
	if obs != nil {
		handler.WithObserver(obs.observer)
	}
	var opts []connect.HandlerOption
	if verifier != nil {
		opts = append(opts, connect.WithInterceptors(loopconnect.NewAuthInterceptor(verifier)))
	}
	path, connectHandler := loopv1connect.NewLoopServiceHandler(handler, opts...)
	mux.Handle(path, connectHandler)
	if obs != nil {
		if obs.metrics != nil && obs.cfg.Metrics.Bind == "" {
			metrics := obs.metrics.Handler()
			if obs.cfg.Metrics.Access == "authenticated" {
				metrics = authenticatedHTTP(verifier, metrics)
			}
			mux.Handle(obs.cfg.Metrics.Path, metrics)
		}
		if obs.levels != nil {
			mux.Handle(observabilityAdminPath, obs.adminHandler(verifier))
			mux.Handle(observabilityReloadPath, obs.reloadHandler(verifier))
			mux.Handle(observabilityReopenPath, obs.reopenHandler(verifier))
		}
	}

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
