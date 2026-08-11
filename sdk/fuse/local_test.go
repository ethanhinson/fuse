package fuse

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/runtime"
)

// fakeRuntime is a seam-level runtime.Runtime double (mirrors
// internal/loopconnect/handler_test.go's) so the local backend is driven without the
// full agent engine (the real-engine proof is the wire acceptance, Task 6). It backs a
// durable "history" slice and a live channel so Observe(fromSeq) can be exercised for
// subscribe-before-replay + dedup.
type fakeRuntime struct {
	mu   sync.Mutex
	hist []event.Event

	startID  string
	startErr error

	sawStartCfg runtime.LoopConfig
	sawSendTn   event.TenantID
	sawSendLoop string
	sawSendIn   string
	sendErr     error

	// live is handed back by Observe. A test writes to it to force an append into the
	// subscribe→replay window (so a naive replay would double-deliver).
	live chan event.Event

	// observeGate, if non-nil, is closed by Observe right after it returns the live
	// channel but BEFORE the backend calls Attach — a test uses it to land an append in
	// the subscribe→replay window deterministically.
	observeGate func()
}

func (f *fakeRuntime) StartLoop(ctx context.Context, cfg runtime.LoopConfig) (runtime.LoopHandle, error) {
	f.sawStartCfg = cfg
	if f.startErr != nil {
		return nil, f.startErr
	}
	return fakeHandle{id: f.startID}, nil
}
func (f *fakeRuntime) Send(ctx context.Context, tenant event.TenantID, loopID, input string) error {
	f.sawSendTn, f.sawSendLoop, f.sawSendIn = tenant, loopID, input
	return f.sendErr
}
func (f *fakeRuntime) Spawn(ctx context.Context, loopID string, opts runtime.SpawnOpts) (runtime.SpawnHandle, error) {
	return nil, nil
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

// collectSeqs drains ch until it has seen `through` (inclusive) or times out.
func collectSeqs(t *testing.T, ch <-chan Event, through Seq) []Seq {
	t.Helper()
	var seqs []Seq
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				return seqs
			}
			seqs = append(seqs, e.Seq)
			if e.Seq >= through {
				return seqs
			}
		case <-deadline:
			t.Fatalf("timeout collecting through seq %d; saw %v", through, seqs)
			return seqs
		}
	}
}

func assertNoLossNoDup(t *testing.T, seqs []Seq, from, through Seq, who string) {
	t.Helper()
	seen := map[Seq]bool{}
	var prev Seq
	for i, s := range seqs {
		if seen[s] {
			t.Fatalf("%s: seq %d delivered twice in %v", who, s, seqs)
		}
		seen[s] = true
		if i > 0 && s <= prev {
			t.Fatalf("%s: seq %d not strictly increasing after %d in %v", who, s, prev, seqs)
		}
		prev = s
	}
	// Every durable seq in (from, through] must appear exactly once (no loss).
	for want := from + 1; want <= through; want++ {
		if !seen[want] {
			t.Fatalf("%s: missing seq %d (loss) in %v", who, want, seqs)
		}
	}
}

func TestLocalStartSendThreadsTenant(t *testing.T) {
	f := &fakeRuntime{startID: "loop-9"}
	c := NewLocal(f, Credentials{Tenant: "acme", Token: "ignored-locally"})
	ctx := context.Background()

	id, err := c.StartLoop(ctx, StartLoopConfig{Task: "t", Model: "m", Interactive: true})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	if id != "loop-9" {
		t.Fatalf("id = %q, want loop-9", id)
	}
	if f.sawStartCfg.Tenant != event.TenantID("acme") || f.sawStartCfg.Task != "t" || f.sawStartCfg.ModelID != "m" || !f.sawStartCfg.Interactive {
		t.Fatalf("StartLoop cfg not threaded: %+v", f.sawStartCfg)
	}

	if err := c.Send(ctx, "loop-9", "hi"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if f.sawSendTn != event.TenantID("acme") || f.sawSendLoop != "loop-9" || f.sawSendIn != "hi" {
		t.Fatalf("Send not threaded: tn=%q loop=%q in=%q", f.sawSendTn, f.sawSendLoop, f.sawSendIn)
	}
}

// TestLocalObserveReplaysHistory proves Observe(fromSeq) replays durable history since
// fromSeq then live-tails, and that fromSeq is honored (events at or below it are not
// re-sent).
func TestLocalObserveReplaysHistory(t *testing.T) {
	f := &fakeRuntime{live: make(chan event.Event, 8)}
	f.hist = []event.Event{
		{Seq: 1, Kind: event.KindTurnStart},
		{Seq: 2, Kind: event.KindModelCallStart},
		{Seq: 3, Kind: event.KindModelCallEnd},
	}
	c := NewLocal(f, Credentials{Tenant: string(event.DefaultTenant)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, unsub, err := c.Observe(ctx, "loop-1", 1) // from 1 ⇒ expect seqs 2,3 then live 4
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	defer unsub()

	// Emit a live event past the replay watermark.
	f.appendHist(event.Event{Seq: 4, Kind: event.KindTurnEnd})
	f.live <- event.Event{Seq: 4, Kind: event.KindTurnEnd}

	seqs := collectSeqs(t, ch, 4)
	assertNoLossNoDup(t, seqs, 1, 4, "replay+live")
	if seqs[0] != 2 {
		t.Fatalf("first replayed seq = %d, want 2 (fromSeq=1 honored)", seqs[0])
	}
}

// TestLocalObserveReconnectNoLossNoDup is the REQUIRED acceptance: a reconnect
// Observe(fromSeq) must deliver every event exactly once across the subscribe→replay
// gap. We force an append into the subscribe→replay window (via observeGate) that is
// ALSO in durable history, so a naive replay would double-deliver it — the watermark
// dedup must drop the duplicate.
func TestLocalObserveReconnectNoLossNoDup(t *testing.T) {
	live := make(chan event.Event, 8)
	f := &fakeRuntime{live: live}
	f.hist = []event.Event{
		{Seq: 1, Kind: event.KindTurnStart},
		{Seq: 2, Kind: event.KindModelCallStart},
	}

	// observeGate fires between subscribe and Attach. It appends seq 3 to BOTH the live
	// channel and durable history: Attach will replay it (Seq 3 > fromSeq 0) AND the
	// live tail will carry it — a naive impl double-delivers seq 3.
	var once sync.Once
	f.observeGate = func() {
		once.Do(func() {
			f.appendHist(event.Event{Seq: 3, Kind: event.KindModelCallEnd})
			live <- event.Event{Seq: 3, Kind: event.KindModelCallEnd}
		})
	}

	c := NewLocal(f, Credentials{Tenant: string(event.DefaultTenant)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, unsub, err := c.Observe(ctx, "loop-1", 0)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	defer unsub()

	// A genuinely-new live event past the overlap so the collector has a terminal seq.
	f.appendHist(event.Event{Seq: 4, Kind: event.KindTurnEnd})
	live <- event.Event{Seq: 4, Kind: event.KindTurnEnd}

	seqs := collectSeqs(t, ch, 4)
	assertNoLossNoDup(t, seqs, 0, 4, "reconnect")
}
