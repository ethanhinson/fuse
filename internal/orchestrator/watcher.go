package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/ethanhinson/fuse/internal/event"
)

const (
	// ringSize bounds how many events the watcher keeps. The target's own store holds
	// the full history; the orchestrator only needs the recent past.
	ringSize = 400
	// payloadCap bounds one kept payload so a huge tool result cannot bloat the ring.
	payloadCap = 16 << 10
	// defaultLimit is how many recent events loop_status shows.
	defaultLimit = 25
	// oneLineCap bounds a quoted fragment in a one-line event summary.
	oneLineCap = 160
	// outputCap bounds a whole tool result handed back to the orchestrator model.
	outputCap = 6000
)

// watcher follows a Target incrementally: each refresh fetches only events after the
// last one seen and keeps a bounded ring of the most recent.
type watcher struct {
	t    Target
	mu   sync.Mutex
	last event.Seq
	ring []entry
}

// entry is one kept event plus its one-line summary, computed on arrival so a payload
// too large to keep still shows up in loop_status.
type entry struct {
	ev   event.Event
	line string
}

func newWatcher(t Target) *watcher { return &watcher{t: t} }

func (w *watcher) refresh(ctx context.Context) error {
	w.mu.Lock()
	after := w.last
	w.mu.Unlock()
	evs, err := w.t.Events(ctx, after)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, e := range evs {
		if e.Seq <= w.last {
			continue
		}
		w.last = e.Seq
		line := ""
		if significant(e.Kind) {
			line = oneLine(e)
		}
		if len(e.Payload) > payloadCap {
			e.Payload = nil // too big to keep in full; the target store still has it
		}
		w.ring = append(w.ring, entry{ev: e, line: line})
	}
	if over := len(w.ring) - ringSize; over > 0 {
		w.ring = append([]entry(nil), w.ring[over:]...)
	}
	return nil
}

// significant reports whether a kind is worth showing the orchestrator. Turn
// boundaries, token deltas and sandbox housekeeping are noise at this level.
func significant(k event.Kind) bool {
	switch k {
	case event.KindUserInput, event.KindModelCallEnd, event.KindToolCall, event.KindToolResult,
		event.KindSpawnStart, event.KindSpawnDone, event.KindError, event.KindLoopParked,
		event.KindLoopTrip:
		return true
	}
	return false
}

// state derives what the target is doing right now from the ring.
func state(ring []entry) string {
	pending := map[string]string{} // tool call id -> name
	var order []string
	lastKind := event.Kind("")
	for _, en := range ring {
		e := en.ev
		switch e.Kind {
		case event.KindToolCall:
			var p event.ToolCallPayload
			_ = json.Unmarshal(e.Payload, &p)
			pending[p.ID] = p.Name
			order = append(order, p.ID)
		case event.KindToolResult:
			var p event.ToolResultPayload
			_ = json.Unmarshal(e.Payload, &p)
			delete(pending, p.ID)
		}
		if e.Kind != event.KindTurnStart && e.Kind != event.KindTurnEnd && e.Kind != event.KindModelDelta {
			lastKind = e.Kind
		}
	}
	for i := len(order) - 1; i >= 0; i-- {
		if name, ok := pending[order[i]]; ok {
			return "running tool " + name
		}
	}
	switch lastKind {
	case "":
		return "no activity yet"
	case event.KindLoopParked:
		return "idle - finished its last answer and is waiting for input"
	case event.KindError:
		return "stopped on an error"
	case event.KindModelCallStart:
		return "waiting on the model"
	}
	return "between steps (thinking or about to act)"
}

// oneLine renders one event as a single compact line.
func oneLine(e event.Event) string {
	who := ""
	if e.Depth > 0 {
		who = fmt.Sprintf("[child d%d] ", e.Depth)
	}
	head := fmt.Sprintf("#%d %s%s", e.Seq, who, e.Kind)
	switch e.Kind {
	case event.KindUserInput:
		var p event.UserInputPayload
		_ = json.Unmarshal(e.Payload, &p)
		return head + ": " + clip(p.Content)
	case event.KindModelCallEnd:
		var p event.ModelCallEndPayload
		_ = json.Unmarshal(e.Payload, &p)
		var names []string
		for _, c := range p.ToolCalls {
			names = append(names, c.Name)
		}
		s := head + ": " + clip(p.Content)
		if len(names) > 0 {
			s += " -> calls " + strings.Join(names, ", ")
		}
		return s
	case event.KindToolCall:
		var p event.ToolCallPayload
		_ = json.Unmarshal(e.Payload, &p)
		return head + ": " + p.Name + " " + clip(string(p.Args))
	case event.KindToolResult:
		var p event.ToolResultPayload
		_ = json.Unmarshal(e.Payload, &p)
		status := "ok"
		if p.IsError {
			status = "error"
		}
		return head + ": " + p.Name + " " + status + " " + clip(p.Result)
	case event.KindSpawnStart:
		var p event.SpawnStartPayload
		_ = json.Unmarshal(e.Payload, &p)
		return head + ": " + p.Label + " - " + clip(p.Task)
	case event.KindSpawnDone:
		var p event.SpawnDonePayload
		_ = json.Unmarshal(e.Payload, &p)
		if p.Err != "" {
			return head + ": " + p.Label + " failed: " + clip(p.Err)
		}
		return head + ": " + p.Label + " done: " + clip(p.Result)
	case event.KindError:
		var p event.ErrorPayload
		_ = json.Unmarshal(e.Payload, &p)
		return head + ": " + clip(p.Err)
	case event.KindLoopParked:
		var p event.LoopParkedPayload
		_ = json.Unmarshal(e.Payload, &p)
		return head + ": " + clip(p.Content)
	}
	return head
}

// clip flattens whitespace and bounds a fragment.
func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > oneLineCap {
		return s[:oneLineCap] + "..."
	}
	return s
}

func capOutput(s string) string {
	if len(s) > outputCap {
		return s[:outputCap] + "\n...(truncated)"
	}
	return s
}
