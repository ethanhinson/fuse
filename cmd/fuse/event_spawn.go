package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/session"
)

// emitSpawnStart records a spawn.start event at the cmd/fuse child-run boundary
// (change 0043). The loop itself never spawns; the spawn tool's child-run closure
// is where the child's lifecycle begins, so the two spawn lifecycle events are
// emitted here (via the process-global store) rather than inside Agent.Run. This
// is one of the three cloned child-builder sites (learning
// patch-every-cloned-child-builder) — shell.go, main.go, research_probe.go all
// call this.
func emitSpawnStart(store event.EventStore, childNode *agent.AgentNode, task string) {
	if store == nil || childNode == nil {
		return
	}
	pl, err := event.MarshalPayload(event.SpawnStartPayload{
		ChildNodeID: childNode.ID,
		Label:       childNode.Label,
		Task:        task,
	})
	if err != nil {
		return
	}
	_ = store.Append(event.Event{
		NodeID:   childNode.ID,
		ParentID: childNode.ParentID,
		Depth:    childNode.Depth,
		Kind:     event.KindSpawnStart,
		Payload:  pl,
	})
}

// emitSpawnDone records a spawn.done event carrying everything the current
// session-log projection needs to reproduce the child-completion LogEntry, plus
// the spawn result and any structured-delegation value (change 0042). Err is set
// from rerr, mirroring the direct sessLog.Write "done"/"error" Kind selection.
func emitSpawnDone(store event.EventStore, childNode *agent.AgentNode, msgs []model.Message, rerr error, sink *agent.ExpectsSink) {
	if store == nil || childNode == nil {
		return
	}
	errStr := ""
	if rerr != nil {
		errStr = rerr.Error()
	}
	var structured json.RawMessage
	if sink != nil && sink.Captured() {
		if b, merr := json.Marshal(sink.Value()); merr == nil {
			structured = b
		}
	}
	pl, err := event.MarshalPayload(event.SpawnDonePayload{
		ChildNodeID: childNode.ID,
		ParentID:    childNode.ParentID,
		Label:       childNode.Label,
		Depth:       childNode.Depth,
		Result:      lastAssistantText(msgs),
		Err:         errStr,
		Structured:  structured,
	})
	if err != nil {
		return
	}
	_ = store.Append(event.Event{
		NodeID:   childNode.ID,
		ParentID: childNode.ParentID,
		Depth:    childNode.Depth,
		Kind:     event.KindSpawnDone,
		Payload:  pl,
	})
}

// projectEventToLog re-expresses the loop event stream as the forensic session
// log (change 0043, "log adapts"): a spawn.done event projects to the exact
// current child-completion LogEntry — the single entry shape the direct
// sessLog.Write produces today (shell.go). Every other kind returns ok=false, so
// the projected log is byte-identical to the direct log. The projected TS is the
// event's TS (the store stamps it on Append), so a downstream consumer writing
// these entries reproduces the current file format.
//
// This projector is the pure, testable core of the log-as-consumer path. It is
// deliberately in cmd/fuse (not internal/session) so internal/session stays free
// of any internal/event dependency.
func projectEventToLog(e event.Event) (session.LogEntry, bool) {
	if e.Kind != event.KindSpawnDone {
		return session.LogEntry{}, false
	}
	var pl event.SpawnDonePayload
	if err := json.Unmarshal(e.Payload, &pl); err != nil {
		return session.LogEntry{}, false
	}
	kind := "done"
	if pl.Err != "" {
		kind = "error"
	}
	return session.LogEntry{
		// The shipped session.jsonl direct write uses time.Now() (LOCAL) — see
		// shell.go. The event stream stamps TS in UTC (correct for a durable,
		// replayable record), so the projection converts back to Local here so the
		// projected log is BYTE-IDENTICAL to the current shipped log. This keeps the
		// "log adapts" byte-equivalence true in production (not merely in a test that
		// pre-normalizes the timezone), which is the gate for the trivial follow-up
		// that deletes the direct write. (Review finding, change 0043.)
		TS:       e.TS.Local(),
		NodeID:   pl.ChildNodeID,
		ParentID: pl.ParentID,
		Label:    pl.Label,
		Depth:    pl.Depth,
		Kind:     kind,
	}, true
}

// startProjectedLogConsumer subscribes to the event stream and writes the
// projected session log (change 0043). It runs transiently alongside the direct
// sessLog.Write path so the two can be proven byte-identical before the direct
// writes are removed (a trivial follow-up). The projection is written to
// session.projected.jsonl in the per-session dir, NOT the shipped session.jsonl,
// so the current file's readers are entirely unaffected during this change.
//
// The returned stop function unsubscribes and drains, then closes the file. It is
// idempotent and safe to defer.
func startProjectedLogConsumer(store event.EventStore, baseDir, sessionID string) func() {
	dir := session.SessionDir(baseDir, sessionID)
	path := filepath.Join(dir, "session.projected.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		// Best-effort: a projection-log failure never disturbs the run.
		_, cancel := store.Subscribe()
		return cancel
	}
	ch, cancel := store.Subscribe()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for e := range ch {
			entry, ok := projectEventToLog(e)
			if !ok {
				continue
			}
			b, merr := json.Marshal(entry)
			if merr != nil {
				continue
			}
			_, _ = f.Write(b)
			_, _ = f.Write([]byte("\n"))
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()  // unsubscribe → closes ch → the goroutine's range ends
			wg.Wait() // drain remaining projected writes
			_ = f.Close()
		})
	}
}
