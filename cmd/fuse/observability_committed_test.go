package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/observe"
)

// sentinelProjector records every projection until it sees the sentinel. The
// dispatcher's queue is FIFO with one consumer, so once the sentinel arrives
// every record enqueued before it has been projected. Waiting for it, rather
// than closing the dispatcher (which cancels before draining), is what lets a
// test see a second, duplicate projection.
type sentinelProjector struct {
	records []observe.Record
	seen    chan struct{}
}

const projectionSentinel = "test.sentinel"

func (p *sentinelProjector) Project(_ context.Context, record observe.Record) error {
	if record.EventName == projectionSentinel {
		close(p.seen)
		return nil
	}
	p.records = append(p.records, record)
	return nil
}

// projectedRecords appends through a projectingDurableStore over a real fsstore
// durable store and returns every record the projection received.
func projectedRecords(t *testing.T, appendFn func(projectingDurableStore, event.StreamKey) event.Seq) ([]observe.Record, event.Seq) {
	t.Helper()
	durable := fsstore.NewDurableFSStore(t.TempDir())
	t.Cleanup(func() { _ = durable.Close() })
	projector := &sentinelProjector{seen: make(chan struct{})}
	dispatcher := newProjectionDispatcher(projector, 8)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = dispatcher.Close(ctx)
	})
	store := projectingDurableStore{CommittedDurableStore: durable, projection: dispatcher}
	key := event.StreamKey{Tenant: "tenant-a", Loop: "loop-a"}
	// One earlier event, straight to the inner store, so the committed seq under
	// test is not the trivial first and nothing extra is projected.
	if _, err := durable.AppendCommitted(context.Background(), key, event.Event{Kind: event.KindTurnStart}); err != nil {
		t.Fatal(err)
	}
	seq := appendFn(store, key)
	dispatcher.Enqueue(observe.Record{EventName: projectionSentinel})
	select {
	case <-projector.seen:
	case <-time.After(5 * time.Second):
		t.Fatal("projection did not drain")
	}
	// The sentinel was observed after every earlier record was appended by the
	// single consumer goroutine; the channel close orders that write before this read.
	return projector.records, seq
}

// The runtime appends loop.cancelled through AppendCommitted to learn its seq.
// Without the wrapper's AppendCommitted override that append bypasses
// projection and fuse_loops_cancelled_total never moves in the shipped binary.
func TestProjectingDurableStoreProjectsAppendCommitted(t *testing.T) {
	payload, err := json.Marshal(event.LoopCancelledPayload{Reason: "stop", WasLive: true})
	if err != nil {
		t.Fatal(err)
	}
	records, seq := projectedRecords(t, func(s projectingDurableStore, key event.StreamKey) event.Seq {
		committed, err := s.AppendCommitted(context.Background(), key, event.Event{Kind: event.KindLoopCancelled, Payload: payload})
		if err != nil {
			t.Fatal(err)
		}
		return committed.Seq
	})
	if len(records) != 1 {
		t.Fatalf("AppendCommitted projected %d records, want exactly one", len(records))
	}
	if seq == 0 || records[0].Sequence != seq {
		t.Fatalf("projected seq = %d, committed seq = %d", records[0].Sequence, seq)
	}
	if records[0].EventName != string(event.KindLoopCancelled) || records[0].CancelWasLive != "true" {
		t.Fatalf("projected record = %#v", records[0])
	}
}

// Append delegates to AppendCommitted, so it must still project exactly once.
func TestProjectingDurableStoreAppendProjectsOnce(t *testing.T) {
	records, _ := projectedRecords(t, func(s projectingDurableStore, key event.StreamKey) event.Seq {
		if err := s.Append(context.Background(), key, event.Event{Kind: event.KindTurnEnd}); err != nil {
			t.Fatal(err)
		}
		return 0
	})
	if len(records) != 1 {
		t.Fatalf("Append projected %d records, want exactly one", len(records))
	}
	if records[0].EventName != string(event.KindTurnEnd) || records[0].Sequence != 2 {
		t.Fatalf("projected record = %#v", records[0])
	}
}
