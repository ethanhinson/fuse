// Command server is a tiny test-only Connect server for the @fuse/sdk node test.
//
// It mounts loopv1connect.NewLoopServiceHandler over a scripted fakeRuntime (a
// runtime.Runtime double, mirroring internal/loopconnect/handler_test.go's) on an
// httptest-style server bound to an ephemeral loopback port, prints the chosen base URL
// on a single `URL <url>` line to stdout, then blocks until killed. The node test
// (sdk/ts/test/remote.test.ts) spawns `go run ./test/server`, reads that URL, and drives
// the @fuse/sdk client against this REAL Connect wire.
//
// The fake scripts a durable history (seqs 1,2) and forces an overlap event (seq 3) into
// the server's subscribe->replay window on the FIRST Observe (observeGate), so a naive
// replay would double-deliver it. That is how the node test proves no-loss/no-dup from
// TS across the subscribe->replay gap — stronger than a one-frame reconnect check.
//
// This proves the WIRE from TS over a fake backend. The real-engine end-to-end for the
// identical (language-agnostic) wire is covered by the Go sdk/fuse/acceptance_test.go
// real-loop test; see sdk/ts/README.md.
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopconnect"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/runtime"
)

// fakeRuntime is a scripted runtime.Runtime double: a durable history slice plus a live
// channel, so Observe(fromSeq) exercises subscribe-before-replay + dedup-at-watermark.
type fakeRuntime struct {
	mu   sync.Mutex
	hist []event.Event

	startID string

	sawSendLoop string
	sawSendIn   string

	// live is the channel Observe hands back for the live tail.
	live chan event.Event

	// observeGate, if non-nil, fires inside Observe right after it hands back the live
	// channel but BEFORE the handler calls Attach — it lands an append in the
	// subscribe->replay window deterministically (the overlap event).
	observeGate func()
}

func (f *fakeRuntime) StartLoop(ctx context.Context, cfg runtime.LoopConfig) (runtime.LoopHandle, error) {
	return fakeHandle{id: f.startID}, nil
}

func (f *fakeRuntime) Send(ctx context.Context, tenant event.TenantID, loopID, input string) error {
	f.mu.Lock()
	f.sawSendLoop, f.sawSendIn = loopID, input
	f.mu.Unlock()
	return nil
}

func (f *fakeRuntime) Spawn(ctx context.Context, loopID string, opts runtime.SpawnOpts) (runtime.SpawnHandle, error) {
	return nil, nil
}

// Resume is a no-op stub for this fake: it returns a handle for the same loop id so
// the handler's resume path (change 0054) can be exercised without a real runtime.
func (f *fakeRuntime) Resume(ctx context.Context, tenant event.TenantID, loopID string) (runtime.LoopHandle, error) {
	return fakeHandle{id: loopID}, nil
}

func (f *fakeRuntime) Observe(ctx context.Context, tenant event.TenantID, loopID string) (<-chan event.Event, func(), error) {
	if f.observeGate != nil {
		f.observeGate()
	}
	return f.live, func() {}, nil
}

func (f *fakeRuntime) Attach(ctx context.Context, tenant event.TenantID, loopID string, from event.Seq) ([]event.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []event.Event
	for _, e := range f.hist {
		if e.Seq > from {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeRuntime) appendHist(e event.Event) {
	f.mu.Lock()
	f.hist = append(f.hist, e)
	f.mu.Unlock()
}

type fakeHandle struct{ id string }

func (h fakeHandle) ID() string                     { return h.id }
func (h fakeHandle) Wait() ([]model.Message, error) { return nil, nil }

func main() {
	live := make(chan event.Event, 32)
	f := &fakeRuntime{
		startID: "loop-ts",
		live:    live,
		hist: []event.Event{
			{Seq: 1, Kind: event.KindTurnStart},
			{Seq: 2, Kind: event.KindModelCallStart},
		},
	}

	// The overlap: on the FIRST Observe only, land seq 3 in BOTH durable history and the
	// live channel, inside the subscribe->replay window (the gate fires inside rt.Observe,
	// BEFORE the handler snapshots history via Attach). A naive replay would double-deliver
	// seq 3; the handler's watermark dedup must drop the live copy so the client sees it
	// exactly once. seq 4 (the terminal loop.parked completion event) is appended to
	// history and pushed to live in-order, in the same gate, so history is deterministically
	// [1,2,3,4] and the live tail is [3,4] — no out-of-order replay, no timing race. On a
	// RECONNECT (second+ Observe) the gate is a no-op: history already holds [1,2,3,4], so
	// Attach(fromSeq) replays only the tail past fromSeq with no loss and no dup.
	var once sync.Once
	f.observeGate = func() {
		once.Do(func() {
			f.appendHist(event.Event{Seq: 3, Kind: event.KindModelCallEnd})
			f.appendHist(event.Event{Seq: 4, Kind: event.KindLoopParked})
			live <- event.Event{Seq: 3, Kind: event.KindModelCallEnd}
			live <- event.Event{Seq: 4, Kind: event.KindLoopParked}
		})
	}

	handler := loopconnect.NewHandler(f).WithKeepalive(time.Hour)
	path, h := loopv1connect.NewLoopServiceHandler(handler)
	_ = path

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

	// Shut down cleanly on SIGINT/SIGTERM (the node test kills the process on teardown).
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
