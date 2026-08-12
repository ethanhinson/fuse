package observe_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/observe"
)

type lifecycleStore struct {
	cancelCalls atomic.Int32
	live        chan event.Event
	subscribed  chan struct{}
}

func (s *lifecycleStore) Append(context.Context, event.StreamKey, event.Event) error { return nil }
func (s *lifecycleStore) Replay(context.Context, event.StreamKey, event.Seq) ([]event.Event, error) {
	return nil, nil
}
func (s *lifecycleStore) Subscribe(context.Context, event.StreamKey) (<-chan event.Event, func(), error) {
	var once sync.Once
	close(s.subscribed)
	return s.live, func() { once.Do(func() { s.cancelCalls.Add(1); close(s.live) }) }, nil
}

func TestRunnerNormalShutdownClosesSubscriptionExactlyOnce(t *testing.T) {
	store := &lifecycleStore{live: make(chan event.Event), subscribed: make(chan struct{})}
	runner := observe.NewRunner(store, event.StreamKey{Tenant: "tenant", Loop: "loop"}, observe.ProjectorFunc(func(context.Context, observe.Record) error { return nil }), nil)
	done := make(chan error, 1)
	go func() { done <- runner.Run(context.Background()) }()
	select {
	case <-store.subscribed:
	case <-time.After(time.Second):
		t.Fatal("runner did not subscribe")
	}
	runner.Close()
	runner.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("runner leaked after Close")
	}
	if got := store.cancelCalls.Load(); got != 1 {
		t.Fatalf("subscription cancel calls=%d, want 1", got)
	}
}

func TestFanoutOutageAttemptsEveryProjectorWithoutRetry(t *testing.T) {
	var calls [3]atomic.Int32
	fanout := observe.Fanout{
		observe.ProjectorFunc(func(context.Context, observe.Record) error { calls[0].Add(1); return errors.New("sink") }),
		observe.ProjectorFunc(func(context.Context, observe.Record) error { calls[1].Add(1); return nil }),
		observe.ProjectorFunc(func(context.Context, observe.Record) error { calls[2].Add(1); return errors.New("exporter") }),
	}
	if err := fanout.Project(context.Background(), observe.Record{}); err == nil {
		t.Fatal("fanout hid outages")
	}
	for i := range calls {
		if got := calls[i].Load(); got != 1 {
			t.Fatalf("projector %d calls=%d, want exactly one", i, got)
		}
	}
}
