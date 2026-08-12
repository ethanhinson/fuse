package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/observe"
)

type recordingWriter struct {
	mu     sync.Mutex
	writes [][]byte
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, bytes.Clone(p))
	return len(p), nil
}
func (w *recordingWriter) Close() error { return nil }

func TestLoggerJSONLConcurrentOneWriteAndAuditBypass(t *testing.T) {
	w := &recordingWriter{}
	levels, _ := NewLevels(LevelError, time.Hour, time.Now)
	defer levels.Close()
	l := New(w, levels, Identity{Service: "fuse", Instance: "i-1"})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = l.Log(context.Background(), Entry{Timestamp: time.Unix(1, 0).UTC(), Level: LevelError, Name: "loop.error", TenantID: "t", LoopID: "l", NodeID: "n", Sequence: 2, Operation: "loop", Outcome: "error", ErrorCategory: "loop_error"})
		}()
	}
	wg.Wait()
	_ = levels.SetGlobal(LevelError)
	if err := l.Audit(context.Background(), Audit{Actor: "admin", Action: "set", Scope: "global", Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Log(context.Background(), Entry{Level: LevelInfo, Name: "suppressed"}); err != nil {
		t.Fatal(err)
	}
	if len(w.writes) != 51 {
		t.Fatalf("writes=%d", len(w.writes))
	}
	for _, line := range w.writes {
		if line[len(line)-1] != '\n' || bytes.Count(line, []byte{'\n'}) != 1 {
			t.Fatalf("not one JSONL record: %q", line)
		}
		var v map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(line), &v); err != nil {
			t.Fatal(err)
		}
		if v["service"] != "fuse" || v["instance_id"] != "i-1" {
			t.Fatalf("identity missing: %v", v)
		}
		if _, nested := v["Data"]; nested || v["event_name"] == nil {
			t.Fatalf("schema is not flat: %v", v)
		}
		if strings.Contains(string(line), "payload") {
			t.Fatalf("payload field present: %s", line)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLoggerProjectsObserveRecord(t *testing.T) {
	w := &recordingWriter{}
	levels, _ := NewLevels(LevelDebug, time.Hour, time.Now)
	defer levels.Close()
	l := New(w, levels, Identity{})
	err := l.Project(context.Background(), observe.Record{Timestamp: time.Unix(1, 0), EventName: "turn.start", TenantID: event.TenantID("t"), LoopID: event.LoopID("l"), NodeID: "n", Sequence: 3, Operation: observe.OperationLoop, Outcome: observe.OutcomeSuccess, ErrorCategory: observe.ErrorCategoryNone})
	if err != nil {
		t.Fatal(err)
	}
	if len(w.writes) != 1 {
		t.Fatalf("writes=%d", len(w.writes))
	}
}

func TestConcurrentReadMutateExpireReloadReopenShutdown(t *testing.T) {
	path := t.TempDir() + "/fuse.log"
	file, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	levels, err := NewLevels(LevelInfo, time.Minute, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	logger := New(file, levels, Identity{Service: "fuse", Instance: "race"})
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 150; j++ {
				_ = logger.Log(context.Background(), Entry{Level: LevelInfo, Name: "turn.start", TenantID: "t", LoopID: "l"})
				_ = levels.Effective("t", "l")
				if i%3 == 0 {
					_ = levels.SetLoop("t", "l", LevelDebug, time.Now().Add(time.Second))
				}
				if i%3 == 1 {
					levels.RemoveLoop("t", "l")
					_ = levels.SetDefault(LevelDebug)
				}
				if i%3 == 2 {
					levels.Expire()
					_ = logger.Reopen()
				}
			}
		}(i)
	}
	// Exercise shutdown against admitted logging and reopen operations. Errors
	// after the close boundary are expected; the race/lifecycle is the assertion.
	wg.Add(1)
	go func() { defer wg.Done(); _ = logger.Close(); _ = logger.Close() }()
	wg.Wait()
	if err := levels.Close(); err != nil {
		t.Fatal(err)
	}
	if err := levels.Close(); err != nil {
		t.Fatal(err)
	}
}
