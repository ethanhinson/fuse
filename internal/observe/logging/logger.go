package logging

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/ethanhinson/fuse/internal/observe"
)

type Identity struct {
	Service  string
	Instance string
}
type Entry struct {
	Timestamp     time.Time `json:"timestamp"`
	Level         Level     `json:"-"`
	Name          string    `json:"event_name"`
	Message       string    `json:"message,omitempty"`
	TraceID       string    `json:"trace_id,omitempty"`
	SpanID        string    `json:"span_id,omitempty"`
	TenantID      string    `json:"tenant_id,omitempty"`
	LoopID        string    `json:"loop_id,omitempty"`
	NodeID        string    `json:"node_id,omitempty"`
	Sequence      uint64    `json:"sequence,omitempty"`
	Operation     string    `json:"operation,omitempty"`
	Outcome       string    `json:"outcome,omitempty"`
	ErrorCategory string    `json:"error_category,omitempty"`
}
type Audit struct {
	Timestamp     time.Time `json:"timestamp"`
	Actor         string    `json:"actor"`
	Action        string    `json:"action"`
	Scope         string    `json:"scope"`
	Previous      string    `json:"previous,omitempty"`
	New           string    `json:"new,omitempty"`
	ExpiresAt     time.Time `json:"expires_at,omitempty"`
	CorrelationID string    `json:"correlation_id,omitempty"`
	Outcome       string    `json:"outcome"`
}
type sink interface {
	io.Writer
	io.Closer
}
type nopCloseWriter struct{ io.Writer }

func (nopCloseWriter) Close() error { return nil }

type Logger struct {
	mu        sync.RWMutex
	writeMu   sync.Mutex
	sink      sink
	levels    *Levels
	identity  Identity
	closed    bool
	closeOnce sync.Once
	closeErr  error
}

func New(w io.Writer, levels *Levels, identity Identity) *Logger {
	if w == nil {
		w = os.Stdout
	}
	s, ok := w.(sink)
	if !ok {
		s = nopCloseWriter{w}
	}
	return &Logger{sink: s, levels: levels, identity: identity}
}
func (l *Logger) Project(ctx context.Context, r observe.Record) error {
	level := LevelInfo
	if r.Outcome != observe.OutcomeSuccess {
		level = LevelError
	}
	return l.Log(ctx, Entry{Timestamp: r.Timestamp, Level: level, Name: r.EventName, TenantID: string(r.TenantID), LoopID: string(r.LoopID), NodeID: r.NodeID, Sequence: uint64(r.Sequence), Operation: string(r.Operation), Outcome: string(r.Outcome), ErrorCategory: string(r.ErrorCategory)})
}
func (l *Logger) Log(_ context.Context, e Entry) error {
	if l.levels != nil && e.Level < l.levels.Effective(e.TenantID, e.LoopID) {
		return nil
	}
	return l.write(e, "routine")
}
func (l *Logger) Audit(_ context.Context, a Audit) error {
	if a.Timestamp.IsZero() {
		a.Timestamp = time.Now().UTC()
	}
	return l.write(a, "audit")
}
func (l *Logger) write(v any, kind string) error {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		return os.ErrClosed
	}
	var body any
	switch x := v.(type) {
	case Entry:
		if x.Timestamp.IsZero() {
			x.Timestamp = time.Now().UTC()
		}
		body = struct {
			Kind          string    `json:"kind"`
			Severity      string    `json:"severity"`
			Service       string    `json:"service"`
			Instance      string    `json:"instance_id"`
			Timestamp     time.Time `json:"timestamp"`
			EventName     string    `json:"event_name"`
			Message       string    `json:"message,omitempty"`
			TraceID       string    `json:"trace_id,omitempty"`
			SpanID        string    `json:"span_id,omitempty"`
			TenantID      string    `json:"tenant_id,omitempty"`
			LoopID        string    `json:"loop_id,omitempty"`
			NodeID        string    `json:"node_id,omitempty"`
			Sequence      uint64    `json:"sequence,omitempty"`
			Operation     string    `json:"operation,omitempty"`
			Outcome       string    `json:"outcome,omitempty"`
			ErrorCategory string    `json:"error_category,omitempty"`
		}{kind, x.Level.String(), l.identity.Service, l.identity.Instance, x.Timestamp, x.Name, x.Message, x.TraceID, x.SpanID, x.TenantID, x.LoopID, x.NodeID, x.Sequence, x.Operation, x.Outcome, x.ErrorCategory}
	case Audit:
		body = struct {
			Kind          string    `json:"kind"`
			Severity      string    `json:"severity"`
			Service       string    `json:"service"`
			Instance      string    `json:"instance_id"`
			Timestamp     time.Time `json:"timestamp"`
			EventName     string    `json:"event_name"`
			Actor         string    `json:"actor"`
			Action        string    `json:"action"`
			Scope         string    `json:"scope"`
			Previous      string    `json:"previous,omitempty"`
			New           string    `json:"new,omitempty"`
			ExpiresAt     time.Time `json:"expires_at,omitempty"`
			CorrelationID string    `json:"correlation_id,omitempty"`
			Outcome       string    `json:"outcome"`
		}{kind, LevelInfo.String(), l.identity.Service, l.identity.Instance, x.Timestamp, "logging.audit", x.Actor, x.Action, x.Scope, x.Previous, x.New, x.ExpiresAt, x.CorrelationID, x.Outcome}
	default:
		return errors.New("logging: unsupported record type")
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	// A record is admitted to the sink with exactly one serialized Write call.
	// The mutex also supports writers whose own Write method is not concurrent-safe.
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	n, err := l.sink.Write(b)
	if err == nil && n != len(b) {
		return io.ErrShortWrite
	}
	return err
}
func (l *Logger) Reopen() error {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		return os.ErrClosed
	}
	r, ok := l.sink.(interface{ Reopen() error })
	if !ok {
		return errors.New("logging: sink does not support reopen")
	}
	return r.Reopen()
}
func (l *Logger) Close() error {
	l.closeOnce.Do(func() { l.mu.Lock(); defer l.mu.Unlock(); l.closed = true; l.closeErr = l.sink.Close() })
	return l.closeErr
}
