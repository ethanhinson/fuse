// Command statesrv is a scripted, mode-switchable Connect server for the @fuse/sdk
// connection-state / error-classification node tests (change 0056). It mounts the real
// loopconnect handler over a scripted runtime.Runtime double whose Observe behavior is
// selected by the MODE env var, prints a single `URL <url>` line, and blocks until killed
// (SIGINT/SIGTERM). It is a sibling of test/server/main.go (the no-loss/no-dup wire test),
// kept separate so that test's forced subscribe->replay overlap is not perturbed.
//
// MODEs:
//
//   reopen   — the FIRST Observe delivers seqs 1,2 then CLOSES its live channel (a clean
//              stream end). The SDK must reconnect; the SECOND+ Observe replays [1,2] from
//              history and delivers the terminal seq 3 (loop.parked) on the live tail. This
//              drives the connection-state transitions connecting→live (first frame) then
//              reconnecting→live (across the re-open), and completion.
//
//   terminal — Observe returns a TERMINAL Connect error immediately. The code is taken from
//              the TERMINAL_CODE env var (unauthenticated | permission_denied | not_found |
//              failed_precondition). The SDK must STOP reconnecting and surface a typed
//              terminal error (no hot-loop).
//
//   transient-then-done — like reopen, but proves a transient drop reconnects: the first
//              Observe closes after seq 1; the second replays [1] and delivers 2,3 (parked).
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopconnect"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/runtime"
)

// stateRuntime is a scripted runtime.Runtime double whose Observe behavior depends on the
// configured mode and the observe-call count (so a reconnect sees different behavior than
// the first open).
type stateRuntime struct {
	mode string

	mu       sync.Mutex
	hist     []event.Event
	observeN int
}

func (r *stateRuntime) StartLoop(ctx context.Context, cfg runtime.LoopConfig) (runtime.LoopHandle, error) {
	return stateHandle{id: "loop-state"}, nil
}

func (r *stateRuntime) Send(ctx context.Context, tenant event.TenantID, loopID, input string) error {
	return nil
}

func (r *stateRuntime) Spawn(ctx context.Context, loopID string, opts runtime.SpawnOpts) (runtime.SpawnHandle, error) {
	return nil, nil
}

// Observe drives the scripted history/live behavior for the current mode. It hands back a
// fresh live channel per call (closing it ends that stream), and grows durable history so
// a reconnect's Attach(fromSeq) replays what earlier passes delivered.
func (r *stateRuntime) Observe(ctx context.Context, tenant event.TenantID, loopID string) (<-chan event.Event, func(), error) {
	r.mu.Lock()
	r.observeN++
	n := r.observeN
	r.mu.Unlock()

	live := make(chan event.Event, 8)

	switch r.mode {
	case "reopen":
		if n == 1 {
			// First pass: land 1,2 in history+live, then CLOSE (clean end → reconnect).
			r.appendHist(event.Event{Seq: 1, Kind: event.KindTurnStart})
			r.appendHist(event.Event{Seq: 2, Kind: event.KindModelCallStart})
			live <- event.Event{Seq: 1, Kind: event.KindTurnStart}
			live <- event.Event{Seq: 2, Kind: event.KindModelCallStart}
			close(live)
		} else {
			// Reconnect: history [1,2] replays via Attach; deliver the terminal parked
			// event 3 on the live tail, then keep the stream open (park).
			r.appendHist(event.Event{Seq: 3, Kind: event.KindLoopParked})
			live <- event.Event{Seq: 3, Kind: event.KindLoopParked}
		}
	case "transient-then-done":
		if n == 1 {
			r.appendHist(event.Event{Seq: 1, Kind: event.KindTurnStart})
			live <- event.Event{Seq: 1, Kind: event.KindTurnStart}
			close(live)
		} else {
			r.appendHist(event.Event{Seq: 2, Kind: event.KindModelCallStart})
			r.appendHist(event.Event{Seq: 3, Kind: event.KindLoopParked})
			live <- event.Event{Seq: 2, Kind: event.KindModelCallStart}
			live <- event.Event{Seq: 3, Kind: event.KindLoopParked}
		}
	}

	return live, func() {}, nil
}

func (r *stateRuntime) Attach(ctx context.Context, tenant event.TenantID, loopID string, from event.Seq) ([]event.Event, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []event.Event
	for _, e := range r.hist {
		if e.Seq > from {
			out = append(out, e)
		}
	}
	return out, nil
}

func (r *stateRuntime) appendHist(e event.Event) {
	r.mu.Lock()
	r.hist = append(r.hist, e)
	r.mu.Unlock()
}

type stateHandle struct{ id string }

func (h stateHandle) ID() string                     { return h.id }
func (h stateHandle) Wait() ([]model.Message, error) { return nil, nil }

// terminalHandler wraps a LoopServiceHandler and overrides Observe to return a fixed
// terminal Connect error, so the SDK sees the terminal code on the WIRE (the real handler
// remaps a runtime Observe error to CodeInternal, which is transient by D2 — we need the
// terminal code itself to reach the client to exercise the stop-reconnect path).
type terminalHandler struct {
	loopv1connect.LoopServiceHandler
	code connect.Code
}

func (t terminalHandler) Observe(ctx context.Context, req *connect.Request[loopv1.ObserveRequest], stream *connect.ServerStream[loopv1.ObserveEvent]) error {
	return connect.NewError(t.code, errors.New("statesrv: scripted terminal error"))
}

func terminalCodeFromEnv() connect.Code {
	switch os.Getenv("TERMINAL_CODE") {
	case "permission_denied":
		return connect.CodePermissionDenied
	case "not_found":
		return connect.CodeNotFound
	case "failed_precondition":
		return connect.CodeFailedPrecondition
	default:
		return connect.CodeUnauthenticated
	}
}

func main() {
	mode := os.Getenv("MODE")
	if mode == "" {
		mode = "reopen"
	}
	rt := &stateRuntime{mode: mode}

	var svc loopv1connect.LoopServiceHandler = loopconnect.NewHandler(rt).WithKeepalive(time.Hour)
	if mode == "terminal" {
		// Wrap so Observe returns the terminal code on the wire (see terminalHandler).
		svc = terminalHandler{LoopServiceHandler: svc, code: terminalCodeFromEnv()}
	}
	path, h := loopv1connect.NewLoopServiceHandler(svc)

	mux := http.NewServeMux()
	mux.Handle(path, h)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}
	srv := &http.Server{Handler: mux}

	fmt.Printf("URL http://%s\n", ln.Addr().String())
	os.Stdout.Sync()

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		os.Exit(0)
	}()

	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		os.Exit(1)
	}
}
