package segment

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
)

// FSSegmentSink is the concrete filesystem SegmentSink (change 0030). It writes
// one markdown segment file per Archive under
// <baseDir>/<sessionID>/segments/ and appends one entry to that session's
// index.json. The session dir is created lazily on the first Archive. A single
// in-process mutex guards the read-modify-write of index.json — the root loop is
// the only writer per session, but the guard makes concurrent Archive calls
// (and children sharing the sink) safe.
type FSSegmentSink struct {
	baseDir   string // <sessions> root, e.g. ~/.fuse/sessions
	sessionID string

	mu  sync.Mutex
	seq int // monotonic disambiguator for repeated turn ranges
}

// NewFSSegmentSink builds a filesystem sink writing under
// <baseDir>/<sessionID>/segments/.
func NewFSSegmentSink(baseDir, sessionID string) *FSSegmentSink {
	return &FSSegmentSink{baseDir: baseDir, sessionID: sessionID}
}

// segmentsDir is the session's segments directory.
func (s *FSSegmentSink) segmentsDir() string {
	return filepath.Join(s.baseDir, s.sessionID, "segments")
}

// Archive persists the raw pre-summarization region and returns the absolute
// path of the written segment file (the recovery pointer). Errors are returned
// so the loop can log them; the loop treats a non-nil error as best-effort
// (pointer dropped, turn continues).
func (s *FSSegmentSink) Archive(r agent.SegmentRegion) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := s.segmentsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("segment dir: %w", err)
	}

	ts := time.Now().UTC()

	// Pick a file name whose seq disambiguates a repeated turn range: probe from
	// the in-process counter, then skip past any name that already exists on disk.
	s.seq++
	seq := s.seq
	name := FileName(r.TurnStart, r.TurnEnd, seq)
	for fileExists(filepath.Join(dir, name)) {
		s.seq++
		seq = s.seq
		name = FileName(r.TurnStart, r.TurnEnd, seq)
	}
	path := filepath.Join(dir, name)

	seg := Segment{
		TurnStart:    r.TurnStart,
		TurnEnd:      r.TurnEnd,
		Tools:        r.ToolNames,
		TokensBefore: r.TokensBefore,
		TokensAfter:  r.TokensAfter,
		TS:           ts,
		Summary:      r.Summary,
		Messages:     r.Messages,
	}
	if err := os.WriteFile(path, []byte(RenderSegment(seg)), 0o600); err != nil {
		return "", fmt.Errorf("segment write: %w", err)
	}

	if err := s.appendIndex(dir, IndexEntry{
		TurnStart:    r.TurnStart,
		TurnEnd:      r.TurnEnd,
		Tools:        r.ToolNames,
		TokensBefore: r.TokensBefore,
		TokensAfter:  r.TokensAfter,
		Path:         name,
		TS:           ts,
	}); err != nil {
		return "", fmt.Errorf("segment index: %w", err)
	}
	return path, nil
}

// appendIndex reads (or initializes) the session's index.json, appends entry,
// and writes it back. Caller holds s.mu.
func (s *FSSegmentSink) appendIndex(dir string, entry IndexEntry) error {
	idxPath := filepath.Join(dir, "index.json")
	idx := Index{SessionID: s.sessionID}
	if b, err := os.ReadFile(idxPath); err == nil {
		_ = json.Unmarshal(b, &idx) // a corrupt index is rebuilt, not fatal
		idx.SessionID = s.sessionID
	}
	idx.Segments = append(idx.Segments, entry)
	out, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	return os.WriteFile(idxPath, out, 0o600)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
