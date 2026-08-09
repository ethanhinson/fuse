// Package session provides JSONL session logging and replay for agent trees.
package session

import (
	"bufio"
	"bytes"
	"compress/gzip"
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ethanhinson/fuse/internal/archive"
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

// SweepOld ARCHIVES files matching pattern (a filepath.Match glob, e.g.
// "*.jsonl") older than maxAge from dir: it gzip-compresses each to "<name>.gz"
// and writes a metadata sidecar describing WHAT is in the file, then removes the
// original — NON-DESTRUCTIVE (the data survives, compressed and self-describing).
// This is the change 0030 scope correction: age sweeps compress rather than
// delete. Already-archived ".gz" files are skipped. Non-fatal throughout,
// matching the original best-effort contract (a failed archive drops that one
// file, the sweep continues).
//
// The pattern parameter generalizes the previously hardcoded "*.jsonl" so the
// segment sweep can share the age/glob machinery. Session logs (the sole caller
// today, pattern "*.jsonl") get session-domain sidecar fields via logMeta.
func SweepOld(dir string, maxAge time.Duration, pattern string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		name := e.Name()
		if ok, _ := filepath.Match(pattern, name); !ok {
			continue
		}
		if strings.HasSuffix(name, archive.GzSuffix) {
			continue // already archived on a prior sweep
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			// Attach session-domain metadata for JSONL logs; other patterns fall
			// back to the common fields only.
			var mf archive.MetaFunc
			if strings.HasSuffix(name, ".jsonl") {
				mf = logMeta
			}
			_, _ = archive.Archive(filepath.Join(dir, name), mf) // best-effort
		}
	}
}

// SweepOldSegments is the NON-DESTRUCTIVE segment GC (change 0030 scope
// correction). New segments are born gzip-compressed as "*.md.gz" (see
// fssink.Archive), so the common case is a no-op. This sweep exists only to
// retroactively compress LEGACY plaintext "*.md" segments older than maxAge
// (baseDir/*/segments/*.md, 14-day window at the call site): it gzips each in
// place to "<name>.md.gz", removes the plaintext original, and re-points that
// entry's Path in index.json — the segment stays discoverable and recoverable,
// it is NEVER deleted. Because nothing is truly removed, the index is not pruned
// and non-empty session dirs are never torn down.
//
// Policy note: creation-time compression (Part A) is the primary mechanism; this
// age sweep is the back-compat bridge for segments that predate it. There is no
// second destructive horizon — segments are retained indefinitely, merely
// compressed. A future purge, if ever wanted, would be a separate long horizon.
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

// sweepSessionSegments compresses one session directory's legacy plaintext
// "*.md" segments older than cutoff in place, re-pointing their index.json Path
// entries. Born-compressed "*.md.gz" segments are skipped (already stored the
// new way). Nothing is deleted, so the index is never pruned and dirs are never
// torn down.
func sweepSessionSegments(sessionDir string, cutoff time.Time) {
	segDir := filepath.Join(sessionDir, "segments")
	segs, err := os.ReadDir(segDir)
	if err != nil {
		return
	}
	// renamed maps an old plaintext name to its new compressed name so the index
	// Path can be re-pointed.
	renamed := map[string]string{}
	for _, s := range segs {
		name := s.Name()
		// Only legacy plaintext .md (not the born-compressed .md.gz).
		if ok, _ := filepath.Match("*.md", name); !ok {
			continue
		}
		info, err := s.Info()
		if err != nil {
			continue
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		if compressSegmentInPlace(filepath.Join(segDir, name)) {
			renamed[name] = name + ".gz"
		}
	}
	if len(renamed) > 0 {
		repointIndex(filepath.Join(segDir, "index.json"), renamed)
	}
}

// compressSegmentInPlace gzips a legacy plaintext segment file to "<path>.gz"
// and removes the plaintext original. Non-fatal: on any error it leaves the
// plaintext in place (still readable) and reports false. Returns true only when
// the plaintext was successfully replaced by the compressed form.
func compressSegmentInPlace(path string) bool {
	content, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(content); err != nil {
		return false
	}
	if err := zw.Close(); err != nil {
		return false
	}
	if err := os.WriteFile(path+".gz", buf.Bytes(), 0o600); err != nil {
		return false
	}
	if err := os.Remove(path); err != nil {
		return false
	}
	return true
}

// repointIndex rewrites index.json so any entry whose Path was compressed points
// at the new ".gz" name. The entry is KEPT (still discoverable) — this sweep
// never prunes. A missing or unparseable index is left alone.
func repointIndex(idxPath string, renamed map[string]string) {
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
	changed := false
	for i, raw := range idx.Segments {
		var e map[string]any
		if json.Unmarshal(raw, &e) != nil {
			continue
		}
		p, _ := e["path"].(string)
		if np, ok := renamed[p]; ok {
			e["path"] = np
			if nb, merr := json.Marshal(e); merr == nil {
				idx.Segments[i] = nb
				changed = true
			}
		}
	}
	if !changed {
		return
	}
	out, err := json.Marshal(idx)
	if err != nil {
		return
	}
	os.WriteFile(idxPath, out, 0o600) //nolint:errcheck
}
