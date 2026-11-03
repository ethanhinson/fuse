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
	// build, when set, runs inside BuildAgent with the loop's store, tree and tool
	// registry (for tools that need them, such as a spawning tool).
	build func(store event.EventStore, tree *agent.AgentTree, reg *tools.Registry)
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
			if o.build != nil {
				o.build(store, tree, reg)
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

// spawnChildTool spawns one child through a real Spawner and returns as soon as its
// own ctx ends, without waiting for the child: the child is still unwinding when
// the root run returns, so only the runtime's wait orders its spawn.done first.
type spawnChildTool struct {
	store event.EventStore
	tree  *agent.AgentTree
	cb    agent.ChildBuilder
}

func (s *spawnChildTool) Name() string               { return "spawn_child" }
func (s *spawnChildTool) Description() string        { return "spawns a child" }
func (s *spawnChildTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (s *spawnChildTool) Execute(ctx context.Context, _ string) tools.Result {
	sp := agent.NewSpawner(
		agent.WithTree(s.tree),
		agent.WithNode(s.tree.Node(s.tree.RootID())),
		agent.WithSpawnDepth(0),
		agent.WithEventStore(s.store),
		agent.WithChildBuilder(s.cb),
	)
	if _, err := sp.Spawn(ctx, agent.SpawnOpts{Label: "kid", Task: "wait"}); err != nil {
		return tools.Result{Output: err.Error(), IsError: true}
	}
	<-ctx.Done()
	return tools.Result{Output: "parent stopped"}
}

// Criterion 7: a child running when the loop is cancelled records spawn.done (with
// its cancellation error) before loop.cancelled, and its scheduler slot is free
// once Cancel returns.
func TestCancelWhileChildRunning(t *testing.T) {
	childStarted := make(chan struct{})
	var once sync.Once
	cb := func(ctx context.Context, _ agent.SpawnOpts, _ *agent.AgentNode, _ *agent.AgentTree) (string, error) {
		once.Do(func() { close(childStarted) })
		<-ctx.Done()
		// Linger past the cancel so the root run finishes first.
		time.Sleep(100 * time.Millisecond)
		return "", ctx.Err()
	}
	var mu sync.Mutex
	var tree *agent.AgentTree
	g := newCancelRig(t, cancelRigOpts{
		completer: script(modelToolCall("c1", "spawn_child", "{}")),
		build: func(store event.EventStore, tr *agent.AgentTree, reg *tools.Registry) {
			mu.Lock()
			tree = tr
			mu.Unlock()
			reg.Register(&spawnChildTool{store: store, tree: tr, cb: cb})
		},
	})
	ctx := context.Background()
	h, err := g.rt.StartLoop(ctx, LoopConfig{Task: "work", ModelID: "m", Tenant: acme})
	if err != nil {
		t.Fatal(err)
	}
	<-childStarted
	res, err := g.c.Cancel(ctx, acme, h.ID(), "stop the family", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != CancelOutcomeCancelled || !res.WasLive {
		t.Fatalf("result = %+v", res)
	}
	mu.Lock()
	tr := tree
	mu.Unlock()
	if n := tr.Scheduler().Snapshot().SlotsInUse; n != 0 {
		t.Fatalf("SlotsInUse = %d after Cancel returned, want 0", n)
	}
	if running, pending := tr.ActiveCounts(); running != 0 || pending != 0 {
		t.Fatalf("ActiveCounts = (%d, %d) after Cancel returned, want (0, 0)", running, pending)
	}
	_, _ = h.Wait()
	evs := streamOf(t, g.store, acme, h.ID())
	doneAt := -1
	for i, e := range evs {
		if e.Kind != event.KindSpawnDone {
			continue
		}
		var p event.SpawnDonePayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Err == "" {
			t.Fatalf("spawn.done = %+v, want a cancellation error", p)
		}
		doneAt = i
	}
	cancelledAt := indexOfKind(evs, event.KindLoopCancelled)
	if doneAt < 0 || cancelledAt < 0 || doneAt > cancelledAt {
		t.Fatalf("spawn.done (index %d) must precede loop.cancelled (index %d)", doneAt, cancelledAt)
	}
	if lastKind(evs) != event.KindLoopCancelled {
		t.Fatalf("last event = %s, want loop.cancelled", lastKind(evs))
	}
}

// ---- Loops not in this process's cache: cross-replica relay and resume refusal ----

// appendRaw appends one event straight to the durable store, as another replica (or
// an operator's tooling) would, and returns the seq the store gave it.
func appendRaw(t *testing.T, store *fsstore.FSDurableStore, loopID string, k event.Kind, payload any) event.Seq {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	key := event.StreamKey{Tenant: event.DefaultTenant, Loop: event.LoopID(loopID)}
	got, err := store.AppendCommitted(context.Background(), key, event.Event{NodeID: loopID, Kind: k, Payload: raw})
	if err != nil {
		t.Fatalf("append %s: %v", k, err)
	}
	return got.Seq
}

func withCancelWait(d time.Duration) func(*Deps) {
	return func(deps *Deps) { deps.CancelWait = d }
}

func cancelRequests(t *testing.T, evs []event.Event) []event.Event {
	t.Helper()
	var out []event.Event
	for _, e := range evs {
		if e.Kind == event.KindLoopCancelRequest {
			out = append(out, e)
		}
	}
	return out
}

// A cancel that reaches the replica NOT running the loop is relayed to the owner
// through the durable stream: the owner unwinds the loop and appends the terminal
// event, and the requester reports Cancelled only once it saw that event and the
// registry shows the loop not live.
func TestCancelOnNonOwnerRelays(t *testing.T) {
	f := &steerFake{targetBlock: make(chan struct{})}
	rtA, rtB, store := twoReplicasWith(t, f, nil, withCancelWait(5*time.Second))
	ctx := context.Background()
	h, err := rtA.StartLoop(ctx, LoopConfig{Task: "build it", ModelID: "target", Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	<-f.targetBlock // A's model call is in flight

	resB, err := rtB.(Canceller).Cancel(ctx, event.DefaultTenant, h.ID(), "stop", "alice")
	if err != nil {
		t.Fatalf("Cancel on the other replica: %v", err)
	}
	liveAtReturn := isLive(t, store, event.DefaultTenant, h.ID())
	if resB.Outcome != CancelOutcomeCancelled || !resB.WasLive || resB.Reason != "stop" {
		t.Fatalf("relayed cancel = %+v, want Cancelled, WasLive, reason stop", resB)
	}
	if liveAtReturn {
		t.Fatal("a relayed Cancelled must mean the registry shows the loop not live")
	}

	evs := streamOf(t, store, event.DefaultTenant, h.ID())
	reqs := cancelRequests(t, evs)
	if len(reqs) != 1 {
		t.Fatalf("want exactly one loop.cancel.request, got %d", len(reqs))
	}
	var rp event.LoopCancelRequestPayload
	if err := json.Unmarshal(reqs[0].Payload, &rp); err != nil {
		t.Fatal(err)
	}
	if rp.Reason != "stop" || rp.RequestedBy != "alice" {
		t.Fatalf("request payload = %+v", rp)
	}
	last := evs[len(evs)-1]
	p := cancelledPayload(t, last)
	if p.Reason != "stop" || p.RequestedBy != "alice" || !p.WasLive || p.RequestSeq != uint64(reqs[0].Seq) {
		t.Fatalf("terminal payload = %+v, want stop/alice/was_live/request_seq %d", p, reqs[0].Seq)
	}
	if resB.TerminalSeq != last.Seq {
		t.Fatalf("TerminalSeq = %d, want the stream's last seq %d", resB.TerminalSeq, last.Seq)
	}
	if n := countKind(evs, event.KindLoopCancelled); n != 1 {
		t.Fatalf("want one loop.cancelled, got %d", n)
	}

	if err := rtA.Send(ctx, event.DefaultTenant, h.ID(), "hi"); !errors.Is(err, ErrLoopCancelled) {
		t.Fatalf("Send on the owner = %v, want ErrLoopCancelled", err)
	}
	if err := rtB.Send(ctx, event.DefaultTenant, h.ID(), "hi"); !errors.Is(err, ErrLoopFinished) {
		t.Fatalf("Send on the other replica = %v, want ErrLoopFinished", err)
	}
	if _, err := rtB.Resume(ctx, event.DefaultTenant, h.ID()); !errors.Is(err, ErrLoopCancelled) {
		t.Fatalf("Resume on the other replica = %v, want ErrLoopCancelled", err)
	}
	if got := len(streamOf(t, store, event.DefaultTenant, h.ID())); got != len(evs) {
		t.Fatalf("the refused resume appended events (%d -> %d)", len(evs), got)
	}
}

// While a relayed request is unanswered, a further cancel from the same process does
// not append another request; once the loop.cancelled lands it reports that one.
func TestCancelRelayPendingAppendsOnce(t *testing.T) {
	f := &steerFake{}
	_, rtB, store := twoReplicasWith(t, f, nil, withCancelWait(30*time.Millisecond))
	ctx := context.Background()
	const id = "ghost-loop"
	key := event.StreamKey{Tenant: event.DefaultTenant, Loop: id}
	if err := store.Register(ctx, event.LoopRecord{Key: key, OwnerNodeID: "ghost", Live: true, LeaseExpiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	c := rtB.(Canceller)

	for i := 0; i < 2; i++ {
		res, err := c.Cancel(ctx, event.DefaultTenant, id, "stop", "alice")
		if err != nil {
			t.Fatalf("cancel %d: %v", i, err)
		}
		if res.Outcome != CancelOutcomeCancelling || res.TerminalSeq != 0 {
			t.Fatalf("cancel %d = %+v, want Cancelling with no terminal seq", i, res)
		}
	}
	if n := len(cancelRequests(t, streamOf(t, store, event.DefaultTenant, id))); n != 1 {
		t.Fatalf("want exactly one loop.cancel.request, got %d", n)
	}

	seq := appendRaw(t, store, id, event.KindLoopCancelled, event.LoopCancelledPayload{Reason: "owner says", RequestedBy: "alice", WasLive: true, RequestSeq: 1})
	for i := 0; i < 2; i++ {
		res, err := c.Cancel(ctx, event.DefaultTenant, id, "again", "bob")
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != CancelOutcomeAlreadyCancelled || res.Reason != "owner says" || res.TerminalSeq != seq || !res.WasLive {
			t.Fatalf("cancel after the terminal (%d) = %+v, want AlreadyCancelled owner says @%d", i, res, seq)
		}
	}
	evs := streamOf(t, store, event.DefaultTenant, id)
	if n := len(cancelRequests(t, evs)); n != 1 {
		t.Fatalf("a cancel after the terminal appended a request (%d)", n)
	}
	if lastKind(evs) != event.KindLoopCancelled {
		t.Fatalf("stream ends with %s", lastKind(evs))
	}
}

// parkAndShutdown starts an interactive loop on rt, waits for it to park, then shuts
// rt down: the loop is recorded not live with no terminal event.
func parkAndShutdown(t *testing.T, rt Runtime, store *fsstore.FSDurableStore) string {
	t.Helper()
	ctx := context.Background()
	h, err := rt.StartLoop(ctx, LoopConfig{Task: "chat", ModelID: "target", Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	waitStreamKind(t, store, event.DefaultTenant, h.ID(), event.KindLoopParked)
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rt.(Shutdowner).Shutdown(sctx); err != nil {
		t.Fatal(err)
	}
	if isLive(t, store, event.DefaultTenant, h.ID()) {
		t.Fatal("a shut-down loop must be recorded not live")
	}
	return h.ID()
}

// A request nobody answered (the owner went away first) still seals the loop: a
// resume answers it with the terminal event instead of reviving the loop.
func TestResumeRefusesUnansweredCancelRequest(t *testing.T) {
	f := &steerFake{}
	rtA, rtB, store := twoReplicas(t, f, nil)
	ctx := context.Background()
	id := parkAndShutdown(t, rtA, store)
	reqSeq := appendRaw(t, store, id, event.KindLoopCancelRequest, event.LoopCancelRequestPayload{Reason: "r", RequestedBy: "bob"})

	if _, err := rtB.Resume(ctx, event.DefaultTenant, id); !errors.Is(err, ErrLoopCancelled) {
		t.Fatalf("Resume with an unanswered cancel request = %v, want ErrLoopCancelled", err)
	}
	if isLive(t, store, event.DefaultTenant, id) {
		t.Fatal("a refused resume launched the loop")
	}
	evs := streamOf(t, store, event.DefaultTenant, id)
	p := cancelledPayload(t, evs[len(evs)-1])
	if p.WasLive || p.RequestSeq != uint64(reqSeq) || p.Reason != "r" || p.RequestedBy != "bob" {
		t.Fatalf("terminal payload = %+v, want not live, request_seq %d, r/bob", p, reqSeq)
	}

	if _, err := rtB.Resume(ctx, event.DefaultTenant, id); !errors.Is(err, ErrLoopCancelled) {
		t.Fatalf("second Resume = %v, want ErrLoopCancelled", err)
	}
	if got := len(streamOf(t, store, event.DefaultTenant, id)); got != len(evs) {
		t.Fatalf("the second resume appended events (%d -> %d)", len(evs), got)
	}
}

// A stream that already ends in loop.cancelled is never resumed.
func TestResumeRefusesCancelledStream(t *testing.T) {
	f := &steerFake{}
	rtA, rtB, store := twoReplicas(t, f, nil)
	ctx := context.Background()
	id := parkAndShutdown(t, rtA, store)
	appendRaw(t, store, id, event.KindLoopCancelled, event.LoopCancelledPayload{Reason: "done with it"})
	n := len(streamOf(t, store, event.DefaultTenant, id))

	// On B, which never held the loop. (A still caches its finished loop, and Resume
	// never relaunches a cached loop.)
	for i := 0; i < 2; i++ {
		if _, err := rtB.Resume(ctx, event.DefaultTenant, id); !errors.Is(err, ErrLoopCancelled) {
			t.Fatalf("Resume of a cancelled stream = %v, want ErrLoopCancelled", err)
		}
	}
	if isLive(t, store, event.DefaultTenant, id) {
		t.Fatal("a refused resume launched the loop")
	}
	if got := len(streamOf(t, store, event.DefaultTenant, id)); got != n {
		t.Fatalf("a refused resume appended events (%d -> %d)", n, got)
	}
}

// A loop that finished on another process is sealed by a replica that never held it.
func TestCancelNotLiveOnColdReplica(t *testing.T) {
	f := &steerFake{}
	rtA, rtB, store := twoReplicas(t, f, nil)
	ctx := context.Background()
	h, err := rtA.StartLoop(ctx, LoopConfig{Task: "answer", ModelID: "target"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatal(err)
	}
	c := rtB.(Canceller)
	res, err := c.Cancel(ctx, event.DefaultTenant, h.ID(), "cleanup", "carol")
	if err != nil {
		t.Fatal(err)
	}
	evs := streamOf(t, store, event.DefaultTenant, h.ID())
	last := evs[len(evs)-1]
	if res.Outcome != CancelOutcomeCancelled || res.WasLive || res.Reason != "cleanup" || res.TerminalSeq != last.Seq {
		t.Fatalf("cold cancel = %+v, want Cancelled, not live, cleanup @%d", res, last.Seq)
	}
	p := cancelledPayload(t, last)
	if p.WasLive || p.Reason != "cleanup" || p.RequestedBy != "carol" || p.RequestSeq != 0 {
		t.Fatalf("terminal payload = %+v", p)
	}

	again, err := c.Cancel(ctx, event.DefaultTenant, h.ID(), "again", "dave")
	if err != nil {
		t.Fatal(err)
	}
	if again.Outcome != CancelOutcomeAlreadyCancelled || again.TerminalSeq != last.Seq || again.Reason != "cleanup" {
		t.Fatalf("repeat cold cancel = %+v, want AlreadyCancelled cleanup @%d", again, last.Seq)
	}
	if got := streamOf(t, store, event.DefaultTenant, h.ID()); len(got) != len(evs) {
		for _, e := range got {
			t.Logf("%d %s %s", e.Seq, e.Kind, e.Payload)
		}
		t.Fatalf("the repeat cancel appended events (%d -> %d)", len(evs), len(got))
	}
	if _, err := rtB.Resume(ctx, event.DefaultTenant, h.ID()); !errors.Is(err, ErrLoopCancelled) {
		t.Fatalf("Resume after a cold cancel = %v, want ErrLoopCancelled", err)
	}
}

// Concurrent cold cancels in one process seal the loop once.
func TestCancelColdConcurrentAppendsOnce(t *testing.T) {
	f := &steerFake{}
	rtA, rtB, store := twoReplicas(t, f, nil)
	ctx := context.Background()
	h, err := rtA.StartLoop(ctx, LoopConfig{Task: "answer", ModelID: "target"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatal(err)
	}
	c := rtB.(Canceller)
	const n = 8
	results := make([]CancelResult, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := c.Cancel(ctx, event.DefaultTenant, h.ID(), "race", "erin")
			if err != nil {
				t.Errorf("cancel %d: %v", i, err)
			}
			results[i] = res
		}(i)
	}
	wg.Wait()
	cancelled := 0
	for _, r := range results {
		switch r.Outcome {
		case CancelOutcomeCancelled:
			cancelled++
		case CancelOutcomeAlreadyCancelled:
		default:
			t.Fatalf("unexpected outcome %+v", r)
		}
	}
	if cancelled != 1 {
		t.Fatalf("want exactly one Cancelled, got %d", cancelled)
	}
	if k := countKind(streamOf(t, store, event.DefaultTenant, h.ID()), event.KindLoopCancelled); k != 1 {
		t.Fatalf("want one loop.cancelled, got %d", k)
	}
}

// Another tenant cannot cancel a loop it does not own, live or not.
func TestCancelColdWrongTenant(t *testing.T) {
	f := &steerFake{}
	rtA, rtB, store := twoReplicas(t, f, nil)
	ctx := context.Background()
	h, err := rtA.StartLoop(ctx, LoopConfig{Task: "answer", ModelID: "target"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatal(err)
	}
	n := len(streamOf(t, store, event.DefaultTenant, h.ID()))
	if _, err := rtB.(Canceller).Cancel(ctx, "globex", h.ID(), "x", "mallory"); !errors.Is(err, ErrLoopNotFound) {
		t.Fatalf("wrong-tenant cold cancel = %v, want ErrLoopNotFound", err)
	}
	if got := len(streamOf(t, store, event.DefaultTenant, h.ID())); got != n {
		t.Fatalf("a wrong-tenant cancel appended events (%d -> %d)", n, got)
	}
}

func mkEv(seq event.Seq, k event.Kind, payload any) event.Event {
	raw, _ := json.Marshal(payload)
	return event.Event{Seq: seq, Kind: k, Payload: raw}
}

// pendingCancel finds the first request no terminal event answers; firstCancelled
// finds the first terminal event (the authoritative one if two exist).
func TestPendingCancelAndFirstCancelled(t *testing.T) {
	req := func(seq event.Seq, reason string) event.Event {
		return mkEv(seq, event.KindLoopCancelRequest, event.LoopCancelRequestPayload{Reason: reason, RequestedBy: "u"})
	}
	done := func(seq event.Seq, reason string) event.Event {
		return mkEv(seq, event.KindLoopCancelled, event.LoopCancelledPayload{Reason: reason})
	}
	base := []event.Event{mkEv(1, event.KindUserInput, event.UserInputPayload{Content: "t"}), mkEv(2, event.KindLoopParked, nil)}

	if _, _, ok := pendingCancel(base); ok {
		t.Fatal("no request: nothing pending")
	}
	open := append(append([]event.Event{}, base...), req(3, "a"), req(4, "b"))
	e, p, ok := pendingCancel(open)
	if !ok || e.Seq != 3 || p.Reason != "a" {
		t.Fatalf("pendingCancel = %d %+v %v, want the first request (3, a)", e.Seq, p, ok)
	}
	answered := append(append([]event.Event{}, open...), done(5, "a"))
	if _, _, ok := pendingCancel(answered); ok {
		t.Fatal("an answered request is not pending")
	}
	if _, _, ok := firstCancelled(open); ok {
		t.Fatal("no terminal event yet")
	}
	twice := append(append([]event.Event{}, answered...), done(6, "late"))
	e, cp, ok := firstCancelled(twice)
	if !ok || e.Seq != 5 || cp.Reason != "a" {
		t.Fatalf("firstCancelled = %d %+v %v, want the first terminal (5, a)", e.Seq, cp, ok)
	}
	malformed := append(append([]event.Event{}, base...), event.Event{Seq: 3, Kind: event.KindLoopCancelRequest, Payload: []byte("{")})
	if _, _, ok := pendingCancel(malformed); ok {
		t.Fatal("a malformed request is skipped")
	}
}

// The owner's relay pump hands every cancel request to onCancel: one already in the
// stream when the pump starts, and one that arrives later. A terminal event another
// replica wrote while this one was starting the loop is honoured too, so a resume
// racing a cold seal never leaves a cancelled loop running.
func TestOwnerRelayPumpHonoursCancelRequests(t *testing.T) {
	store := fsstore.NewDurableFSStore(t.TempDir())
	r := New(Deps{DurableStore: store, Registry: store, RelayPoll: 20 * time.Millisecond}).(*inProcRuntime)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var got []relayedCancel
	onCancel := func(c relayedCancel) {
		mu.Lock()
		got = append(got, c)
		mu.Unlock()
	}
	calls := func() []relayedCancel {
		mu.Lock()
		defer mu.Unlock()
		return append([]relayedCancel(nil), got...)
	}
	start := func(id string) chan struct{} {
		tree := agent.NewAgentTree("root", "m")
		stop := make(chan struct{})
		r.startRelayPump(ctx, event.StreamKey{Tenant: event.DefaultTenant, Loop: event.LoopID(id)}, agent.NewHumanBus(tree), tree.RootID(), stop, onCancel)
		return stop
	}

	// A request already pending when the pump starts.
	s1 := appendRaw(t, store, "pre", event.KindLoopCancelRequest, event.LoopCancelRequestPayload{Reason: "early", RequestedBy: "u1"})
	stop := start("pre")
	eventually(t, func() bool { return len(calls()) == 1 })
	if c := calls()[0]; c.requestSeq != s1 || c.sealedSeq != 0 || c.reason != "early" || c.requestedBy != "u1" {
		t.Fatalf("first-pass cancel = %+v", c)
	}
	// One that arrives while the pump runs.
	s2 := appendRaw(t, store, "pre", event.KindLoopCancelRequest, event.LoopCancelRequestPayload{Reason: "late", RequestedBy: "u2"})
	eventually(t, func() bool { return len(calls()) == 2 })
	if c := calls()[1]; c.requestSeq != s2 || c.sealedSeq != 0 || c.reason != "late" {
		t.Fatalf("later cancel = %+v", c)
	}
	close(stop)

	// A stream someone already sealed while this replica was starting it.
	appendRaw(t, store, "sealed", event.KindLoopCancelRequest, event.LoopCancelRequestPayload{Reason: "x"})
	sealSeq := appendRaw(t, store, "sealed", event.KindLoopCancelled, event.LoopCancelledPayload{Reason: "cold seal", RequestedBy: "u3", RequestSeq: 1})
	stop = start("sealed")
	eventually(t, func() bool { return len(calls()) == 3 })
	if c := calls()[2]; c.requestSeq != 1 || c.sealedSeq != sealSeq || c.reason != "cold seal" || c.requestedBy != "u3" {
		t.Fatalf("sealed-stream cancel = %+v", c)
	}
	close(stop)

	// A pump with nothing to cancel never calls onCancel.
	appendRaw(t, store, "quiet", event.KindUserInput, event.UserInputPayload{Content: "hi"})
	stop = start("quiet")
	time.Sleep(100 * time.Millisecond)
	close(stop)
	if n := len(calls()); n != 3 {
		t.Fatalf("a stream with no cancel triggered onCancel (%d calls)", n)
	}
}

// A loop.cancelled another replica wrote reaches the owner's pump after the owner's
// run already returned (the pump's last pass races its stop). The owner seals the loop
// in memory and appends no second terminal event.
func TestRelayedSealOfFinishedLoopAppendsNothing(t *testing.T) {
	f := &steerFake{}
	rtA, _, store := twoReplicas(t, f, nil)
	ctx := context.Background()
	h, err := rtA.StartLoop(ctx, LoopConfig{Task: "answer", ModelID: "target"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatal(err)
	}
	seq := appendRaw(t, store, h.ID(), event.KindLoopCancelled, event.LoopCancelledPayload{Reason: "cold", RequestedBy: "u"})
	n := len(streamOf(t, store, event.DefaultTenant, h.ID()))

	r := rtA.(*inProcRuntime)
	lp := r.loopCache(event.DefaultTenant, h.ID())
	if lp == nil {
		t.Fatal("A must still cache its finished loop")
	}
	r.cancelRelayed(lp, relayedCancel{reason: "cold", requestedBy: "u", sealedSeq: seq})

	if got := len(streamOf(t, store, event.DefaultTenant, h.ID())); got != n {
		t.Fatalf("a relayed seal of a finished loop appended events (%d -> %d)", n, got)
	}
	if err := rtA.Send(ctx, event.DefaultTenant, h.ID(), "hi"); !errors.Is(err, ErrLoopCancelled) {
		t.Fatalf("Send after a relayed seal = %v, want ErrLoopCancelled", err)
	}
	res, err := r.Cancel(ctx, event.DefaultTenant, h.ID(), "again", "v")
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != CancelOutcomeAlreadyCancelled || res.TerminalSeq != seq || res.Reason != "cold" {
		t.Fatalf("Cancel after a relayed seal = %+v, want AlreadyCancelled cold @%d", res, seq)
	}
}

// A replica keeps a loop that finished on it in its cache. Once another replica has
// sealed the stream, a cancel here reports that seal (AlreadyCancelled, the stream's
// terminal event) and appends no second loop.cancelled.
func TestCancelFinishedCachedLoopSealedElsewhere(t *testing.T) {
	f := &steerFake{}
	rtA, rtB, store := twoReplicas(t, f, nil)
	ctx := context.Background()
	h, err := rtA.StartLoop(ctx, LoopConfig{Task: "answer", ModelID: "target"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatal(err)
	}
	sealed, err := rtB.(Canceller).Cancel(ctx, event.DefaultTenant, h.ID(), "cold", "carol")
	if err != nil {
		t.Fatal(err)
	}
	if sealed.Outcome != CancelOutcomeCancelled || sealed.TerminalSeq == 0 {
		t.Fatalf("seal on B = %+v, want Cancelled with a seq", sealed)
	}
	n := len(streamOf(t, store, event.DefaultTenant, h.ID()))

	if rtA.(*inProcRuntime).loopCache(event.DefaultTenant, h.ID()) == nil {
		t.Fatal("A must still cache its finished loop")
	}
	for i := 0; i < 2; i++ {
		res, err := rtA.(Canceller).Cancel(ctx, event.DefaultTenant, h.ID(), "late", "dave")
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != CancelOutcomeAlreadyCancelled || res.WasLive || res.TerminalSeq != sealed.TerminalSeq || res.Reason != "cold" {
			t.Fatalf("cancel %d on A = %+v, want AlreadyCancelled cold @%d", i, res, sealed.TerminalSeq)
		}
	}
	evs := streamOf(t, store, event.DefaultTenant, h.ID())
	if k := countKind(evs, event.KindLoopCancelled); k != 1 || len(evs) != n {
		t.Fatalf("A's cancel appended: %d loop.cancelled, %d -> %d events", k, n, len(evs))
	}
	if err := rtA.Send(ctx, event.DefaultTenant, h.ID(), "hi"); !errors.Is(err, ErrLoopCancelled) {
		t.Fatalf("Send on A after the seal = %v, want ErrLoopCancelled", err)
	}
}

// A loop that finished on A and was then resumed on B runs on B, though A still
// caches the finished loop. A cancel on A is relayed to B and reports Cancelled only
// once B unwound the loop and it reads not live.
func TestCancelFinishedCachedLoopResumedElsewhereRelays(t *testing.T) {
	f := &steerFake{}
	rtA, rtB, store := twoReplicasWith(t, f, nil, withCancelWait(5*time.Second))
	ctx := context.Background()
	h, err := rtA.StartLoop(ctx, LoopConfig{Task: "answer", ModelID: "target"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Wait(); err != nil {
		t.Fatal(err)
	}
	hb, err := rtB.Resume(ctx, event.DefaultTenant, h.ID())
	if err != nil {
		t.Fatalf("Resume on B: %v", err)
	}
	if !isLive(t, store, event.DefaultTenant, h.ID()) {
		t.Fatal("the resumed loop must read live")
	}
	if rtA.(*inProcRuntime).loopCache(event.DefaultTenant, h.ID()) == nil {
		t.Fatal("A must still cache its finished loop")
	}

	res, err := rtA.(Canceller).Cancel(ctx, event.DefaultTenant, h.ID(), "stop", "erin")
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != CancelOutcomeCancelled || !res.WasLive || res.Reason != "stop" {
		t.Fatalf("cancel on A = %+v, want Cancelled, was live, stop", res)
	}
	if isLive(t, store, event.DefaultTenant, h.ID()) {
		t.Fatal("Cancelled reported while the loop still reads live")
	}
	waitDone := make(chan struct{})
	go func() { _, _ = hb.Wait(); close(waitDone) }()
	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("B's resumed loop did not end")
	}

	evs := streamOf(t, store, event.DefaultTenant, h.ID())
	reqs := cancelRequests(t, evs)
	if len(reqs) != 1 || countKind(evs, event.KindLoopCancelled) != 1 || lastKind(evs) != event.KindLoopCancelled {
		t.Fatalf("want one request and one terminal loop.cancelled, got %d / %d (last %s)",
			len(reqs), countKind(evs, event.KindLoopCancelled), lastKind(evs))
	}
	last := evs[len(evs)-1]
	if p := cancelledPayload(t, last); !p.WasLive || p.RequestSeq != uint64(reqs[0].Seq) || res.TerminalSeq != last.Seq {
		t.Fatalf("terminal payload = %+v @%d, result %+v, request @%d", p, last.Seq, res, reqs[0].Seq)
	}

	again, err := rtA.(Canceller).Cancel(ctx, event.DefaultTenant, h.ID(), "again", "erin")
	if err != nil {
		t.Fatal(err)
	}
	if again.Outcome != CancelOutcomeAlreadyCancelled || again.TerminalSeq != last.Seq {
		t.Fatalf("repeat cancel on A = %+v, want AlreadyCancelled @%d", again, last.Seq)
	}
	if got := len(streamOf(t, store, event.DefaultTenant, h.ID())); got != len(evs) {
		t.Fatalf("the repeat cancel appended events (%d -> %d)", len(evs), got)
	}
}
