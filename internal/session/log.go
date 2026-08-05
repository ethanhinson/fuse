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
	crand.Read(buf[:])
	name := fmt.Sprintf("%s-%x.jsonl", time.Now().Format("2006-01-02"), buf)
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("session log create: %w", err)
	}
	return &Logger{f: f, w: bufio.NewWriter(f)}, nil
}

// Write appends a log entry to the session file.
func (l *Logger) Write(entry LogEntry) error {
	b, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	l.w.Write(b)        //nolint:errcheck
	l.w.WriteByte('\n') //nolint:errcheck
	return l.w.Flush()
}

// Close flushes and closes the log file.
func (l *Logger) Close() error {
	if err := l.w.Flush(); err != nil {
		return err
	}
	return l.f.Close()
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
