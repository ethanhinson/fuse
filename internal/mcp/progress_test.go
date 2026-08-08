package mcp

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/tools"
)

// collectProgress subscribes an observer that records ProgressEvents.
type progressCollector struct {
	mu     sync.Mutex
	events []ProgressEvent
}

func (p *progressCollector) observe(e ProgressEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, e)
}

func (p *progressCollector) snapshot() []ProgressEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]ProgressEvent, len(p.events))
	copy(out, p.events)
	return out
}

// TestProgressNotificationFansToObserver: a $/progress with a matched token
// delivers a ProgressEvent carrying the tracking's server/tool and the progress
// (and total, when present) to the registered observer.
func TestProgressNotificationFansToObserver(t *testing.T) {
	m, _ := NewManager(nil, tools.NewRegistry())
	defer m.Close()

	pc := &progressCollector{}
	m.OnProgress(pc.observe)

	// Begin a call to register a token.
	token, end := m.beginCall("srv", "longtool")
	defer end()

	// Determinate progress (total present).
	total := 10.0
	params, _ := json.Marshal(map[string]any{
		"progressToken": token, "progress": 3.0, "total": total,
	})
	m.dispatchNotification("srv", "$/progress", params)

	// Indeterminate progress (no total).
	params2, _ := json.Marshal(map[string]any{
		"progressToken": token, "progress": 5.0,
	})
	m.dispatchNotification("srv", "$/progress", params2)

	evts := pc.snapshot()
	if len(evts) != 2 {
		t.Fatalf("observer saw %d events, want 2", len(evts))
	}
	e0 := evts[0]
	if e0.Server != "srv" || e0.Tool != "longtool" {
		t.Errorf("event0 server/tool = %q/%q, want srv/longtool", e0.Server, e0.Tool)
	}
	if e0.Progress != 3.0 {
		t.Errorf("event0 progress = %v, want 3", e0.Progress)
	}
	if e0.Total == nil || *e0.Total != 10.0 {
		t.Errorf("event0 total = %v, want 10 (determinate)", e0.Total)
	}
	if evts[1].Total != nil {
		t.Errorf("event1 total = %v, want nil (indeterminate)", evts[1].Total)
	}
}

// TestProgressUnmatchedTokenDropped: a $/progress for a token that was never
// registered (or was already cleared) is silently dropped — no observer event,
// no panic.
func TestProgressUnmatchedTokenDropped(t *testing.T) {
	m, _ := NewManager(nil, tools.NewRegistry())
	defer m.Close()

	pc := &progressCollector{}
	m.OnProgress(pc.observe)

	params, _ := json.Marshal(map[string]any{"progressToken": "ghost", "progress": 1.0})
	m.dispatchNotification("srv", "$/progress", params)

	if got := len(pc.snapshot()); got != 0 {
		t.Errorf("unmatched token produced %d events, want 0", got)
	}
}

// TestProgressAfterEndDropped: a late $/progress arriving after the call ended
// (token cleared) must not panic or fire.
func TestProgressAfterEndDropped(t *testing.T) {
	m, _ := NewManager(nil, tools.NewRegistry())
	defer m.Close()

	pc := &progressCollector{}
	m.OnProgress(pc.observe)

	token, end := m.beginCall("srv", "t")
	end() // clear tracking

	params, _ := json.Marshal(map[string]any{"progressToken": token, "progress": 1.0})
	m.dispatchNotification("srv", "$/progress", params)

	if got := len(pc.snapshot()); got != 0 {
		t.Errorf("late token produced %d events, want 0", got)
	}
}
