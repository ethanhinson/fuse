package fuse

import (
	"context"
	"errors"
	"testing"

	"github.com/ethanhinson/fuse/internal/event"
)

// stubBackend is a test double implementing the unexported backend interface so the
// public Client surface can be exercised without a runtime or a network. It records
// the arguments each method received and returns scripted results.
type stubBackend struct {
	startCfg   StartLoopConfig
	startID    LoopID
	startErr   error
	sawSendID  LoopID
	sawSendIn  string
	sendErr    error
	sawObsID   LoopID
	sawObsFrom Seq
	obsCh      <-chan Event
	obsUnsub   func()
	obsErr     error
}

func (s *stubBackend) startLoop(ctx context.Context, cfg StartLoopConfig) (LoopID, error) {
	s.startCfg = cfg
	return s.startID, s.startErr
}

func (s *stubBackend) send(ctx context.Context, id LoopID, input string) error {
	s.sawSendID, s.sawSendIn = id, input
	return s.sendErr
}

func (s *stubBackend) observe(ctx context.Context, id LoopID, from Seq) (<-chan Event, func(), error) {
	s.sawObsID, s.sawObsFrom = id, from
	return s.obsCh, s.obsUnsub, s.obsErr
}

// newTestClient builds a Client over a stub backend (mirrors how NewLocal/NewRemote
// wrap a backend).
func newTestClient(b backend) *Client { return &Client{backend: b} }

func TestClientDelegatesToBackend(t *testing.T) {
	ch := make(chan Event, 1)
	unsub := func() {}
	b := &stubBackend{
		startID:  "loop-7",
		obsCh:    ch,
		obsUnsub: unsub,
	}
	c := newTestClient(b)
	ctx := context.Background()

	id, err := c.StartLoop(ctx, StartLoopConfig{Task: "do it", Model: "cloud/x", Interactive: true})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	if id != "loop-7" {
		t.Fatalf("StartLoop id = %q, want loop-7", id)
	}
	if b.startCfg.Task != "do it" || b.startCfg.Model != "cloud/x" || !b.startCfg.Interactive {
		t.Fatalf("StartLoop cfg not forwarded: %+v", b.startCfg)
	}

	if err := c.Send(ctx, "loop-7", "more"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if b.sawSendID != "loop-7" || b.sawSendIn != "more" {
		t.Fatalf("Send args not forwarded: id=%q in=%q", b.sawSendID, b.sawSendIn)
	}

	gotCh, gotUnsub, err := c.Observe(ctx, "loop-7", 5)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if b.sawObsID != "loop-7" || b.sawObsFrom != Seq(5) {
		t.Fatalf("Observe args not forwarded: id=%q from=%d", b.sawObsID, b.sawObsFrom)
	}
	if gotCh == nil || gotUnsub == nil {
		t.Fatal("Observe returned nil channel or unsubscribe")
	}
}

func TestClientPropagatesErrors(t *testing.T) {
	b := &stubBackend{startErr: ErrUnauthenticated, sendErr: ErrLoopNotFound, obsErr: ErrPermissionDenied}
	c := newTestClient(b)
	ctx := context.Background()

	if _, err := c.StartLoop(ctx, StartLoopConfig{}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("StartLoop err = %v, want ErrUnauthenticated", err)
	}
	if err := c.Send(ctx, "x", "y"); !errors.Is(err, ErrLoopNotFound) {
		t.Fatalf("Send err = %v, want ErrLoopNotFound", err)
	}
	if _, _, err := c.Observe(ctx, "x", 0); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("Observe err = %v, want ErrPermissionDenied", err)
	}
}

// TestIsCompletionFiresOnParked pins the completion predicate to the real kind.
func TestIsCompletionFiresOnParked(t *testing.T) {
	parked := Event{Kind: event.KindLoopParked}
	if !IsCompletion(parked) {
		t.Fatal("IsCompletion(loop.parked) = false, want true")
	}
	for _, k := range []Kind{event.KindTurnStart, event.KindTurnEnd, event.KindModelCallEnd} {
		if IsCompletion(Event{Kind: k}) {
			t.Fatalf("IsCompletion(%s) = true, want false", k)
		}
	}
}

// TestPublicTypeAliases is a compile-time proof that callers get the event types
// without importing internal/event on the surface (they are aliases, so assignment
// in both directions is legal).
func TestPublicTypeAliases(t *testing.T) {
	var e Event = event.Event{Seq: 3, Kind: event.KindTurnStart}
	var s Seq = e.Seq
	var k Kind = e.Kind
	var raw event.Event = e // alias round-trips to the internal type
	if raw.Seq != 3 || s != 3 || k != event.KindTurnStart {
		t.Fatalf("type alias round-trip lost data: %+v", raw)
	}
	var _ LoopID = "loop-1"
}
