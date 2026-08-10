package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/session"
)

// TestProjectEventToLogEquivalence is the additive-ness gate for the log-as-
// consumer path (change 0043): a spawn.done event projects to a LogEntry that
// marshals BYTE-IDENTICAL to what the direct sessLog.Write path produces for the
// same child node. If these ever diverge, the projected session.jsonl would stop
// matching the shipped one — so the projection must reproduce the direct write
// exactly.
func TestProjectEventToLogEquivalence(t *testing.T) {
	ts := time.Date(2026, 8, 10, 5, 30, 0, 0, time.UTC)

	cases := []struct {
		name     string
		errStr   string
		wantKind string
	}{
		{"success", "", "done"},
		{"failure", "boom", "error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The direct write the shipped code produces today (shell.go).
			direct := session.LogEntry{
				TS:       ts,
				NodeID:   "child-9",
				ParentID: "root-0",
				Label:    "researcher",
				Depth:    2,
				Kind:     tc.wantKind,
			}
			directBytes, err := json.Marshal(direct)
			if err != nil {
				t.Fatalf("direct marshal: %v", err)
			}

			// The event the spawn boundary emits, projected back to a LogEntry.
			pl, _ := event.MarshalPayload(event.SpawnDonePayload{
				ChildNodeID: "child-9",
				ParentID:    "root-0",
				Label:       "researcher",
				Depth:       2,
				Result:      "some result text",
				Err:         tc.errStr,
			})
			ev := event.Event{
				TS:      ts,
				Kind:    event.KindSpawnDone,
				Payload: pl,
			}
			projected, ok := projectEventToLog(ev)
			if !ok {
				t.Fatal("projectEventToLog returned ok=false for a spawn.done event")
			}
			projBytes, err := json.Marshal(projected)
			if err != nil {
				t.Fatalf("projected marshal: %v", err)
			}
			if string(projBytes) != string(directBytes) {
				t.Errorf("projection != direct log:\n proj %s\n dir  %s", projBytes, directBytes)
			}
		})
	}
}

// TestProjectEventToLogIgnoresNonSpawnKinds: every kind except spawn.done yields
// ok=false, so the projected log records exactly what the current log records
// (child completions only) and nothing more.
func TestProjectEventToLogIgnoresNonSpawnKinds(t *testing.T) {
	for _, k := range []event.Kind{
		event.KindTurnStart, event.KindModelCallEnd, event.KindToolResult,
		event.KindSpawnStart, event.KindSummarize, event.KindError,
	} {
		if _, ok := projectEventToLog(event.Event{Kind: k}); ok {
			t.Errorf("kind %q unexpectedly projected to a log entry", k)
		}
	}
}

// TestEmitSpawnStartDone: the spawn boundary helpers append well-formed
// spawn.start / spawn.done events to the store.
func TestEmitSpawnStartDone(t *testing.T) {
	rec := &recordStore{}
	node := &agent.AgentNode{ID: "c1", ParentID: "root", Label: "worker", Depth: 1}
	emitSpawnStart(rec, node, "do the thing")
	emitSpawnDone(rec, node, nil, nil, nil)

	if len(rec.events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(rec.events))
	}
	if rec.events[0].Kind != event.KindSpawnStart || rec.events[1].Kind != event.KindSpawnDone {
		t.Fatalf("kinds = %q,%q", rec.events[0].Kind, rec.events[1].Kind)
	}
	var sp event.SpawnStartPayload
	if err := json.Unmarshal(rec.events[0].Payload, &sp); err != nil {
		t.Fatalf("spawn.start payload: %v", err)
	}
	if sp.ChildNodeID != "c1" || sp.Label != "worker" || sp.Task != "do the thing" {
		t.Errorf("spawn.start payload = %+v", sp)
	}
}

// recordStore is a minimal in-memory EventStore for cmd/fuse tests.
type recordStore struct {
	events []event.Event
}

func (r *recordStore) Append(e event.Event) error {
	r.events = append(r.events, e)
	return nil
}
func (r *recordStore) Subscribe() (<-chan event.Event, func()) {
	ch := make(chan event.Event)
	close(ch)
	return ch, func() {}
}
func (r *recordStore) Replay(from event.Seq) ([]event.Event, error) { return r.events, nil }
