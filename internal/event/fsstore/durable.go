package fsstore

// durable.go adds the change-0047 durable, tenant-partitioned event store —
// FSDurableStore — the process-local backend for the event.DurableStore +
// event.LoopRegistry seams. It is a SEPARATE type from the legacy FSEventStore
// (store.go): the legacy store satisfies the no-key event.EventStore interface
// (Append(Event)/Subscribe()/Replay(Seq)) that pre-0047 callers depend on, and Go
// cannot host both those method sets and the context+key durable set on one type.
// FSDurableStore keys every stream by event.StreamKey and partitions storage under
// <baseDir>/<tenant>/<loop>/events.jsonl.
//
// Storage format is byte-identical to the legacy store (plaintext JSONL, ADR-0024)
// and the fan-out is the same non-blocking drop-newest-with-gap (ADR-0016); only
// the addressing (per-StreamKey, lazily opened) and the tenant subdir layout are
// new.

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"sync"

	"github.com/ethanhinson/fuse/internal/event"
)

// dstream is the per-(tenant, loop) durable state for FSDurableStore: the Seq
// counter, the append handle, and the live-subscriber set for one events.jsonl
// file. Access is guarded by the owning store's single mutex.
type dstream struct {
	dir string // <baseDir>/<tenant>/<loop>

	seq     event.Seq
	f       *os.File
	w       *bufio.Writer
	subs    map[int]*subscriber
	nextSub int
	dropped uint64 // count of non-blocking-send drops (gap visibility)
}

func (st *dstream) eventsPath() string { return filepath.Join(st.dir, "events.jsonl") }

// FSDurableStore is the process-local durable event store (change 0047). One
// instance serves many (tenant, loop) streams keyed by event.StreamKey; a single
// mutex guards the stream map and each stream's Seq counter, write handle, and
// subscriber set. It satisfies event.DurableStore and event.LoopRegistry.
type FSDurableStore struct {
	baseDir string

	mu      sync.Mutex
	streams map[event.StreamKey]*dstream
	closed  bool
}

// compile-time assertion (the LoopRegistry assertion lands with its methods in
// registry.go).
var _ event.DurableStore = (*FSDurableStore)(nil)

// NewDurableFSStore creates a durable, tenant-partitioned event store rooted at
// baseDir. Streams are opened lazily on first use at
// <baseDir>/<tenant>/<loop>/events.jsonl (tenant via event.NormalizeTenant).
func NewDurableFSStore(baseDir string) *FSDurableStore {
	return &FSDurableStore{
		baseDir: baseDir,
		streams: map[event.StreamKey]*dstream{},
	}
}

// normalizeKey applies DefaultTenant so the empty tenant and DefaultTenant address
// the same stream.
func normalizeKey(k event.StreamKey) event.StreamKey {
	return event.StreamKey{Tenant: event.NormalizeTenant(k.Tenant), Loop: k.Loop}
}

// streamDir computes the on-disk directory for a normalized StreamKey.
func (s *FSDurableStore) streamDir(k event.StreamKey) string {
	nk := normalizeKey(k)
	return filepath.Join(s.baseDir, string(nk.Tenant), string(nk.Loop))
}

// getStream returns the lazily-opened stream for key (caller holds s.mu). A stream
// reopened over an existing file recovers its Seq from disk so the per-loop total
// order continues.
func (s *FSDurableStore) getStream(k event.StreamKey) (*dstream, error) {
	nk := normalizeKey(k)
	if st, ok := s.streams[nk]; ok {
		return st, nil
	}
	dir := filepath.Join(s.baseDir, string(nk.Tenant), string(nk.Loop))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	st := &dstream{
		dir:  dir,
		f:    f,
		w:    bufio.NewWriter(f),
		subs: map[int]*subscriber{},
	}
	last, err := lastSeqInFile(st.eventsPath())
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	st.seq = last
	s.streams[nk] = st
	return st, nil
}

// Append records one event on the (tenant, loop) stream named by key. The store
// allocates e.Seq per-loop from 1; callers pass Seq == 0. It honors ctx.Err() at
// entry, writes durably (plaintext JSONL, flushed), then fans out non-blocking.
// Never blocks on a subscriber (ADR-0016).
func (s *FSDurableStore) Append(ctx context.Context, key event.StreamKey, e event.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return os.ErrClosed
	}
	st, err := s.getStream(key)
	if err != nil {
		return err
	}

	st.seq++
	e.Seq = st.seq // store-allocated; caller Seq ignored
	if e.TS.IsZero() {
		e.TS = nowUTC()
	}
	line, err := e.MarshalJSONL()
	if err != nil {
		return err
	}
	if _, err := st.w.Write(line); err != nil {
		return err
	}
	if err := st.w.WriteByte('\n'); err != nil {
		return err
	}
	if err := st.w.Flush(); err != nil {
		return err
	}
	// Non-blocking fan-out: a full buffer drops the newest event and bumps the gap
	// counter — never back-pressures the loop (ADR-0016).
	for _, sub := range st.subs {
		select {
		case sub.ch <- e:
		default:
			st.dropped++
		}
	}
	return nil
}

// Subscribe returns a live tail of the (tenant, loop) stream plus an idempotent
// unsubscribe closure. Delivery is non-blocking (see Append).
func (s *FSDurableStore) Subscribe(ctx context.Context, key event.StreamKey) (<-chan event.Event, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, nil, os.ErrClosed
	}
	st, err := s.getStream(key)
	if err != nil {
		return nil, nil, err
	}
	id := st.nextSub
	st.nextSub++
	sub := &subscriber{ch: make(chan event.Event, subBuffer)}
	st.subs[id] = sub

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if cur, ok := st.subs[id]; ok {
				delete(st.subs, id)
				close(cur.ch)
			}
		})
	}
	return sub.ch, cancel, nil
}

// Replay returns durable history for the (tenant, loop) stream with Seq > from
// (from == 0 ⇒ all), in Seq order, tenant-scoped.
func (s *FSDurableStore) Replay(ctx context.Context, key event.StreamKey, from event.Seq) ([]event.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	nk := normalizeKey(key)
	if st, ok := s.streams[nk]; ok && !s.closed && st.w != nil {
		_ = st.w.Flush()
	}
	path := filepath.Join(s.streamDir(key), "events.jsonl")
	s.mu.Unlock()
	return replayJSONLFile(path, from)
}

// DroppedFor returns the number of non-blocking-send drops for the given stream so
// far (gap visibility for tests and diagnostics).
func (s *FSDurableStore) DroppedFor(key event.StreamKey) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.streams[normalizeKey(key)]; ok {
		return st.dropped
	}
	return 0
}

// Dropped returns the total non-blocking-send drops across all streams so the
// conformance suite's dropCounter check works without knowing the active StreamKey.
func (s *FSDurableStore) Dropped() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var total uint64
	for _, st := range s.streams {
		total += st.dropped
	}
	return total
}

// Close flushes and closes every stream's write handle and closes all subscriber
// channels. Idempotent.
func (s *FSDurableStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var ferr error
	for _, st := range s.streams {
		if st.w != nil {
			if err := st.w.Flush(); err != nil && ferr == nil {
				ferr = err
			}
		}
		if st.f != nil {
			if cerr := st.f.Close(); cerr != nil && ferr == nil {
				ferr = cerr
			}
		}
		for id, sub := range st.subs {
			delete(st.subs, id)
			close(sub.ch)
		}
	}
	return ferr
}

// replayJSONLFile reads durable history from path and returns events with Seq >
// from (from == 0 ⇒ all), in Seq order. It opens the file independently of any
// write handle, so history is readable at any time. Shared by Replay and Seq
// recovery.
func replayJSONLFile(path string, from event.Seq) ([]event.Event, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var out []event.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		ev, perr := event.ParseEvent(line)
		if perr != nil {
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

// lastSeqInFile returns the highest Seq recorded in the file at path (0 if
// absent/empty), so a reopened stream continues the per-loop total order.
func lastSeqInFile(path string) (event.Seq, error) {
	evs, err := replayJSONLFile(path, 0)
	if err != nil {
		return 0, err
	}
	var max event.Seq
	for _, e := range evs {
		if e.Seq > max {
			max = e.Seq
		}
	}
	return max, nil
}
