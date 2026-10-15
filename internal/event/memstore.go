package event

import (
	"sync"
	"time"
)

// DefaultMemStoreCap is how many events a MemStore keeps when no cap is given.
const DefaultMemStoreCap = 20000

// memSubBuffer is each live subscriber's channel buffer, the same depth as the file
// store's.
const memSubBuffer = 256

// MemStore is an in-memory EventStore for a run that wants its event stream observable
// (replay plus live tail) but writes no files - a one-shot run with a control socket.
// It keeps the newest cap events; the oldest fall off, but Seq keeps rising, so a reader
// that asks for history older than the window simply gets what remains.
//
// It follows the EventStore contract: Append allocates Seq and never blocks on a slow
// subscriber (a full subscriber buffer drops that event for that subscriber).
type MemStore struct {
	mu      sync.Mutex
	cap     int
	events  []Event
	seq     Seq
	subs    map[int]chan Event
	nextSub int
	closed  bool
}

// NewMemStore returns an empty MemStore keeping at most cap events (DefaultMemStoreCap
// when cap <= 0).
func NewMemStore(cap int) *MemStore {
	if cap <= 0 {
		cap = DefaultMemStoreCap
	}
	return &MemStore{cap: cap, subs: map[int]chan Event{}}
}

// Append records e with a store-allocated Seq and fans it out to live subscribers.
func (s *MemStore) Append(e Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil // emission after close is dropped, like the no-op store
	}
	s.seq++
	e.Seq = s.seq
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	s.events = append(s.events, e)
	if over := len(s.events) - s.cap; over > 0 {
		s.events = append([]Event(nil), s.events[over:]...)
	}
	for _, ch := range s.subs {
		select {
		case ch <- e:
		default: // never back-pressure the loop
		}
	}
	return nil
}

// Subscribe returns a live tail from now plus an idempotent unsubscribe.
func (s *MemStore) Subscribe() (<-chan Event, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan Event, memSubBuffer)
	if s.closed {
		close(ch)
		return ch, func() {}
	}
	id := s.nextSub
	s.nextSub++
	s.subs[id] = ch
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if cur, ok := s.subs[id]; ok {
				delete(s.subs, id)
				close(cur)
			}
		})
	}
}

// Replay returns the kept events with Seq > from, in Seq order.
func (s *MemStore) Replay(from Seq) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Event
	for _, e := range s.events {
		if e.Seq > from {
			out = append(out, e)
		}
	}
	return out, nil
}

// Close ends every live subscription. Replay keeps working.
func (s *MemStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	for id, ch := range s.subs {
		delete(s.subs, id)
		close(ch)
	}
	return nil
}
