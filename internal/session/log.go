// Package session provides JSONL session logging and replay for agent trees.
package session

import (
	"bufio"
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LogEntry is one line in the JSONL session log.
type LogEntry struct {
	TS       time.Time `json:"ts"`
	NodeID   string    `json:"node_id"`
	ParentID string    `json:"parent_id,omitempty"`
	Label    string    `json:"label,omitempty"`
	Depth    int       `json:"depth,omitempty"`
	Kind     string    `json:"kind"`
}

// Logger writes JSONL session log entries to a file.
type Logger struct {
	f *os.File
	w *bufio.Writer
	// firstErr latches the first write/flush failure so a hot, per-child write
	// path need not log on every call (which could spam under a full disk).
	// Callers surface it once via Err()/Close() instead. bufio.Writer already
	// latches its own error, but this also captures the marshal path and gives
	// the caller a single place to check.
	firstErr error
}

// DefaultLogDir returns ~/.fuse/sessions.
func DefaultLogDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".fuse", "sessions")
}

// NewLogger creates a new JSONL session log file in dir.
func NewLogger(dir string) (*Logger, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("session log dir: %w", err)
	}
	var buf [3]byte
	if _, err := crand.Read(buf[:]); err != nil {
		// Extremely unlikely; fall back to a time-based suffix so the filename
		// stays unique-ish rather than failing session logging entirely.
		return nil, fmt.Errorf("session log rand: %w", err)
	}
	name := fmt.Sprintf("%s-%x.jsonl", time.Now().Format("2006-01-02"), buf)
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("session log create: %w", err)
	}
	return &Logger{f: f, w: bufio.NewWriter(f)}, nil
}

// SessionDir returns the per-session directory <baseDir>/<sessionID> (change
// 0030). It holds session.jsonl and the segments/ subtree.
func SessionDir(baseDir, sessionID string) string {
	return filepath.Join(baseDir, sessionID)
}

// NewSessionLogger opens the session log under the per-session directory
// <baseDir>/<sessionID>/session.jsonl (change 0030, keyed by the root
// AgentNode.ID). The directory is created if absent. Existing flat *.jsonl logs
// in baseDir are left untouched (read-compatible; no migration).
func NewSessionLogger(baseDir, sessionID string) (*Logger, error) {
	dir := SessionDir(baseDir, sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("session log dir: %w", err)
	}
	path := filepath.Join(dir, "session.jsonl")
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("session log create: %w", err)
	}
	return &Logger{f: f, w: bufio.NewWriter(f)}, nil
}

// Write appends a log entry to the session file. It returns any error and also
// latches the first failure in the Logger (see Err) so a hot per-child call site
// can ignore the per-write return and surface a single error at Close.
func (l *Logger) Write(entry LogEntry) error {
	b, err := json.Marshal(entry)
	if err != nil {
		l.setErr(err)
		return err
	}
	// bufio.Writer latches its own first error; Flush returns it. Writing then
	// flushing surfaces disk-full / closed-file failures that were previously
	// dropped by the //nolint:errcheck on the intermediate writes.
	_, _ = l.w.Write(b)
	_ = l.w.WriteByte('\n')
	if err := l.w.Flush(); err != nil {
		l.setErr(err)
		return err
	}
	return nil
}

func (l *Logger) setErr(err error) {
	if l.firstErr == nil {
		l.firstErr = err
	}
}

// Err returns the first write/flush error seen, or nil. Lets a caller that
// ignores per-Write returns still detect that session logging silently failed.
func (l *Logger) Err() error { return l.firstErr }

// Close flushes and closes the log file. It prefers to report a close/flush
// failure, falling back to any latched write error so a caller checking only
// Close still learns that logging failed.
func (l *Logger) Close() error {
	flushErr := l.w.Flush()
	closeErr := l.f.Close()
	switch {
	case flushErr != nil:
		return flushErr
	case closeErr != nil:
		return closeErr
	default:
		return l.firstErr
	}
}

// SweepOld deletes session log files older than maxAge from dir. Non-fatal.
func SweepOld(dir string, maxAge time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(dir, e.Name())) //nolint:errcheck
		}
	}
}
