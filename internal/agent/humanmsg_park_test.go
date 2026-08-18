package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// counters records park/wake callback invocations.
type parkCounters struct{ parks, wakes atomic.Int64 }

func (c *parkCounters) bind(h *HumanInjector) {
	h.SetTurnBoundary(func() { c.parks.Add(1) }, func() { c.wakes.Add(1) })
}

func TestHumanInjectorTurnBoundaryParkAndWake(t *testing.T) {
	bus := NewHumanBus(nil)
	inj := NewHumanInjector("n1", bus)
	var c parkCounters
	c.bind(inj)

	done := make(chan error, 1)
	go func() { done <- inj.Wait(context.Background()) }()

	// Wait until the callback observes the park, then deliver a message.
	deadline := time.After(2 * time.Second)
	for c.parks.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("onPark never fired")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if got := c.wakes.Load(); got != 0 {
		t.Fatalf("onWake fired before a message arrived: %d", got)
	}
	bus.Enqueue("n1", ModeDirect, "human", "hello")

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Wait returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait never returned")
	}
	if got := c.parks.Load(); got != 1 {
		t.Fatalf("onPark fired %d times, want 1", got)
	}
	if got := c.wakes.Load(); got != 1 {
		t.Fatalf("onWake fired %d times, want 1", got)
	}
}

func TestHumanInjectorTurnBoundaryCancelParksWithoutWake(t *testing.T) {
	bus := NewHumanBus(nil)
	inj := NewHumanInjector("n1", bus)
	var c parkCounters
	c.bind(inj)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := inj.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait returned %v, want context.Canceled", err)
	}
	if got := c.parks.Load(); got != 1 {
		t.Fatalf("onPark fired %d times, want 1", got)
	}
	if got := c.wakes.Load(); got != 0 {
		t.Fatalf("onWake fired %d times, want 0", got)
	}
}

func TestHumanInjectorTurnBoundaryNoBusNeverParks(t *testing.T) {
	inj := NewHumanInjector("n1", nil)
	var c parkCounters
	c.bind(inj)

	if err := inj.Wait(context.Background()); !errors.Is(err, errNoBus) {
		t.Fatalf("Wait returned %v, want errNoBus", err)
	}
	if got := c.parks.Load(); got != 0 {
		t.Fatalf("onPark fired %d times, want 0", got)
	}
	if got := c.wakes.Load(); got != 0 {
		t.Fatalf("onWake fired %d times, want 0", got)
	}
}

// Regression guard: every existing binding sets no callbacks at all.
func TestHumanInjectorNilCallbacksUnchanged(t *testing.T) {
	bus := NewHumanBus(nil)
	inj := NewHumanInjector("n1", bus)

	bus.Enqueue("n1", ModeDirect, "human", "hi")
	if err := inj.Wait(context.Background()); err != nil {
		t.Fatalf("Wait returned %v, want nil", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	drained := bus.Drain("n1")
	if len(drained) != 1 {
		t.Fatalf("drained %d messages, want 1", len(drained))
	}
	if err := inj.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait returned %v, want context.Canceled", err)
	}

	var nilInj *HumanInjector
	if err := nilInj.Wait(context.Background()); !errors.Is(err, errNoBus) {
		t.Fatalf("nil receiver Wait returned %v, want errNoBus", err)
	}
	// A nil receiver must also tolerate SetTurnBoundary-free use of Poll.
	if _, ok := nilInj.Poll(); ok {
		t.Fatal("nil receiver Poll reported a message")
	}
}
