package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// cancelRig is a durable runtime (a real fsstore durable store as both stream store
// and registry) for the cancel tests. Every value the run goroutine writes and the
// test reads is guarded.
type cancelRig struct {
	rt    Runtime
	c     Canceller
	store *fsstore.FSDurableStore

	mu        sync.Mutex
	teardowns int
}

type cancelRigOpts struct {
	completer  agent.Completer
	tools      []tools.Tool
	idleTTL    time.Duration
	cancelWait time.Duration
}

func newCancelRig(t *testing.T, o cancelRigOpts) *cancelRig {
	t.Helper()
	g := &cancelRig{store: fsstore.NewDurableFSStore(t.TempDir())}
	if o.idleTTL == 0 {
		o.idleTTL = time.Minute
	}
	if o.cancelWait == 0 {
		o.cancelWait = 5 * time.Second
	}
	g.rt = New(Deps{
		DurableStore:  g.store,
		Registry:      g.store,
		RelayPoll:     20 * time.Millisecond,
		CancelWait:    o.cancelWait,
		IdleTTL:       o.idleTTL,
		MaxConcurrent: 2,
		LoopTeardown: func(*tools.Registry) {
			g.mu.Lock()
			g.teardowns++
			g.mu.Unlock()
		},
		BuildAgent: func(store event.EventStore, tree *agent.AgentTree, modelID string, reg *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			for _, tl := range o.tools {
				reg.Register(tl)
			}
			return agent.New(o.completer, execAll{reg}, nopRenderer{}, modelID, "", 0, 0), nil, modelID, nil
		},
	})
	c, ok := g.rt.(Canceller)
	if !ok {
		t.Fatal("the in-process runtime must implement Canceller")
	}
	g.c = c
	return g
}

func (g *cancelRig) teardownCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.teardowns
}

func streamOf(t *testing.T, store *fsstore.FSDurableStore, tenant event.TenantID, loopID string) []event.Event {
	t.Helper()
	evs, err := store.Replay(context.Background(), event.StreamKey{Tenant: event.NormalizeTenant(tenant), Loop: event.LoopID(loopID)}, 0)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	return evs
}

func lastKind(evs []event.Event) event.Kind {
	if len(evs) == 0 {
		return ""
	}
	return evs[len(evs)-1].Kind
}

func countKind(evs []event.Event, k event.Kind) int {
	n := 0
	for _, e := range evs {
		if e.Kind == k {
			n++
		}
	}
	return n
}

func indexOfKind(evs []event.Event, k event.Kind) int {
	for i, e := range evs {
		if e.Kind == k {
			return i
		}
	}
	return -1
}

func cancelledPayload(t *testing.T, e event.Event) event.LoopCancelledPayload {
	t.Helper()
	if e.Kind != event.KindLoopCancelled {
		t.Fatalf("event %d is %s, want %s", e.Seq, e.Kind, event.KindLoopCancelled)
	}
	var p event.LoopCancelledPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatalf("decode loop.cancelled: %v", err)
	}
	return p
}

func isLive(t *testing.T, store *fsstore.FSDurableStore, tenant event.TenantID, loopID string) bool {
	t.Helper()
	rec, err := store.Resolve(context.Background(), event.StreamKey{Tenant: event.NormalizeTenant(tenant), Loop: event.LoopID(loopID)})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return rec.Live
}

// waitStreamKind polls the durable stream until it holds an event of kind k.
func waitStreamKind(t *testing.T, store *fsstore.FSDurableStore, tenant event.TenantID, loopID string, k event.Kind) {
	t.Helper()
	eventually(t, func() bool { return indexOfKind(streamOf(t, store, tenant, loopID), k) >= 0 })
}

// cancelBlockingCompleter closes started on its first call, then blocks until its ctx
// ends and records the ctx error. Later calls answer "done".
type cancelBlockingCompleter struct {
	started chan struct{}
	once    sync.Once

	mu    sync.Mutex
	calls int
	saw   error
}

func newCancelBlockingCompleter() *cancelBlockingCompleter {
	return &cancelBlockingCompleter{started: make(chan struct{})}
}

func (b *cancelBlockingCompleter) Complete(ctx context.Context, _ model.CompletionReq) (model.CompletionResp, error) {
	b.mu.Lock()
	b.calls++
	first := b.calls == 1
	b.mu.Unlock()
	if !first {
		return modelText("done"), nil
	}
	b.once.Do(func() { close(b.started) })
	<-ctx.Done()
	b.mu.Lock()
	b.saw = ctx.Err()
	b.mu.Unlock()
	return model.CompletionResp{}, ctx.Err()
}

func (b *cancelBlockingCompleter) sawErr() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.saw
}

// blockTool closes started when it runs, then waits for its ctx to end. It lingers
// briefly after the cancel so the agent's own cancellation branch records the result.
type blockTool struct {
	started chan struct{}
	once    sync.Once
}

func (b *blockTool) Name() string               { return "block" }
func (b *blockTool) Description() string        { return "blocks until cancelled" }
func (b *blockTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (b *blockTool) Execute(ctx context.Context, _ string) tools.Result {
	b.once.Do(func() { close(b.started) })
	<-ctx.Done()
	time.Sleep(50 * time.Millisecond)
	return tools.Result{Output: "finished"}
}

// script answers each call with the next response, then "done".
func script(resps ...model.CompletionResp) agent.Completer {
	var mu sync.Mutex
	i := 0
	return completerFunc(func(context.Context, model.CompletionReq) (model.CompletionResp, error) {
		mu.Lock()
		defer mu.Unlock()
		if i >= len(resps) {
			return modelText("done"), nil
		}
		r := resps[i]
		i++
		return r, nil
	})
}

const acme = event.TenantID("acme")

// startCancelledOneShot starts a one-shot loop blocked in its model call and
// cancels it, returning the rig, the loop id, and the cancel result.
func startCancelledOneShot(t *testing.T) (*cancelRig, *cancelBlockingCompleter, LoopHandle, CancelResult) {
	t.Helper()
	bc := newCancelBlockingCompleter()
	g := newCancelRig(t, cancelRigOpts{completer: bc})
	ctx := context.Background()
	h, err := g.rt.StartLoop(ctx, LoopConfig{Task: "work", ModelID: "m", Tenant: acme})
	if err != nil {
		t.Fatal(err)
	}
	<-bc.started
	res, err := g.c.Cancel(ctx, acme, h.ID(), "operator stop", "alice")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	return g, bc, h, res
}

// Criterion 5: a one-shot loop blocked in model.Complete is interrupted, ends with
// loop.cancelled as its highest-seq event, and is not live once Cancel returns.
func TestCancelOneShotDuringModelCall(t *testing.T) {
	g, bc, h, res := startCancelledOneShot(t)
	live := isLive(t, g.store, acme, h.ID()) // read right after Cancel returned

	if res.Outcome != CancelOutcomeCancelled || !res.WasLive || res.Reason != "operator stop" {
		t.Fatalf("result = %+v, want Cancelled/was_live/operator stop", res)
	}
	if live {
		t.Fatal("the registry still showed the loop live when Cancel returned Cancelled")
	}
	if !errors.Is(bc.sawErr(), context.Canceled) {
		t.Fatalf("the completer saw %v, want context.Canceled", bc.sawErr())
	}
	if _, err := h.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait = %v, want context.Canceled", err)
	}
	evs := streamOf(t, g.store, acme, h.ID())
	last := evs[len(evs)-1]
	p := cancelledPayload(t, last)
	if p != (event.LoopCancelledPayload{Reason: "operator stop", RequestedBy: "alice", WasLive: true}) {
		t.Fatalf("payload = %+v", p)
	}
	if last.Seq != res.TerminalSeq || res.TerminalSeq == 0 {
		t.Fatalf("terminal seq: event %d, result %d", last.Seq, res.TerminalSeq)
	}
	if countKind(evs, event.KindLoopCancelled) != 1 {
		t.Fatalf("want exactly one loop.cancelled, got %d", countKind(evs, event.KindLoopCancelled))
	}
	if i := indexOfKind(evs, event.KindError); i < 0 || i >= len(evs)-1 {
		t.Fatalf("an error event must precede loop.cancelled (index %d of %d)", i, len(evs))
	}
	if g.teardownCount() != 1 {
		t.Fatalf("LoopTeardown ran %d times, want 1", g.teardownCount())
	}
}

// Criterion 6: a tool blocked on its ctx is cancelled, its result says so and
// precedes the terminal event, and the loop's resources were torn down.
func TestCancelDuringToolCall(t *testing.T) {
	bt := &blockTool{started: make(chan struct{})}
	g := newCancelRig(t, cancelRigOpts{
		completer: script(modelToolCall("c1", "block", "{}")),
		tools:     []tools.Tool{bt},
	})
	ctx := context.Background()
	h, err := g.rt.StartLoop(ctx, LoopConfig{Task: "work", ModelID: "m", Tenant: acme})
	if err != nil {
		t.Fatal(err)
	}
	<-bt.started
	res, err := g.c.Cancel(ctx, acme, h.ID(), "stop the tool", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != CancelOutcomeCancelled || !res.WasLive {
		t.Fatalf("result = %+v", res)
	}
	if g.teardownCount() != 1 {
		t.Fatalf("LoopTeardown ran %d times when Cancel returned, want 1", g.teardownCount())
	}
	_, _ = h.Wait()
	evs := streamOf(t, g.store, acme, h.ID())
	toolAt := -1
	for i, e := range evs {
		if e.Kind != event.KindToolResult {
			continue
		}
		var p event.ToolResultPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.ID == "c1" {
			if !p.IsError || !strings.Contains(p.Result, "cancelled") {
				t.Fatalf("tool result = %+v, want a cancelled error", p)
			}
			toolAt = i
		}
	}
	if toolAt < 0 {
		t.Fatal("no tool.result for c1")
	}
	if lastKind(evs) != event.KindLoopCancelled || toolAt >= len(evs)-1 {
		t.Fatalf("tool.result (index %d) must precede the terminal loop.cancelled (last %s)", toolAt, lastKind(evs))
	}
}

// Criterion 8: a parked interactive loop is cancelled and sealed; history stays
// readable.
func TestCancelParkedInteractiveLoop(t *testing.T) {
	g := newCancelRig(t, cancelRigOpts{completer: script(modelText("hi"))})
	ctx := context.Background()
	h, err := g.rt.StartLoop(ctx, LoopConfig{Task: "hello", ModelID: "m", Tenant: acme, Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	waitStreamKind(t, g.store, acme, h.ID(), event.KindLoopParked)

	res, err := g.c.Cancel(ctx, acme, h.ID(), "bye", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != CancelOutcomeCancelled || !res.WasLive {
		t.Fatalf("result = %+v", res)
	}
	evs := streamOf(t, g.store, acme, h.ID())
	if p := cancelledPayload(t, evs[len(evs)-1]); !p.WasLive || p.Reason != "bye" {
		t.Fatalf("payload = %+v", p)
	}
	if i := indexOfKind(evs, event.KindLoopParked); i < 0 || i >= len(evs)-1 {
		t.Fatal("loop.parked must precede loop.cancelled")
	}

	if err := g.rt.Send(ctx, acme, h.ID(), "more"); !errors.Is(err, ErrLoopCancelled) || errors.Is(err, ErrLoopFinished) {
		t.Fatalf("Send = %v, want ErrLoopCancelled (not ErrLoopFinished)", err)
	}
	if err := g.rt.(Steerer).Steer(ctx, acme, h.ID(), "more", SteerOpts{Interrupt: true}); !errors.Is(err, ErrLoopCancelled) || errors.Is(err, ErrLoopFinished) {
		t.Fatalf("Steer = %v, want ErrLoopCancelled", err)
	}
	if _, err := g.rt.Resume(ctx, acme, h.ID()); !errors.Is(err, ErrLoopCancelled) {
		t.Fatalf("Resume = %v, want ErrLoopCancelled", err)
	}
	if got := streamOf(t, g.store, acme, h.ID()); len(got) != len(evs) {
		t.Fatalf("the sealed loop's stream grew from %d to %d events", len(evs), len(got))
	}
	_, unsub, err := g.rt.Observe(ctx, acme, h.ID())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	unsub()
	hist, err := g.rt.Attach(ctx, acme, h.ID(), 0)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if lastKind(hist) != event.KindLoopCancelled || len(hist) != len(evs) {
		t.Fatalf("Attach history ends with %s (%d events), want loop.cancelled (%d)", lastKind(hist), len(hist), len(evs))
	}
}

// The idle reaper never fires a second completion on a cancelled loop.
func TestCancelParkedLoopReaperStaysQuiet(t *testing.T) {
	g := newCancelRig(t, cancelRigOpts{completer: script(modelText("hi")), idleTTL: 500 * time.Millisecond})
	ctx := context.Background()
	h, err := g.rt.StartLoop(ctx, LoopConfig{Task: "hello", ModelID: "m", Tenant: acme, Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	waitStreamKind(t, g.store, acme, h.ID(), event.KindLoopParked)
	if _, err := g.c.Cancel(ctx, acme, h.ID(), "bye", "alice"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	evs := streamOf(t, g.store, acme, h.ID())
	if lastKind(evs) != event.KindLoopCancelled || countKind(evs, event.KindLoopCancelled) != 1 {
		t.Fatalf("stream ends with %s holding %d loop.cancelled, want exactly one, last", lastKind(evs), countKind(evs, event.KindLoopCancelled))
	}
	if g.teardownCount() != 1 {
		t.Fatalf("LoopTeardown ran %d times, want 1", g.teardownCount())
	}
}

// Criterion 9 (local half): a second cancel reports the first and appends nothing.
func TestCancelIdempotent(t *testing.T) {
	g, _, h, first := startCancelledOneShot(t)
	_, _ = h.Wait()
	before := streamOf(t, g.store, acme, h.ID())

	again, err := g.c.Cancel(context.Background(), acme, h.ID(), "again", "bob")
	if err != nil {
		t.Fatal(err)
	}
	if again.Outcome != CancelOutcomeAlreadyCancelled || again.Reason != "operator stop" || again.TerminalSeq != first.TerminalSeq || !again.WasLive {
		t.Fatalf("second cancel = %+v, first = %+v", again, first)
	}
	if after := streamOf(t, g.store, acme, h.ID()); len(after) != len(before) {
		t.Fatalf("a second cancel appended: %d -> %d events", len(before), len(after))
	}
}

// Concurrent cancels: exactly one seals, the others report it, and exactly one
// terminal event exists.
func TestCancelConcurrentCallersAppendOnce(t *testing.T) {
	bc := newCancelBlockingCompleter()
	g := newCancelRig(t, cancelRigOpts{completer: bc})
	ctx := context.Background()
	h, err := g.rt.StartLoop(ctx, LoopConfig{Task: "work", ModelID: "m", Tenant: acme})
	if err != nil {
		t.Fatal(err)
	}
	<-bc.started
	const n = 8
	results := make([]CancelResult, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := g.c.Cancel(ctx, acme, h.ID(), "r", "alice")
			if err != nil {
				t.Errorf("Cancel %d: %v", i, err)
			}
			results[i] = res
		}(i)
	}
	wg.Wait()
	_, _ = h.Wait()
	evs := streamOf(t, g.store, acme, h.ID())
	if countKind(evs, event.KindLoopCancelled) != 1 || lastKind(evs) != event.KindLoopCancelled {
		t.Fatalf("want exactly one terminal loop.cancelled, got %d (last %s)", countKind(evs, event.KindLoopCancelled), lastKind(evs))
	}
	seq := evs[len(evs)-1].Seq
	cancelled := 0
	for i, r := range results {
		switch r.Outcome {
		case CancelOutcomeCancelled:
			cancelled++
		case CancelOutcomeAlreadyCancelled:
		default:
			t.Fatalf("result %d = %+v", i, r)
		}
		if r.TerminalSeq != seq || !r.WasLive {
			t.Fatalf("result %d = %+v, terminal seq %d", i, r, seq)
		}
	}
	if cancelled != 1 {
		t.Fatalf("%d callers reported Cancelled, want exactly 1", cancelled)
	}
}

// A cancel racing the run's own completion: exactly one party appends the terminal
// event, and the result agrees with it.
func TestCancelRacingCompletionAppendsExactlyOne(t *testing.T) {
	for i := 0; i < 25; i++ {
		g := newCancelRig(t, cancelRigOpts{completer: script(modelText("done"))})
		ctx := context.Background()
		h, err := g.rt.StartLoop(ctx, LoopConfig{Task: "work", ModelID: "m", Tenant: acme})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Duration(i%5) * time.Millisecond)
		res, err := g.c.Cancel(ctx, acme, h.ID(), "race", "alice")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = h.Wait()
		evs := streamOf(t, g.store, acme, h.ID())
		if countKind(evs, event.KindLoopCancelled) != 1 || lastKind(evs) != event.KindLoopCancelled {
			t.Fatalf("iteration %d: %d loop.cancelled, last %s", i, countKind(evs, event.KindLoopCancelled), lastKind(evs))
		}
		p := cancelledPayload(t, evs[len(evs)-1])
		if res.Outcome != CancelOutcomeCancelled || res.TerminalSeq != evs[len(evs)-1].Seq || res.WasLive != p.WasLive {
			t.Fatalf("iteration %d: result %+v, terminal %d payload %+v", i, res, evs[len(evs)-1].Seq, p)
		}
		if isLive(t, g.store, acme, h.ID()) {
			t.Fatalf("iteration %d: still live after Cancelled", i)
		}
	}
}

// Criterion 9: cancelling a loop that finished on its own seals it.
func TestCancelFinishedLoopSeals(t *testing.T) {
	g := newCancelRig(t, cancelRigOpts{completer: script(modelText("done"))})
	ctx := context.Background()
	h, err := g.rt.StartLoop(ctx, LoopConfig{Task: "work", ModelID: "m", Tenant: acme})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatal(err)
	}
	res, err := g.c.Cancel(ctx, acme, h.ID(), "cleanup", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != CancelOutcomeCancelled || res.WasLive || res.TerminalSeq == 0 {
		t.Fatalf("result = %+v, want Cancelled, was_live=false, a seq", res)
	}
	evs := streamOf(t, g.store, acme, h.ID())
	if p := cancelledPayload(t, evs[len(evs)-1]); p.WasLive || p.Reason != "cleanup" || evs[len(evs)-1].Seq != res.TerminalSeq {
		t.Fatalf("payload = %+v seq %d", p, evs[len(evs)-1].Seq)
	}
	if err := g.rt.Send(ctx, acme, h.ID(), "again"); !errors.Is(err, ErrLoopCancelled) || errors.Is(err, ErrLoopFinished) {
		t.Fatalf("Send = %v, want ErrLoopCancelled", err)
	}
	if _, err := g.rt.Resume(ctx, acme, h.ID()); !errors.Is(err, ErrLoopCancelled) {
		t.Fatalf("Resume = %v, want ErrLoopCancelled", err)
	}
}

// Criterion 9: an idle-reaped interactive loop emits nothing; cancelling it seals it.
func TestCancelIdleReapedLoopSeals(t *testing.T) {
	g := newCancelRig(t, cancelRigOpts{completer: script(modelText("hi")), idleTTL: 50 * time.Millisecond})
	ctx := context.Background()
	h, err := g.rt.StartLoop(ctx, LoopConfig{Task: "hello", ModelID: "m", Tenant: acme, Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = h.Wait() // reaped
	if countKind(streamOf(t, g.store, acme, h.ID()), event.KindLoopCancelled) != 0 {
		t.Fatal("an idle reap must not emit loop.cancelled")
	}
	res, err := g.c.Cancel(ctx, acme, h.ID(), "seal", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != CancelOutcomeCancelled || res.WasLive {
		t.Fatalf("result = %+v", res)
	}
	evs := streamOf(t, g.store, acme, h.ID())
	if p := cancelledPayload(t, evs[len(evs)-1]); p.WasLive {
		t.Fatalf("payload = %+v", p)
	}
	if err := g.rt.Send(ctx, acme, h.ID(), "more"); !errors.Is(err, ErrLoopCancelled) {
		t.Fatalf("Send = %v, want ErrLoopCancelled", err)
	}
}

func TestCancelUnknownAndWrongTenant(t *testing.T) {
	bc := newCancelBlockingCompleter()
	g := newCancelRig(t, cancelRigOpts{completer: bc})
	ctx := context.Background()
	h, err := g.rt.StartLoop(ctx, LoopConfig{Task: "work", ModelID: "m", Tenant: acme})
	if err != nil {
		t.Fatal(err)
	}
	<-bc.started
	before := streamOf(t, g.store, acme, h.ID())
	if _, err := g.c.Cancel(ctx, acme, "nope", "r", "alice"); !errors.Is(err, ErrLoopNotFound) {
		t.Fatalf("unknown loop: %v", err)
	}
	if _, err := g.c.Cancel(ctx, "globex", h.ID(), "r", "mallory"); !errors.Is(err, ErrLoopNotFound) {
		t.Fatalf("wrong tenant: %v", err)
	}
	if after := streamOf(t, g.store, acme, h.ID()); len(after) != len(before) {
		t.Fatalf("a refused cancel touched the acme stream: %d -> %d", len(before), len(after))
	}
	// Clean up: the acme loop is still running.
	if _, err := g.c.Cancel(ctx, acme, h.ID(), "cleanup", "alice"); err != nil {
		t.Fatal(err)
	}
	_, _ = h.Wait()
}

// A slow unwind outlasts CancelWait: the call answers Cancelling and the terminal
// event follows.
func TestCancelWaitExpiresReturnsCancelling(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	slow := completerFunc(func(ctx context.Context, _ model.CompletionReq) (model.CompletionResp, error) {
		once.Do(func() { close(started) })
		time.Sleep(300 * time.Millisecond)
		return model.CompletionResp{}, ctx.Err()
	})
	g := newCancelRig(t, cancelRigOpts{completer: slow, cancelWait: 20 * time.Millisecond})
	ctx := context.Background()
	h, err := g.rt.StartLoop(ctx, LoopConfig{Task: "work", ModelID: "m", Tenant: acme})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	res, err := g.c.Cancel(ctx, acme, h.ID(), "slow", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != CancelOutcomeCancelling || res.TerminalSeq != 0 || !res.WasLive || res.Reason != "slow" {
		t.Fatalf("result = %+v, want Cancelling with no seq", res)
	}
	_, _ = h.Wait()
	evs := streamOf(t, g.store, acme, h.ID())
	if p := cancelledPayload(t, evs[len(evs)-1]); !p.WasLive {
		t.Fatalf("payload = %+v", p)
	}
}

// Criterion 11: a drain and an idle reap emit no terminal event.
func TestDrainAndReapEmitNoTerminal(t *testing.T) {
	t.Run("drain", func(t *testing.T) {
		g := newCancelRig(t, cancelRigOpts{completer: script(modelText("hi"))})
		ctx := context.Background()
		h, err := g.rt.StartLoop(ctx, LoopConfig{Task: "hello", ModelID: "m", Tenant: acme, Interactive: true})
		if err != nil {
			t.Fatal(err)
		}
		waitStreamKind(t, g.store, acme, h.ID(), event.KindLoopParked)
		sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := g.rt.(Shutdowner).Shutdown(sctx); err != nil {
			t.Fatal(err)
		}
		if n := countKind(streamOf(t, g.store, acme, h.ID()), event.KindLoopCancelled); n != 0 {
			t.Fatalf("a drain emitted %d loop.cancelled", n)
		}
		// A drained loop stays resumable: Send reports it finished, not cancelled.
		if err := g.rt.Send(ctx, acme, h.ID(), "x"); !errors.Is(err, ErrLoopFinished) {
			t.Fatalf("Send after drain = %v, want ErrLoopFinished", err)
		}
	})
	t.Run("reap", func(t *testing.T) {
		g := newCancelRig(t, cancelRigOpts{completer: script(modelText("hi")), idleTTL: 50 * time.Millisecond})
		h, err := g.rt.StartLoop(context.Background(), LoopConfig{Task: "hello", ModelID: "m", Tenant: acme, Interactive: true})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = h.Wait()
		if n := countKind(streamOf(t, g.store, acme, h.ID()), event.KindLoopCancelled); n != 0 {
			t.Fatalf("a reap emitted %d loop.cancelled", n)
		}
	})
}
