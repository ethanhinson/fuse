package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
)

// stallingStore is a durable store whose first Replay of one stream, once armed,
// blocks until released: a slow store round trip for that stream alone.
type stallingStore struct {
	*fsstore.FSDurableStore

	mu      sync.Mutex
	stall   event.StreamKey
	armed   bool
	entered chan struct{}
	release chan struct{}
}

func (s *stallingStore) arm(key event.StreamKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stall, s.armed = key, true
	s.entered, s.release = make(chan struct{}), make(chan struct{})
}

func (s *stallingStore) Replay(ctx context.Context, key event.StreamKey, from event.Seq) ([]event.Event, error) {
	s.mu.Lock()
	hit := s.armed && key == s.stall
	if hit {
		s.armed = false
	}
	entered, release := s.entered, s.release
	s.mu.Unlock()
	if hit {
		close(entered)
		<-release
	}
	return s.FSDurableStore.Replay(ctx, key, from)
}

// A cancel serializes only with cancels of the same stream: while one stream's cold
// seal is stalled on a slow store read, a cold cancel of another stream in the same
// process completes.
func TestCancelOtherStreamNotBlockedByStalledSeal(t *testing.T) {
	f := &steerFake{}
	var stalling *stallingStore
	rtA, rtB, store := twoReplicasWith(t, f, nil, func(d *Deps) {
		if stalling == nil {
			stalling = &stallingStore{FSDurableStore: d.DurableStore.(*fsstore.FSDurableStore)}
		}
		d.DurableStore = stalling
	})
	ctx := context.Background()
	var ids [2]string
	for i := range ids {
		h, err := rtA.StartLoop(ctx, LoopConfig{Task: "answer", ModelID: "target"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.Wait(); err != nil {
			t.Fatal(err)
		}
		ids[i] = h.ID()
	}
	c := rtB.(Canceller)

	stalling.arm(event.StreamKey{Tenant: event.DefaultTenant, Loop: event.LoopID(ids[0])})
	first := cancelAsync(c, ids[0], "stop", "alice")
	select {
	case <-stalling.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel of the first stream never read it")
	}

	second := cancelAsync(c, ids[1], "stop", "alice")
	select {
	case res := <-second:
		if res.Outcome != CancelOutcomeCancelled {
			t.Fatalf("second cancel = %+v, want Cancelled", res)
		}
	case <-time.After(5 * time.Second):
		close(stalling.release)
		<-first
		t.Fatal("cancel of another stream waited on the stalled stream's seal")
	}

	close(stalling.release)
	if res := <-first; res.Outcome != CancelOutcomeCancelled {
		t.Fatalf("first cancel = %+v, want Cancelled", res)
	}
	for _, id := range ids {
		if n := countKind(streamOf(t, store, event.DefaultTenant, id), event.KindLoopCancelled); n != 1 {
			t.Fatalf("loop %s: want one loop.cancelled, got %d", id, n)
		}
	}
}

// held reports how many stream locks are held or waited for.
func (l *streamLocks) held() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.m)
}

// One stream's lock excludes its other callers, not another stream's, and every
// entry is dropped once its last holder unlocks.
func TestStreamLocksPerKeyExclusionAndCleanup(t *testing.T) {
	var l streamLocks
	a := event.StreamKey{Tenant: event.DefaultTenant, Loop: "a"}
	b := event.StreamKey{Tenant: "globex", Loop: "a"}

	unlockA := l.lock(a)
	gotB := make(chan func(), 1)
	go func() { gotB <- l.lock(b) }()
	select {
	case unlockB := <-gotB:
		unlockB()
	case <-time.After(5 * time.Second):
		t.Fatal("lock of another stream waited on a held one")
	}

	gotA := make(chan func(), 1)
	go func() { gotA <- l.lock(a) }()
	select {
	case <-gotA:
		t.Fatal("second lock of the same stream did not wait")
	case <-time.After(50 * time.Millisecond):
	}
	unlockA()
	(<-gotA)()

	const n = 16
	var wg sync.WaitGroup
	inside := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer l.lock(a)()
			inside++ // the race detector flags this if the lock does not exclude
		}()
	}
	wg.Wait()
	if inside != n {
		t.Fatalf("inside = %d, want %d", inside, n)
	}
	if h := l.held(); h != 0 {
		t.Fatalf("%d stream locks left after every unlock", h)
	}
}
