package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/runtime"
)

// TestLoopServeNetDispatchRegistered proves `fuse loop-serve-net` is wired into the
// dispatch switch. It swaps the netListen seam for a loopback ephemeral listener and
// the run's context for one we cancel immediately, so runLoopServeNet stands up the
// WS+HTTP mux, serves, and returns exit 0 on the cancel — no real signal, no port
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
// live channel. No live model is involved. It mirrors the doubles in the loopserver
// package's own tests.
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

// netWSClient is a minimal test-side WS JSON-RPC client. Its read pump demuxes
// id-keyed responses from id-less loop.event notifications (the
// mcp-read-pumps-drop-inbound-notifications guard).
type netWSClient struct {
	c   *websocket.Conn
	ctx context.Context

	mu      sync.Mutex
	nextID  int
	waiters map[string]chan netRawFrame

	events chan netEventParams
}

type netRawFrame struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
	Params json.RawMessage `json:"params"`
}

type netEventParams struct {
	LoopID string      `json:"loop_id"`
	Event  event.Event `json:"event"`
	Gap    bool        `json:"gap"`
}

func newNetWSClient(ctx context.Context, c *websocket.Conn) *netWSClient {
	cl := &netWSClient{
		c:       c,
		ctx:     ctx,
		nextID:  1,
		waiters: map[string]chan netRawFrame{},
		events:  make(chan netEventParams, 64),
	}
	go cl.readPump()
	return cl
}

func (cl *netWSClient) readPump() {
	for {
		_, data, err := cl.c.Read(cl.ctx)
		if err != nil {
			cl.mu.Lock()
			for id, ch := range cl.waiters {
				close(ch)
				delete(cl.waiters, id)
			}
			cl.mu.Unlock()
			return
		}
		var f netRawFrame
		if err := json.Unmarshal(data, &f); err != nil {
			return
		}
		if len(f.ID) == 0 {
			if f.Method == "loop.event" {
				var np netEventParams
				if err := json.Unmarshal(f.Params, &np); err != nil {
					return
				}
				select {
				case cl.events <- np:
				case <-cl.ctx.Done():
					return
				}
			}
			continue
		}
		cl.mu.Lock()
		w := cl.waiters[string(f.ID)]
		delete(cl.waiters, string(f.ID))
		cl.mu.Unlock()
		if w != nil {
			w <- f
		}
	}
}

func (cl *netWSClient) call(t *testing.T, method string, params any) netRawFrame {
	t.Helper()
	cl.mu.Lock()
	id := cl.nextID
	cl.nextID++
	idRaw, _ := json.Marshal(id)
	wch := make(chan netRawFrame, 1)
	cl.waiters[string(idRaw)] = wch
	cl.mu.Unlock()

	praw, _ := json.Marshal(params)
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(idRaw),
		"method":  method,
		"params":  json.RawMessage(praw),
	})
	if err := cl.c.Write(cl.ctx, websocket.MessageText, body); err != nil {
		t.Fatalf("ws write %s: %v", method, err)
	}
	select {
	case f, ok := <-wch:
		if !ok {
			t.Fatalf("connection dropped awaiting %s response", method)
		}
		return f
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout awaiting %s response", method)
		return netRawFrame{}
	}
}

func (cl *netWSClient) nextEvent(t *testing.T) netEventParams {
	t.Helper()
	select {
	case np, ok := <-cl.events:
		if !ok {
			t.Fatalf("events channel closed")
		}
		return np
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout awaiting loop.event")
		return netEventParams{}
	}
}

// TestLoopServeNetEndToEnd is the fast in-process E2E: serveNet is started on
// 127.0.0.1:0 with a fake Runtime, the test reads the chosen port off the listener,
// dials the WS with a real coder/websocket client, drives loop.start + loop.observe
// and asserts the replayed frames, then hits the HTTP replay endpoint and asserts the
// same history. Shut down cleanly via context cancel. No live model.
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

	// Dial the WS endpoint.
	wsURL := "ws://" + addr + "/ws"
	dialCtx := context.Background()
	c, _, err := websocket.Dial(dialCtx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial %s: %v", wsURL, err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	cl := newNetWSClient(dialCtx, c)

	// loop.start -> loop_id
	f := cl.call(t, "loop.start", map[string]any{"task": "hi", "model": "cloud/x"})
	if len(f.Error) != 0 && string(f.Error) != "null" {
		t.Fatalf("loop.start error: %s", f.Error)
	}
	var sr struct {
		LoopID string `json:"loop_id"`
	}
	if err := json.Unmarshal(f.Result, &sr); err != nil {
		t.Fatalf("unmarshal startResult: %v", err)
	}
	if sr.LoopID != "loop-net" {
		t.Fatalf("loop_id = %q, want loop-net", sr.LoopID)
	}

	// loop.observe from 0 -> observeResult then replayed loop.event frames.
	f = cl.call(t, "loop.observe", map[string]any{"loop_id": "loop-net"})
	if len(f.Error) != 0 && string(f.Error) != "null" {
		t.Fatalf("loop.observe error: %s", f.Error)
	}
	var or struct {
		Replayed int       `json:"replayed"`
		LastSeq  event.Seq `json:"last_seq"`
	}
	if err := json.Unmarshal(f.Result, &or); err != nil {
		t.Fatalf("unmarshal observeResult: %v", err)
	}
	if or.Replayed != 2 || or.LastSeq != 2 {
		t.Fatalf("observeResult = %+v, want replayed:2 last_seq:2", or)
	}
	for _, want := range []event.Seq{1, 2} {
		np := cl.nextEvent(t)
		if np.Event.Seq != want {
			t.Fatalf("replay seq = %d, want %d", np.Event.Seq, want)
		}
	}
	// A live event tails through.
	live <- event.Event{Seq: 3, Kind: event.KindTurnEnd}
	np := cl.nextEvent(t)
	if np.Event.Seq != 3 || np.LoopID != "loop-net" {
		t.Fatalf("live note = %+v, want seq 3 loop-net", np)
	}

	// HTTP replay endpoint: GET /loops/{id}/events returns the durable history.
	httpURL := fmt.Sprintf("http://%s/loops/%s/events", addr, "loop-net")
	req, _ := http.NewRequest(http.MethodGet, httpURL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("http GET %s: %v", httpURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("http replay status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	var hist []event.Event
	if err := json.Unmarshal(body, &hist); err != nil {
		t.Fatalf("unmarshal replay history: %v", err)
	}
	if len(hist) != 2 || hist[0].Seq != 1 || hist[1].Seq != 2 {
		t.Fatalf("replay history = %+v, want seqs [1 2]", hist)
	}
}
