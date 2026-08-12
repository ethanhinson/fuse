package observe

import (
	"context"
	"sync"

	"github.com/ethanhinson/fuse/internal/event"
)

const defaultDedupCapacity = 4096

// ProjectionFailure is deliberately bounded: callbacks receive event identity
// and a fixed category, never a projector's raw error value.
type ProjectionFailure struct {
	TenantID  event.TenantID
	LoopID    event.LoopID
	Sequence  event.Seq
	EventName string
	Category  ErrorCategory
}

// FailureReporter receives best-effort projection failures. It must return
// promptly and must not attempt to retry an event into the loop.
type FailureReporter func(ProjectionFailure)

// Runner projects a durable stream. It subscribes before replaying so that
// events appended during the replay handoff are buffered, then deduplicates the
// overlap by tenant, loop, and sequence.
type Runner struct {
	store     event.DurableStore
	key       event.StreamKey
	projector Projector
	report    FailureReporter
	capacity  int

	mu        sync.Mutex
	cancel    func()
	closed    chan struct{}
	closeOnce sync.Once
}

func NewRunner(store event.DurableStore, key event.StreamKey, projector Projector, report FailureReporter) *Runner {
	return &Runner{store: store, key: event.StreamKey{Tenant: event.NormalizeTenant(key.Tenant), Loop: key.Loop}, projector: projector, report: report, capacity: defaultDedupCapacity, closed: make(chan struct{})}
}

// Close stops a live subscription and is safe to call repeatedly.
func (r *Runner) Close() {
	r.closeOnce.Do(func() { close(r.closed) })
	r.mu.Lock()
	cancel := r.cancel
	r.cancel = nil
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Run owns no background goroutine. It returns on context cancellation, Close,
// or an underlying store failure; projection failures are reported and skipped.
func (r *Runner) Run(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-r.closed:
		return nil
	default:
	}
	live, cancel, err := r.store.Subscribe(ctx, r.key)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.cancel = cancel
	r.mu.Unlock()
	defer r.Close()
	select {
	case <-r.closed:
		return nil
	default:
	}

	dedup := newDeduper(r.capacity)
	history, err := r.store.Replay(ctx, r.key, 0)
	if err != nil {
		return err
	}
	for _, e := range history {
		r.project(ctx, e, dedup)
	}
	for {
		select {
		case <-r.closed:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case e, ok := <-live:
			if !ok {
				return nil
			}
			r.project(ctx, e, dedup)
		}
	}
}

func (r *Runner) project(ctx context.Context, e event.Event, dedup *deduper) {
	if !dedup.Add(r.key, e.Seq) || r.projector == nil {
		return
	}
	record := ProjectEvent(r.key, e)
	if err := r.projector.Project(ctx, record); err != nil && r.report != nil {
		r.report(ProjectionFailure{TenantID: record.TenantID, LoopID: record.LoopID, Sequence: record.Sequence, EventName: record.EventName, Category: ErrorCategoryProjection})
	}
}

type dedupKey struct {
	tenant event.TenantID
	loop   event.LoopID
	seq    event.Seq
}
type deduper struct {
	capacity int
	seen     map[dedupKey]struct{}
	order    []dedupKey
}

func newDeduper(capacity int) *deduper {
	if capacity < 1 {
		capacity = 1
	}
	return &deduper{capacity: capacity, seen: make(map[dedupKey]struct{}, capacity)}
}
func (d *deduper) Add(key event.StreamKey, seq event.Seq) bool {
	k := dedupKey{tenant: event.NormalizeTenant(key.Tenant), loop: key.Loop, seq: seq}
	if _, ok := d.seen[k]; ok {
		return false
	}
	d.seen[k] = struct{}{}
	d.order = append(d.order, k)
	if len(d.order) > d.capacity {
		delete(d.seen, d.order[0])
		d.order = d.order[1:]
	}
	return true
}
