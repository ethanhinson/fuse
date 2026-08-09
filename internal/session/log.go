// Package session provides JSONL session logging and replay for agent trees.
package session

import (
	"bufio"
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// SweepOld deletes files matching pattern (a filepath.Match glob, e.g.
// "*.jsonl") older than maxAge from dir. Non-fatal. The pattern parameter
// (change 0030) generalizes the previously hardcoded "*.jsonl" so the segment
// sweep can share the age/glob machinery.
func SweepOld(dir string, maxAge time.Duration, pattern string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		if ok, _ := filepath.Match(pattern, e.Name()); !ok {
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

// SweepOldSegments sweeps stale segment files under baseDir/*/segments/*.md
// older than maxAge (change 0030, 14-day window at the call site), prunes their
// entries from each session's index.json, and removes a session directory left
// empty after the sweep. Non-fatal throughout — GC never blocks a session.
//
// Symlink safety (learning dirent-isdir-skips-symlinks): a session dir reached
// through a symlink reports DirEntry.IsDir() == false, so the descent falls back
// to os.Stat rather than trusting the dirent, otherwise a symlinked session dir
// would be silently skipped.
func SweepOldSegments(baseDir string, maxAge time.Duration) {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		isDir := e.IsDir()
		if !isDir {
			// Follow symlinks (and anything IsDir() misreports) via a stat.
			if info, serr := os.Stat(filepath.Join(baseDir, e.Name())); serr == nil && info.IsDir() {
				isDir = true
			}
		}
		if !isDir {
			continue
		}
		sweepSessionSegments(filepath.Join(baseDir, e.Name()), cutoff)
	}
}

// sweepSessionSegments sweeps one session directory's segments/ subtree, prunes
// its index.json, and removes the session dir if it is left empty.
func sweepSessionSegments(sessionDir string, cutoff time.Time) {
	segDir := filepath.Join(sessionDir, "segments")
	segs, err := os.ReadDir(segDir)
	if err != nil {
		return
	}
	swept := map[string]bool{}
	for _, s := range segs {
		if ok, _ := filepath.Match("*.md", s.Name()); !ok {
			continue
		}
		info, err := s.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if os.Remove(filepath.Join(segDir, s.Name())) == nil {
				swept[s.Name()] = true
			}
		}
	}
	if len(swept) > 0 {
		pruneIndex(filepath.Join(segDir, "index.json"), swept)
	}
	removeIfEmpty(segDir)
	removeIfEmpty(sessionDir)
}

// pruneIndex rewrites index.json to drop entries whose Path was swept. A missing
// or unparseable index is left alone.
func pruneIndex(idxPath string, swept map[string]bool) {
	b, err := os.ReadFile(idxPath)
	if err != nil {
		return
	}
	var idx struct {
		SessionID string            `json:"session_id"`
		Segments  []json.RawMessage `json:"segments"`
	}
	if err := json.Unmarshal(b, &idx); err != nil {
		return
	}
	kept := idx.Segments[:0]
	for _, raw := range idx.Segments {
		var e struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(raw, &e) == nil && swept[e.Path] {
			continue
		}
		kept = append(kept, raw)
	}
	idx.Segments = kept
	// If no segments remain, drop the index file so the dir can be removed.
	if len(idx.Segments) == 0 {
		os.Remove(idxPath) //nolint:errcheck
		return
	}
	out, err := json.Marshal(idx)
	if err != nil {
		return
	}
	os.WriteFile(idxPath, out, 0o600) //nolint:errcheck
}

// removeIfEmpty removes dir if it contains no entries. Non-fatal.
func removeIfEmpty(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	if len(entries) == 0 {
		os.Remove(dir) //nolint:errcheck
	}
}
