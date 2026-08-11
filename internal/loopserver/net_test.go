package loopserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/ethanhinson/fuse/internal/event"
	rt "github.com/ethanhinson/fuse/internal/runtime"
)

// wsClient is the TEST-SIDE WebSocket JSON-RPC client. Its read pump demuxes two
// distinct frame classes off the single connection:
//
//   - id-keyed frames (responses to loop.start / loop.observe) are routed to a
//     per-id waiter channel; and
//   - id-LESS loop.event notification frames (server push) are routed to a separate
//     events channel.
//
// This split is the whole point of the mcp-read-pumps-drop-inbound-notifications
// guard: a demux that keys only on id silently drops every server-push loop.event.
// So the pump inspects each inbound frame and, when it has no id, treats it as a
// notification instead of discarding it.
type wsClient struct {
	c   *websocket.Conn
	ctx context.Context

	mu      sync.Mutex
	nextID  int
	waiters map[string]chan rawFrame

	events chan eventNoteParams

	pumpErr chan error
}

func newWSClient(ctx context.Context, c *websocket.Conn) *wsClient {
	cl := &wsClient{
		c:       c,
		ctx:     ctx,
		nextID:  1,
		waiters: map[string]chan rawFrame{},
		events:  make(chan eventNoteParams, 256),
		pumpErr: make(chan error, 1),
	}
	go cl.readPump()
	return cl
}

func (cl *wsClient) readPump() {
	for {
		_, data, err := cl.c.Read(cl.ctx)
		if err != nil {
			cl.pumpErr <- err
			// Fail every outstanding waiter so a blocked call() unblocks on drop.
			cl.mu.Lock()
			for id, ch := range cl.waiters {
				close(ch)
				delete(cl.waiters, id)
			}
			cl.mu.Unlock()
			close(cl.events)
			return
		}
		var f rawFrame
		if err := json.Unmarshal(data, &f); err != nil {
			cl.pumpErr <- err
			return
		}
		// THE GUARD: an id-less frame is a server-push notification, NOT a response.
		// Route it to the events channel. A demux keyed only on id would drop it here.
		if len(f.ID) == 0 {
			if f.Method == "loop.event" {
				var np eventNoteParams
				if err := json.Unmarshal(f.Params, &np); err != nil {
					cl.pumpErr <- err
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
		// id-keyed response: route to its waiter.
		cl.mu.Lock()
		w := cl.waiters[string(f.ID)]
		delete(cl.waiters, string(f.ID))
		cl.mu.Unlock()
		if w != nil {
			w <- f
		}
	}
}

// call sends a JSON-RPC request and blocks for its id-keyed response.
func (cl *wsClient) call(t *testing.T, method string, params any) rawFrame {
	t.Helper()
	cl.mu.Lock()
	id := cl.nextID
	cl.nextID++
	idRaw, _ := json.Marshal(id)
	wch := make(chan rawFrame, 1)
	cl.waiters[string(idRaw)] = wch
	cl.mu.Unlock()

	praw, _ := json.Marshal(params)
	body, _ := json.Marshal(req{JSONRPC: "2.0", ID: idRaw, Method: method, Params: praw})
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
		return rawFrame{}
	}
}

// nextEvent reads the next server-push loop.event within a timeout.
func (cl *wsClient) nextEvent(t *testing.T) eventNoteParams {
	t.Helper()
	select {
	case np, ok := <-cl.events:
		if !ok {
			t.Fatalf("events channel closed awaiting next loop.event")
		}
		return np
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout awaiting loop.event")
		return eventNoteParams{}
	}
}

// serveOneWS accepts a single WS connection on an httptest server and runs ServeWS
// over it against runtime r. It returns the server, a dialed client Conn, and a
// done channel closed when ServeWS returns.
func serveOneWS(t *testing.T, r rt.Runtime) (*httptest.Server, *websocket.Conn, chan error) {
	t.Helper()
	done := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		c, err := websocket.Accept(w, req, nil)
		if err != nil {
			done <- err
			return
		}
		done <- ServeWS(req.Context(), c, r)
	}))
	t.Cleanup(srv.Close)

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	c, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	return srv, c, done
}

// --- Test A: happy path over the real WS transport -------------------------------

func TestWSObserveReplaysThenLiveTails(t *testing.T) {
	live := make(chan event.Event, 4)
	fr := &fakeRuntime{
		startID: "loop-ws",
		attachHist: []event.Event{
			{Seq: 1, Kind: event.KindTurnStart},
			{Seq: 2, Kind: event.KindModelCallStart},
		},
		observeCh: live,
	}
	_, conn, _ := serveOneWS(t, fr)

	ctx := context.Background()
	cl := newWSClient(ctx, conn)

	// loop.start -> loop_id
	f := cl.call(t, "loop.start", startParams{Task: "hi", Model: "cloud/x"})
	if f.Error != nil {
		t.Fatalf("loop.start error: %+v", f.Error)
	}
	var sr startResult
	if err := json.Unmarshal(f.Result, &sr); err != nil {
		t.Fatalf("unmarshal startResult: %v", err)
	}
	if sr.LoopID != "loop-ws" {
		t.Fatalf("loop_id = %q, want loop-ws", sr.LoopID)
	}

	// loop.observe from 0 -> observeResult response, THEN replayed loop.event frames,
	// THEN live-tail frames.
	f = cl.call(t, "loop.observe", observeParams{LoopID: "loop-ws"})
	if f.Error != nil {
		t.Fatalf("loop.observe error: %+v", f.Error)
	}
	var or observeResult
	if err := json.Unmarshal(f.Result, &or); err != nil {
		t.Fatalf("unmarshal observeResult: %v", err)
	}
	if or.Replayed != 2 || or.LastSeq != 2 {
		t.Fatalf("observeResult = %+v, want replayed:2 last_seq:2", or)
	}

	// Feed one live event AFTER draining the two replays.
	seen := map[event.Seq]int{}
	var order []event.Seq
	for _, want := range []event.Seq{1, 2} {
		np := cl.nextEvent(t)
		seen[np.Event.Seq]++
		order = append(order, np.Event.Seq)
		if np.Event.Seq != want {
			t.Fatalf("replay seq = %d, want %d", np.Event.Seq, want)
		}
		if np.Gap {
			t.Fatalf("replay seq %d unexpectedly gap:true", np.Event.Seq)
		}
	}
	live <- event.Event{Seq: 3, Kind: event.KindTurnEnd}
	np := cl.nextEvent(t)
	seen[np.Event.Seq]++
	order = append(order, np.Event.Seq)
	if np.Event.Seq != 3 || np.LoopID != "loop-ws" {
		t.Fatalf("live note = %+v, want seq 3 loop-ws", np)
	}

	// Each seq seen exactly once, in order 1,2,3.
	for seq, n := range seen {
		if n != 1 {
			t.Fatalf("seq %d seen %d times, want exactly once", seq, n)
		}
	}
	for i, want := range []event.Seq{1, 2, 3} {
		if order[i] != want {
			t.Fatalf("order[%d] = %d, want %d", i, order[i], want)
		}
	}
}

// --- Test B: concurrent reattach dedup (LOAD BEARING) ----------------------------

// gatedRuntime is a runtime double that FORCES an append into the subscribe->replay
// gap concurrently: Attach blocks until the test has both (a) pushed the injected
// event onto the live channel and (b) recorded it in the history slice Attach will
// return. That models a durable event that races into the window between Observe
// (subscribe) and Attach (history read): it lands in BOTH the replayed history AND
// the live channel, so the inherited serveObserve watermark-dedup is exercised over
// the REAL WS transport. A sequential test cannot produce this double-delivery.
type gatedRuntime struct {
	loopID   string
	live     chan event.Event
	attachGo chan struct{} // closed by the test to release the blocked Attach

	mu   sync.Mutex
	hist []event.Event
}

func (g *gatedRuntime) StartLoop(ctx context.Context, cfg rt.LoopConfig) (rt.LoopHandle, error) {
	return runtimeHandle{id: g.loopID}, nil
}
func (g *gatedRuntime) Send(ctx context.Context, tenant event.TenantID, loopID, input string) error {
	return nil
}
func (g *gatedRuntime) Spawn(ctx context.Context, loopID string, opts rt.SpawnOpts) (rt.SpawnHandle, error) {
	return nil, nil
}
func (g *gatedRuntime) Observe(ctx context.Context, tenant event.TenantID, loopID string) (<-chan event.Event, func(), error) {
	g.mu.Lock()
	ch := g.live
	g.mu.Unlock()
	return ch, func() {}, nil
}
func (g *gatedRuntime) Attach(ctx context.Context, tenant event.TenantID, loopID string, from event.Seq) ([]event.Event, error) {
	// Block until the test has injected the concurrent event into BOTH paths, so the
	// history we return already includes it (and the live channel already carries it).
	g.mu.Lock()
	gate := g.attachGo
	g.mu.Unlock()
	select {
	case <-gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []event.Event
	for _, e := range g.hist {
		if e.Seq > from {
			out = append(out, e)
		}
	}
	return out, nil
}

func (g *gatedRuntime) setHist(evs []event.Event) {
	g.mu.Lock()
	g.hist = evs
	g.mu.Unlock()
}

func TestWSConcurrentReattachDedup(t *testing.T) {
	live := make(chan event.Event, 8)
	g := &gatedRuntime{
		loopID:   "loop-gap",
		live:     live,
		attachGo: make(chan struct{}),
	}
	// Baseline durable history at observe time: Seq 1,2.
	g.setHist([]event.Event{
		{Seq: 1, Kind: event.KindTurnStart},
		{Seq: 2, Kind: event.KindModelCallStart},
	})

	_, conn, _ := serveOneWS(t, g)
	ctx := context.Background()
	cl := newWSClient(ctx, conn)

	// Issue loop.observe. serveObserve calls Observe (subscribe) and then blocks in
	// Attach until we release attachGo. CONCURRENTLY, inject Seq 3 into the live
	// channel AND into the history so it double-delivers, then release Attach.
	respCh := make(chan rawFrame, 1)
	go func() { respCh <- cl.call(t, "loop.observe", observeParams{LoopID: "loop-gap"}) }()

	// Force the concurrent append into the subscribe->replay gap: Seq 3 is both pushed
	// live and added to the history Attach will return.
	live <- event.Event{Seq: 3, Kind: event.KindModelCallEnd}
	g.setHist([]event.Event{
		{Seq: 1, Kind: event.KindTurnStart},
		{Seq: 2, Kind: event.KindModelCallStart},
		{Seq: 3, Kind: event.KindModelCallEnd},
	})
	close(g.attachGo) // release Attach: history now includes Seq 3.

	f := <-respCh
	if f.Error != nil {
		t.Fatalf("observe error: %+v", f.Error)
	}
	var or observeResult
	if err := json.Unmarshal(f.Result, &or); err != nil {
		t.Fatalf("unmarshal observeResult: %v", err)
	}
	if or.LastSeq != 3 {
		t.Fatalf("observeResult last_seq = %d, want 3 (Seq 3 replayed in-window)", or.LastSeq)
	}

	// Now feed a genuinely-new contiguous Seq 4, then re-observe won't be needed for
	// the dedup assertion — the live pump must SKIP the duplicate Seq 3 delivered on
	// the live channel and deliver Seq 4 next, with no gap.
	live <- event.Event{Seq: 4, Kind: event.KindTurnEnd}

	seen := map[event.Seq]int{}
	// Replayed history notes: Seq 1,2,3.
	for _, want := range []event.Seq{1, 2, 3} {
		np := cl.nextEvent(t)
		seen[np.Event.Seq]++
		if np.Event.Seq != want {
			t.Fatalf("replay seq = %d, want %d", np.Event.Seq, want)
		}
	}
	// The live channel delivered Seq 3 (duplicate) then Seq 4. The watermark dedup
	// must drop the duplicate 3 and surface 4 next with no gap.
	np := cl.nextEvent(t)
	seen[np.Event.Seq]++
	if np.Event.Seq != 4 {
		t.Fatalf("post-window live seq = %d, want 4 (duplicate Seq 3 must be deduped)", np.Event.Seq)
	}
	if np.Gap {
		t.Fatalf("seq 4 unexpectedly gap:true (contiguous with watermark 3)")
	}
	for seq, n := range seen {
		if n != 1 {
			t.Fatalf("seq %d delivered %d times, want exactly once (no dup, no loss)", seq, n)
		}
	}

	// A forced hole PAST the watermark yields exactly one gap:true frame. Drop the
	// connection, re-observe from lastSeq=4, then push a hole to Seq 7.
	conn.Close(websocket.StatusNormalClosure, "reattach")

	// Second connection re-observes from 4.
	_, conn2, _ := serveOneWS(t, g)
	cl2 := newWSClient(ctx, conn2)
	// Reset the gate + history for the second observe: history has Seq 1..4, no
	// in-window injection this time (release immediately). Swap in a FRESH live
	// channel so the second observe's pump is the sole reader of the forced-hole event
	// (the first connection's pump, still winding down on its cancelled ctx, cannot
	// steal it off the shared channel).
	g2go := make(chan struct{})
	live2 := make(chan event.Event, 4)
	g.mu.Lock()
	g.attachGo = g2go
	g.live = live2
	g.hist = []event.Event{
		{Seq: 1, Kind: event.KindTurnStart},
		{Seq: 2, Kind: event.KindModelCallStart},
		{Seq: 3, Kind: event.KindModelCallEnd},
		{Seq: 4, Kind: event.KindTurnEnd},
	}
	g.mu.Unlock()

	respCh2 := make(chan rawFrame, 1)
	go func() { respCh2 <- cl2.call(t, "loop.observe", observeParams{LoopID: "loop-gap", FromSeq: 4}) }()
	close(g2go)
	f2 := <-respCh2
	if f2.Error != nil {
		t.Fatalf("re-observe error: %+v", f2.Error)
	}
	var or2 observeResult
	if err := json.Unmarshal(f2.Result, &or2); err != nil {
		t.Fatalf("unmarshal re-observe result: %v", err)
	}
	if or2.Replayed != 0 || or2.LastSeq != 4 {
		t.Fatalf("re-observe result = %+v, want replayed:0 last_seq:4", or2)
	}

	// Force a hole: watermark is 4, push Seq 7 -> exactly one gap:true frame.
	live2 <- event.Event{Seq: 7, Kind: event.KindTurnEnd}
	gaps := 0
	np = cl2.nextEvent(t)
	if np.Event.Seq != 7 {
		t.Fatalf("post-hole seq = %d, want 7", np.Event.Seq)
	}
	if np.Gap {
		gaps++
	}
	if gaps != 1 {
		t.Fatalf("hole past watermark yielded %d gap frames, want exactly 1", gaps)
	}
}

// --- Goroutine cleanup: client drop tears down the server session ----------------

// cancelSignalRuntime is a runtime double whose Observe cancel() closes a channel
// exactly once, giving the test a race-free signal that the observe pump's deferred
// cancel() fired (releasing the subscription) on client drop / ctx cancel. It avoids
// the plain-bool observeStop field on server_test.go's fakeRuntime, which a
// concurrent poll would data-race against.
type cancelSignalRuntime struct {
	loopID   string
	live     chan event.Event
	hist     []event.Event
	released chan struct{}
	once     sync.Once
}

func (r *cancelSignalRuntime) StartLoop(ctx context.Context, cfg rt.LoopConfig) (rt.LoopHandle, error) {
	return runtimeHandle{id: r.loopID}, nil
}
func (r *cancelSignalRuntime) Send(ctx context.Context, tenant event.TenantID, loopID, input string) error {
	return nil
}
func (r *cancelSignalRuntime) Spawn(ctx context.Context, loopID string, opts rt.SpawnOpts) (rt.SpawnHandle, error) {
	return nil, nil
}
func (r *cancelSignalRuntime) Observe(ctx context.Context, tenant event.TenantID, loopID string) (<-chan event.Event, func(), error) {
	return r.live, func() { r.once.Do(func() { close(r.released) }) }, nil
}
func (r *cancelSignalRuntime) Attach(ctx context.Context, tenant event.TenantID, loopID string, from event.Seq) ([]event.Event, error) {
	return r.hist, nil
}

func TestWSClientDropReleasesServer(t *testing.T) {
	r := &cancelSignalRuntime{
		loopID:   "loop-drop",
		live:     make(chan event.Event, 1),
		hist:     []event.Event{{Seq: 1, Kind: event.KindTurnStart}},
		released: make(chan struct{}),
	}
	_, conn, done := serveOneWS(t, r)
	ctx := context.Background()
	cl := newWSClient(ctx, conn)

	// Establish an observe so the live pump goroutine + subscription are active.
	f := cl.call(t, "loop.observe", observeParams{LoopID: "loop-drop"})
	if f.Error != nil {
		t.Fatalf("observe error: %+v", f.Error)
	}
	_ = cl.nextEvent(t) // drain the one replayed note

	before := runtime.NumGoroutine()

	// Client drops the connection. ServeWS must return and the observe pump's
	// deferred cancel() must fire, releasing the subscription (closes r.released).
	conn.Close(websocket.StatusNormalClosure, "bye")

	select {
	case <-done: // Serve returns once the client is gone.
	case <-time.After(5 * time.Second):
		t.Fatal("ServeWS did not return after client drop")
	}

	select {
	case <-r.released:
	case <-time.After(2 * time.Second):
		t.Fatal("observe subscription not released after client drop (cancel() did not fire)")
	}

	// No goroutine leak: after teardown, the count settles back to (roughly) the
	// pre-drop baseline. Poll to let scheduled goroutines exit.
	leakDeadline := time.Now().Add(2 * time.Second)
	for {
		runtime.GC()
		after := runtime.NumGoroutine()
		if after <= before {
			break
		}
		if time.Now().After(leakDeadline) {
			t.Fatalf("goroutine leak: %d goroutines after teardown, baseline %d", after, before)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
