// Package fsstore implements the concrete JSONL-backed EventStore for the loop
// event stream (change 0043). It mirrors internal/segment/fssink structurally — a
// mutex-guarded, per-session, append-only file writer — but events.jsonl is born
// PLAINTEXT (no gzip: the segment store owns the compressed pre-summary archive;
// resolution (b) of the segment-store-overlap open question keeps the two stores
// independent). It additionally fans out each appended event to live subscribers
// with a strictly non-blocking send, so a slow or absent consumer can never
// back-pressure Append or wedge an agent's scheduler slot (ADR-0016).
//
// fsstore is imported only by the composition root (cmd/fuse), like fssink.
package fsstore

import (
	"bufio"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/session"
)

func nowUTC() time.Time { return time.Now().UTC() }

// subBuffer is the per-subscriber channel capacity. Past it, the store drops the
// newest event for that subscriber and records a gap, never blocking Append.
const subBuffer = 256

type subscriber struct {
	ch chan event.Event
}

// FSEventStore writes one JSON Event per line to <baseDir>/<sessionID>/events.jsonl
// and fans out to live subscribers. A single mutex guards the Seq counter, the
// write handle, and the subscriber set — the loop is the only appender per session,
// but the guard keeps concurrent Appends (and children sharing the store) safe.
type FSEventStore struct {
	baseDir   string
	sessionID string

	mu      sync.Mutex
	seq     event.Seq
	f       *os.File
	w       *bufio.Writer
	subs    map[int]*subscriber
	nextSub int
	dropped uint64 // count of non-blocking-send drops (gap visibility)
	closed  bool
}

// NewFSEventStore opens (creating as needed) the per-session events.jsonl for
// append. The session dir is created eagerly here so Replay works even before the
// first Append; mirrors the per-session layout of ADR-0018.
func NewFSEventStore(baseDir, sessionID string) (*FSEventStore, error) {
	dir := session.SessionDir(baseDir, sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "events.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &FSEventStore{
		baseDir:   baseDir,
		sessionID: sessionID,
		f:         f,
		w:         bufio.NewWriter(f),
		subs:      map[int]*subscriber{},
	}, nil
}

func (s *FSEventStore) eventsPath() string {
	return filepath.Join(session.SessionDir(s.baseDir, s.sessionID), "events.jsonl")
}

// Append allocates the event's Seq, writes it durably (plaintext JSONL, flushed),
// then fans out to subscribers with a non-blocking send. A write error is returned
// (best-effort at the call site); a subscriber-full drop is NOT an error — it is
// recorded as a gap. Never blocks on a subscriber (ADR-0016).
func (s *FSEventStore) Append(e event.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return os.ErrClosed
	}

	s.seq++
	e.Seq = s.seq // store-allocated; caller Seq ignored
	if e.TS.IsZero() {
		e.TS = nowUTC()
	}

	line, err := e.MarshalJSONL()
	if err != nil {
		return err
	}
	if _, err := s.w.Write(line); err != nil {
		return err
	}
	if err := s.w.WriteByte('\n'); err != nil {
		return err
	}
	if err := s.w.Flush(); err != nil {
		return err
	}

	// Non-blocking fan-out. A full buffer drops the newest event for that
	// subscriber and bumps the gap counter — never back-pressures the loop.
	for _, sub := range s.subs {
		select {
		case sub.ch <- e:
		default:
			s.dropped++
		}
	}
	return nil
}

// Subscribe registers a buffered live-tail channel and returns it with an
// idempotent unsubscribe closure. Delivery is non-blocking (see Append).
func (s *FSEventStore) Subscribe() (<-chan event.Event, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextSub
	s.nextSub++
	sub := &subscriber{ch: make(chan event.Event, subBuffer)}
	s.subs[id] = sub

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if cur, ok := s.subs[id]; ok {
				delete(s.subs, id)
				close(cur.ch)
			}
		})
	}
	return sub.ch, cancel
}

// Replay reads durable history from events.jsonl and returns events with Seq >
// from (from == 0 ⇒ all), in Seq order. It opens the file independently of the
// write handle, so history is readable at any time.
func (s *FSEventStore) Replay(from event.Seq) ([]event.Event, error) {
	// Flush any buffered writes so the on-disk file is current for the read.
	s.mu.Lock()
	if s.w != nil && !s.closed {
		_ = s.w.Flush()
	}
	s.mu.Unlock()

	f, err := os.Open(s.eventsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var out []event.Event
	sc := bufio.NewScanner(f)
	// Large payloads (full tool results, assistant responses) can exceed the
	// default 64K token size; raise the cap generously.
	sc.Buffer(make([]byte, 0, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		ev, perr := event.ParseEvent(line)
		if perr != nil {
			// A single corrupt line is skipped, not fatal — the stream stays readable.
			continue
		}
		if ev.Seq > from {
			out = append(out, ev)
		}
	}
	if err := sc.Err(); err != nil {
		return out, err
	}
	return out, nil
}

// Dropped returns the number of non-blocking-send drops so far (gap visibility for
// tests and diagnostics).
func (s *FSEventStore) Dropped() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// Close flushes and closes the write handle and closes all subscriber channels.
// Idempotent.
func (s *FSEventStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var ferr error
	if s.w != nil {
		ferr = s.w.Flush()
	}
	if s.f != nil {
		if cerr := s.f.Close(); ferr == nil {
			ferr = cerr
		}
	}
	for id, sub := range s.subs {
		delete(s.subs, id)
		close(sub.ch)
	}
	return ferr
}
