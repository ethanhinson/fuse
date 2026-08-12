package observe

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
)

func TestRunnerSubscribeFirstReplayLiveExactlyOnceAndTenantScoped(t *testing.T) {
	store := newInterleavingStore()
	keyA := event.StreamKey{Tenant: "a", Loop: "shared"}
	keyB := event.StreamKey{Tenant: "b", Loop: "shared"}
	store.history[keyA] = []event.Event{{Seq: 1, Kind: event.KindTurnStart}}
	store.history[keyB] = []event.Event{{Seq: 1, Kind: event.KindTurnStart}}
	var mu sync.Mutex
	got := make([]Record, 0, 3)
	p := ProjectorFunc(func(_ context.Context, r Record) error { mu.Lock(); got = append(got, r); mu.Unlock(); return nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runnerA := NewRunner(store, keyA, p, nil)
	done := make(chan error, 1)
	go func() { done <- runnerA.Run(ctx) }()
	<-store.subscribed
	store.live[keyA] <- event.Event{Seq: 2, Kind: event.KindModelCallEnd} // arrives before replay returns
	store.replayGate <- struct{}{}
	deadline := time.After(time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("runner did not project replay/live events: %#v", got)
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil && err != context.Canceled {
		t.Fatal(err)
	}
	runnerB := NewRunner(store, keyB, p, nil)
	ctxB, cancelB := context.WithCancel(context.Background())
	doneB := make(chan error, 1)
	go func() { doneB <- runnerB.Run(ctxB) }()
	<-store.subscribed
	store.replayGate <- struct{}{}
	deadline = time.After(time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("tenant B collided: %#v", got)
		case <-time.After(time.Millisecond):
		}
	}
	cancelB()
	<-doneB
}

func TestRunnerReportsProjectionFailuresAndContinues(t *testing.T) {
	store := newInterleavingStore()
	key := event.StreamKey{Tenant: "a", Loop: "l"}
	store.history[key] = []event.Event{{Seq: 1, Kind: event.KindError}, {Seq: 2, Kind: event.KindTurnEnd}}
	var mu sync.Mutex
	var failures []ProjectionFailure
	var count int
	runner := NewRunner(store, key, ProjectorFunc(func(context.Context, Record) error {
		mu.Lock()
		defer mu.Unlock()
		count++
		if count == 1 {
			return assertError{}
		}
		return nil
	}), func(f ProjectionFailure) { mu.Lock(); failures = append(failures, f); mu.Unlock() })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	<-store.subscribed
	store.replayGate <- struct{}{}
	deadline := time.After(time.Second)
	for {
		mu.Lock()
		failureCount, projected := len(failures), count
		mu.Unlock()
		if failureCount == 1 && projected == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("projection failure was not reported or stream did not continue")
		case <-time.After(time.Millisecond):
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if failures[0].Category != ErrorCategoryProjection || failures[0].Sequence != 1 {
		t.Fatalf("failure=%#v", failures[0])
	}
	cancel()
	<-done
}

func TestRunnerCloseStopsLiveWait(t *testing.T) {
	store := newInterleavingStore()
	runner := NewRunner(store, event.StreamKey{Tenant: "a", Loop: "l"}, ProjectorFunc(func(context.Context, Record) error { return nil }), nil)
	done := make(chan error, 1)
	go func() { done <- runner.Run(context.Background()) }()
	<-store.subscribed
	store.replayGate <- struct{}{}
	runner.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not stop runner")
	}
}

type carrierStartRecorder struct{ delayed []bool }

func (o *carrierStartRecorder) Start(ctx context.Context, _ Descriptor) (context.Context, Handle) {
	return ctx, NoopHandle{}
}

func (o *carrierStartRecorder) StartFromCarrier(ctx context.Context, c *event.TraceCarrier, delayed bool, _ Descriptor) (context.Context, Handle) {
	if c == nil || c.Validate() != nil {
		panic("runner started from invalid carrier")
	}
	o.delayed = append(o.delayed, delayed)
	return ctx, NoopHandle{}
}

// TestRunnerUsesImmediateAndDelayedCarrierStarts proves the durable consumer
// preserves parentage for live work and uses a link-safe delayed start on replay.
func TestRunnerUsesImmediateAndDelayedCarrierStarts(t *testing.T) {
	carrier := &event.TraceCarrier{TraceParent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}
	observer := &carrierStartRecorder{}
	runner := NewRunner(nil, event.StreamKey{Tenant: "a", Loop: "l"}, ProjectorFunc(func(context.Context, Record) error { return nil }), nil, observer)
	dedup := newDeduper(2)
	runner.project(context.Background(), event.Event{Seq: 1, Kind: event.KindTurnStart, Trace: carrier}, dedup, true)
	runner.project(context.Background(), event.Event{Seq: 2, Kind: event.KindTurnEnd, Trace: carrier}, dedup, false)
	if got, want := len(observer.delayed), 2; got != want || !observer.delayed[0] || observer.delayed[1] {
		t.Fatalf("delayed starts = %v, want [true false]", observer.delayed)
	}
}

type interleavingStore struct {
	history    map[event.StreamKey][]event.Event
	live       map[event.StreamKey]chan event.Event
	subscribed chan struct{}
	replayGate chan struct{}
}

func newInterleavingStore() *interleavingStore {
	return &interleavingStore{history: map[event.StreamKey][]event.Event{}, live: map[event.StreamKey]chan event.Event{}, subscribed: make(chan struct{}, 2), replayGate: make(chan struct{}, 2)}
}
func (s *interleavingStore) Append(context.Context, event.StreamKey, event.Event) error { return nil }
func (s *interleavingStore) Subscribe(_ context.Context, key event.StreamKey) (<-chan event.Event, func(), error) {
	ch := make(chan event.Event, 4)
	s.live[key] = ch
	s.subscribed <- struct{}{}
	return ch, func() {}, nil
}
func (s *interleavingStore) Replay(_ context.Context, key event.StreamKey, _ event.Seq) ([]event.Event, error) {
	<-s.replayGate
	return s.history[key], nil
}
