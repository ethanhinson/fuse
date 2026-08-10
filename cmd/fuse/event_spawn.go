package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/session"
)

// Note (change 0044): the spawn.start / spawn.done EMITTERS moved from here into
// the Spawner (internal/agent/spawn.go) — the single choke point every spawn
// passes through. This file retains only the CONSUMER side: the projection of a
// spawn.done event back into the forensic session log, proving the "log adapts"
// byte-equivalence with the direct sessLog.Write. The Spawner-emitted spawn.done
// carries the same node-identity fields this projection reads, so the projected
// log stays byte-identical.

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
