package main

import (
	"encoding/json"
	"testing"
	"time"

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
	// A concrete instant; the store stamps events in UTC, the direct write uses the
	// same instant in Local (time.Now()), and the projection Local-izes back — so
	// the direct side is constructed in Local to model the shipped write faithfully.
	utcTS := time.Date(2026, 8, 10, 5, 30, 0, 0, time.UTC)

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
			// The direct write the shipped code produces today (shell.go) — Local TS.
			direct := session.LogEntry{
				TS:       utcTS.Local(),
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

			// The event the spawn boundary emits (UTC-stamped), projected back to a
			// LogEntry (which Local-izes the TS to match the shipped log).
			pl, _ := event.MarshalPayload(event.SpawnDonePayload{
				ChildNodeID: "child-9",
				ParentID:    "root-0",
				Label:       "researcher",
				Depth:       2,
				Result:      "some result text",
				Err:         tc.errStr,
			})
			ev := event.Event{
				TS:      utcTS,
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

// TestProjectionMatchesDirectProductionTimestamp guards the review finding: the
// direct session.jsonl write stamps TS with time.Now() (LOCAL) while the event
// store stamps TS in UTC. The projection must convert back to Local so the two
// production timestamps marshal byte-identical — proving the equivalence in
// production, not with a pre-normalized timezone. A wall-clock instant is stamped
// as the store would (UTC) and as the direct write would (its Local rendering);
// both must marshal to the same JSON bytes.
func TestProjectionMatchesDirectProductionTimestamp(t *testing.T) {
	now := time.Now() // the instant a real spawn.done would occur

	// Direct write path (shell.go): time.Now() as-is (Local on this host).
	direct := session.LogEntry{
		TS:       now,
		NodeID:   "c",
		ParentID: "r",
		Label:    "w",
		Depth:    1,
		Kind:     "done",
	}
	directBytes, _ := json.Marshal(direct)

	// Event path: the store stamps the SAME instant in UTC, then the projection
	// converts back to Local.
	pl, _ := event.MarshalPayload(event.SpawnDonePayload{ChildNodeID: "c", ParentID: "r", Label: "w", Depth: 1})
	ev := event.Event{TS: now.UTC(), Kind: event.KindSpawnDone, Payload: pl}
	projected, ok := projectEventToLog(ev)
	if !ok {
		t.Fatal("projection returned ok=false")
	}
	projBytes, _ := json.Marshal(projected)

	if string(projBytes) != string(directBytes) {
		t.Errorf("production TS mismatch (UTC event vs local direct write):\n proj %s\n dir  %s", projBytes, directBytes)
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

// Note (change 0044): TestEmitSpawnStartDone and its recordStore double were
// removed with the cmd-site emitters they exercised — spawn.start / spawn.done
// emission now lives in the Spawner (internal/agent/spawn.go) and is covered by
// TestSpawnerEmitsSpawnStartDone there. The projection tests above (the CONSUMER
// side) remain the cmd/fuse gate for the "log adapts" byte-equivalence.
