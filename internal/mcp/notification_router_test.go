package mcp

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"

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
	if progressCalls != 1 || streamCalls != 0 {
		t.Fatalf("progress=%d stream=%d, want 1/0", progressCalls, streamCalls)
	}
	m.dispatchNotification("s", "$/stream", nil)
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
