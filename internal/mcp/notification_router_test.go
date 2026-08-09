package mcp

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/tools"
)

// TestManagerDispatchNotificationDeliversToHandler registers a handler for
// $/progress and asserts dispatch delivers (server, params) to it.
func TestManagerDispatchNotificationDeliversToHandler(t *testing.T) {
	m, err := NewManager(nil, tools.NewRegistry())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer m.Close()

	var (
		gotServer string
		gotParams json.RawMessage
		called    int
	)
	m.OnNotification("$/progress", func(server string, params json.RawMessage) {
		gotServer = server
		gotParams = params
		called++
	})

	m.dispatchNotification("srv1", "$/progress", json.RawMessage(`{"progress":0.5}`))

	m.waitNotifyDrained() // dispatch is async; the barrier also gives happens-before
	if called != 1 {
		t.Fatalf("handler called %d times, want 1", called)
	}
	if gotServer != "srv1" {
		t.Errorf("server = %q, want srv1", gotServer)
	}
	if string(gotParams) != `{"progress":0.5}` {
		t.Errorf("params = %s, want {\"progress\":0.5}", gotParams)
	}
}

// TestManagerNotificationHandlersAreIsolated verifies a handler for one method
// does not fire for another.
func TestManagerNotificationHandlersAreIsolated(t *testing.T) {
	m, _ := NewManager(nil, tools.NewRegistry())
	defer m.Close()

	var progressCalls, streamCalls int
	m.OnNotification("$/progress", func(string, json.RawMessage) { progressCalls++ })
	m.OnNotification("$/stream", func(string, json.RawMessage) { streamCalls++ })

	m.dispatchNotification("s", "$/progress", nil)
	m.waitNotifyDrained()
	if progressCalls != 1 || streamCalls != 0 {
		t.Fatalf("progress=%d stream=%d, want 1/0", progressCalls, streamCalls)
	}
	m.dispatchNotification("s", "$/stream", nil)
	m.waitNotifyDrained()
	if progressCalls != 1 || streamCalls != 1 {
		t.Fatalf("progress=%d stream=%d, want 1/1", progressCalls, streamCalls)
	}
}

// TestManagerDispatchUnregisteredIsNoop verifies dispatching an unregistered
// method is a silent no-op (no panic).
func TestManagerDispatchUnregisteredIsNoop(t *testing.T) {
	m, _ := NewManager(nil, tools.NewRegistry())
	defer m.Close()
	// No handlers registered.
	m.dispatchNotification("s", "$/unknown", json.RawMessage(`{}`))
	// Reaching here without panic is success.
}

// TestManagerDispatchRaceClean exercises concurrent register + dispatch to catch
// data races under -race.
func TestManagerDispatchRaceClean(t *testing.T) {
	m, _ := NewManager(nil, tools.NewRegistry())
	defer m.Close()

	var count atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			m.OnNotification("$/progress", func(string, json.RawMessage) { count.Add(1) })
		}()
		go func() {
			defer wg.Done()
			m.dispatchNotification("s", "$/progress", nil)
		}()
	}
	wg.Wait()
}

// TestSlowHandlerDoesNotBlockDispatch is the regression test for #18: a slow
// notification handler must NOT block dispatchNotification (the read-pump call
// site). Before the fix, handlers ran inline on the read-pump, so a blocking
// handler stalled all response dispatch on that connection. Now dispatch only
// does a non-blocking enqueue, so it returns immediately regardless of handler
// speed.
func TestSlowHandlerDoesNotBlockDispatch(t *testing.T) {
	m, _ := NewManager(nil, tools.NewRegistry())
	defer m.Close()

	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	m.OnNotification("$/slow", func(string, json.RawMessage) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release // block the worker until the test releases it
	})

	// First dispatch occupies the worker in the blocking handler.
	m.dispatchNotification("s", "$/slow", nil)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never ran")
	}

	// While the worker is blocked, further dispatches must return immediately
	// (non-blocking enqueue), never wedging the caller (the read-pump).
	done := make(chan struct{})
	go func() {
		for i := 0; i < notifyQueueSize+50; i++ {
			m.dispatchNotification("s", "$/slow", nil) // extras beyond cap are dropped+logged
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatchNotification blocked while a handler was slow — the read-pump would stall")
	}

	close(release) // let the worker drain so Close() is clean
}

// TestNotificationOrderingPreserved confirms the single worker preserves
// enqueue (FIFO) order across notifications.
func TestNotificationOrderingPreserved(t *testing.T) {
	m, _ := NewManager(nil, tools.NewRegistry())
	defer m.Close()

	var mu sync.Mutex
	var order []string
	m.OnNotification("$/seq", func(_ string, params json.RawMessage) {
		mu.Lock()
		order = append(order, string(params))
		mu.Unlock()
	})

	for i := 0; i < 20; i++ {
		m.dispatchNotification("s", "$/seq", json.RawMessage([]byte{byte('a' + i)}))
	}
	m.waitNotifyDrained()

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 20 {
		t.Fatalf("delivered %d, want 20", len(order))
	}
	for i, v := range order {
		if want := string([]byte{byte('a' + i)}); v != want {
			t.Fatalf("order[%d] = %q, want %q (FIFO broken)", i, v, want)
		}
	}
}
