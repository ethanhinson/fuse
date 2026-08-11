package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/ethanhinson/fuse/internal/event"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/runtime"
)

// TestLoopServeNetDispatchRegistered proves `fuse loop-serve-net` is wired into the
// dispatch switch. It swaps the netListen seam for a loopback ephemeral listener and
// the run's context for one we cancel immediately, so runLoopServeNet stands up the
// Connect mux, serves, and returns exit 0 on the cancel — no real signal, no port
// contention. Mirrors TestLoopServerDispatchRegistered.
func TestLoopServeNetDispatchRegistered(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Ephemeral loopback listener so the test never binds the default :8787.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	oldListen := netListen
	netListen = func(string, string) (net.Listener, error) { return ln, nil }
	defer func() { netListen = oldListen }()

	// Cancel the serve context immediately so http.Serve returns and the run exits 0.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	oldCtx := serveNetContext
	serveNetContext = func() (context.Context, context.CancelFunc) { return ctx, func() {} }
	defer func() { serveNetContext = oldCtx }()

	var out, errb strings.Builder
	code := run([]string{"loop-serve-net"}, &out, &errb)
	if code != 0 {
		t.Fatalf("loop-serve-net exit = %d, want 0; stderr:\n%s", code, errb.String())
	}
}

// TestHelpListsLoopServeNet proves the help output advertises the new subcommand.
func TestHelpListsLoopServeNet(t *testing.T) {
	var out, errb strings.Builder
	if code := run([]string{"help"}, &out, &errb); code != 0 {
		t.Fatalf("help exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "loop-serve-net") {
		t.Fatalf("help output does not mention loop-serve-net:\n%s", out.String())
	}
}

// --- Test B: in-process E2E over serveNet against a fake Runtime ------------------

// netFakeRuntime is a scripted runtime.Runtime double: StartLoop hands back a fixed
// loop_id, Attach returns a canned durable history, and Observe feeds a controllable
// live channel. No live model is involved.
type netFakeRuntime struct {
	startID string
	hist    []event.Event
	live    chan event.Event
}

func (f *netFakeRuntime) StartLoop(ctx context.Context, cfg runtime.LoopConfig) (runtime.LoopHandle, error) {
	return netFakeHandle{id: f.startID}, nil
}
func (f *netFakeRuntime) Send(ctx context.Context, tenant event.TenantID, loopID, input string) error {
	return nil
}
func (f *netFakeRuntime) Spawn(ctx context.Context, loopID string, opts runtime.SpawnOpts) (runtime.SpawnHandle, error) {
	return nil, nil
}
func (f *netFakeRuntime) Observe(ctx context.Context, tenant event.TenantID, loopID string) (<-chan event.Event, func(), error) {
	return f.live, func() {}, nil
}
func (f *netFakeRuntime) Attach(ctx context.Context, tenant event.TenantID, loopID string, from event.Seq) ([]event.Event, error) {
	var out []event.Event
	for _, e := range f.hist {
		if e.Seq > from {
			out = append(out, e)
		}
	}
	return out, nil
}

type netFakeHandle struct{ id string }

func (h netFakeHandle) ID() string                     { return h.id }
func (h netFakeHandle) Wait() ([]model.Message, error) { return nil, nil }

// TestLoopServeNetEndToEnd is the fast in-process E2E: serveNet is started on
// 127.0.0.1:0 with a fake Runtime, the test reads the chosen port off the listener,
// and a connect-go client drives StartLoop (unary) and Observe (server-stream),
// asserting the replayed frames then a live-tailed event. Shut down cleanly via
// context cancel. No live model, no WebSocket.
func TestLoopServeNetEndToEnd(t *testing.T) {
	live := make(chan event.Event, 4)
	fr := &netFakeRuntime{
		startID: "loop-net",
		hist: []event.Event{
			{Seq: 1, Kind: event.KindTurnStart},
			{Seq: 2, Kind: event.KindModelCallStart},
		},
		live: live,
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- serveNet(ctx, ln, fr) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-serveDone:
		case <-time.After(5 * time.Second):
			t.Error("serveNet did not return after cancel")
		}
	})

	base := "http://" + addr
	client := loopv1connect.NewLoopServiceClient(http.DefaultClient, base)

	// StartLoop (unary) -> loop_id.
	sr, err := client.StartLoop(context.Background(), connect.NewRequest(&loopv1.StartLoopRequest{Task: "hi", Model: "cloud/x"}))
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	if sr.Msg.LoopId != "loop-net" {
		t.Fatalf("loop_id = %q, want loop-net", sr.Msg.LoopId)
	}

	// Observe (server-stream) from 0 -> replayed 1,2 then a live-tailed 3.
	stream, err := client.Observe(context.Background(), connect.NewRequest(&loopv1.ObserveRequest{LoopId: "loop-net"}))
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	// A live event tails through after the replayed pair.
	live <- event.Event{Seq: 3, Kind: event.KindTurnEnd}

	var seqs []uint64
	for len(seqs) < 3 {
		if !stream.Receive() {
			t.Fatalf("stream ended early (err=%v); got %v", stream.Err(), seqs)
		}
		msg := stream.Msg()
		if msg.Keepalive || msg.Event == nil {
			continue
		}
		seqs = append(seqs, msg.Event.Seq)
	}
	if fmt.Sprint(seqs) != "[1 2 3]" {
		t.Fatalf("observed seqs = %v, want [1 2 3]", seqs)
	}
	_ = stream.Close()
}
