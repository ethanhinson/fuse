// Package tenantlimit bounds, per tenant, how many loops a tenant keeps alive
// and how fast it starts them on a shared loop server (change 0103). It is the
// operator floor the Connect edge enforces after authentication: the start rate
// is a process-local token bucket per tenant, and the live-loop cap counts the
// durable registry (the cross-process truth) plus this process's in-flight
// starts. It imports only internal/event and the standard library.
package tenantlimit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
)

// Limits is one tenant's bounds. 0 on an axis means unlimited.
type Limits struct {
	// MaxLiveLoops is the most loops the tenant may have live at once.
	MaxLiveLoops int
	// StartsPerMinute is the most start attempts per minute, as a token bucket
	// with one minute of burst.
	StartsPerMinute int
}

// Config is the operator's limits: Default for every tenant, overridden per
// tenant id axis by axis. An override axis > 0 replaces the default axis; 0
// (or omitted) inherits it, so an override cannot make an axis unlimited while
// the default limits it.
type Config struct {
	Default Limits
	Tenants map[string]Limits
}

// Any reports whether any axis of the default or of any override is limited.
func (c Config) Any() bool {
	if c.Default.MaxLiveLoops > 0 || c.Default.StartsPerMinute > 0 {
		return true
	}
	for _, o := range c.Tenants {
		if o.MaxLiveLoops > 0 || o.StartsPerMinute > 0 {
			return true
		}
	}
	return false
}

// anyCap reports whether any axis sets a live-loop cap.
func (c Config) anyCap() bool {
	if c.Default.MaxLiveLoops > 0 {
		return true
	}
	for _, o := range c.Tenants {
		if o.MaxLiveLoops > 0 {
			return true
		}
	}
	return false
}

// For returns the tenant's effective limits: the default with the tenant's
// override applied per axis. The empty tenant is the default tenant.
func (c Config) For(tenant event.TenantID) Limits {
	eff := Limits{
		MaxLiveLoops:    max(c.Default.MaxLiveLoops, 0),
		StartsPerMinute: max(c.Default.StartsPerMinute, 0),
	}
	o, ok := c.Tenants[string(event.NormalizeTenant(tenant))]
	if !ok {
		return eff
	}
	if o.MaxLiveLoops > 0 {
		eff.MaxLiveLoops = o.MaxLiveLoops
	}
	if o.StartsPerMinute > 0 {
		eff.StartsPerMinute = o.StartsPerMinute
	}
	return eff
}

// Axis names the limit that refused a start, in its config spelling.
type Axis string

const (
	AxisLiveLoops Axis = "max_live_loops"
	AxisStartRate Axis = "starts_per_minute"
)

// RefusedError is a start refused by a limit. RetryAfter is set only for the
// rate axis, as a whole number of seconds (at least one).
type RefusedError struct {
	Tenant     event.TenantID
	Axis       Axis
	Limit      int
	RetryAfter time.Duration
}

func (e *RefusedError) Error() string {
	if e.Axis == AxisStartRate {
		return fmt.Sprintf("tenant %q exceeded its loop_server.limits %s (%d); retry after %s",
			e.Tenant, e.Axis, e.Limit, e.RetryAfter)
	}
	return fmt.Sprintf("tenant %q is at its loop_server.limits %s (%d); wait for a loop to finish",
		e.Tenant, e.Axis, e.Limit)
}

// LiveCounter reports how many loops a tenant has live right now.
type LiveCounter func(ctx context.Context, tenant event.TenantID) (int, error)

// RegistryCounter counts the tenant's registry records that are live and not
// abandoned, by the runtime's own rule: Live, and either no lease or a lease
// that has not passed. A record keyed to another tenant is never counted. A nil
// registry yields a nil counter, so New refuses a cap with nothing to count.
func RegistryCounter(reg event.LoopRegistry, now func() time.Time) LiveCounter {
	if reg == nil {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	return func(ctx context.Context, tenant event.TenantID) (int, error) {
		nt := event.NormalizeTenant(tenant)
		recs, err := reg.List(ctx, nt)
		if err != nil {
			return 0, err
		}
		at := now()
		n := 0
		for _, rec := range recs {
			if event.NormalizeTenant(rec.Key.Tenant) != nt || !rec.Live {
				continue
			}
			if rec.LeaseExpiry.IsZero() || !at.After(rec.LeaseExpiry) {
				n++
			}
		}
		return n, nil
	}
}

// Limiter enforces Config. It is safe for concurrent use; every map is keyed by
// the normalized tenant, so one tenant's state never touches another's.
type Limiter struct {
	cfg   Config
	count LiveCounter
	now   func() time.Time

	mu      sync.Mutex
	buckets map[event.TenantID]*bucket
	pending map[event.TenantID]int
	// locks serializes count-and-reserve per tenant. Each is a one-slot
	// semaphore rather than a sync.Mutex so a waiter can give up on ctx.
	locks map[event.TenantID]chan struct{}
}

// New builds a Limiter. now defaults to time.Now. It refuses a config that sets
// any max_live_loops with a nil counter: a cap with nothing to count is inert.
func New(cfg Config, count LiveCounter, now func() time.Time) (*Limiter, error) {
	if cfg.anyCap() && count == nil {
		return nil, errors.New("tenantlimit: max_live_loops is set but there is no live-loop counter")
	}
	if now == nil {
		now = time.Now
	}
	return &Limiter{
		cfg:     cfg,
		count:   count,
		now:     now,
		buckets: map[event.TenantID]*bucket{},
		pending: map[event.TenantID]int{},
		locks:   map[event.TenantID]chan struct{}{},
	}, nil
}

// For returns the tenant's effective limits. A nil Limiter is unlimited.
func (l *Limiter) For(tenant event.TenantID) Limits {
	if l == nil {
		return Limits{}
	}
	return l.cfg.For(tenant)
}

func noop() {}

// Admit decides whether the tenant may start one more loop. The rate is checked
// first (in memory, so a hammering client cannot drive registry reads), and
// every call that reaches it spends a start token, even one the cap then
// refuses. The cap counts live loops plus this process's in-flight starts under
// a per-tenant lock, so concurrent starts cannot both take the last slot.
//
// On success the caller must call release once the start has finished, whether
// it succeeded or failed; release is idempotent and never nil. A refusal is a
// *RefusedError; any other error (a failed count, a cancelled ctx) means the
// cap could not be checked and the start must not proceed. A nil Limiter
// admits everything.
func (l *Limiter) Admit(ctx context.Context, tenant event.TenantID) (release func(), err error) {
	if l == nil {
		return noop, nil
	}
	t := event.NormalizeTenant(tenant)
	lim := l.cfg.For(t)
	if lim.MaxLiveLoops == 0 && lim.StartsPerMinute == 0 {
		return noop, nil
	}

	if lim.StartsPerMinute > 0 {
		now := l.now()
		l.mu.Lock()
		b, ok := l.buckets[t]
		if !ok {
			b = newBucket(lim.StartsPerMinute, now)
			l.buckets[t] = b
		}
		wait, ok := b.take(now)
		l.mu.Unlock()
		if !ok {
			return nil, &RefusedError{Tenant: t, Axis: AxisStartRate, Limit: lim.StartsPerMinute, RetryAfter: wait}
		}
	}

	if lim.MaxLiveLoops == 0 {
		return noop, nil
	}

	l.mu.Lock()
	sem, ok := l.locks[t]
	if !ok {
		sem = make(chan struct{}, 1)
		l.locks[t] = sem
	}
	l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-sem }()

	n, err := l.count(ctx, t)
	if err != nil {
		return nil, fmt.Errorf("tenantlimit: count live loops for %q: %w", t, err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if n+l.pending[t] >= lim.MaxLiveLoops {
		return nil, &RefusedError{Tenant: t, Axis: AxisLiveLoops, Limit: lim.MaxLiveLoops}
	}
	l.pending[t]++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.pending[t] <= 1 {
				delete(l.pending, t)
				return
			}
			l.pending[t]--
		})
	}, nil
}

// tokenEpsilon absorbs float rounding so a caller who waits exactly the
// advertised Retry-After is admitted.
const tokenEpsilon = 1e-9

// bucket is a refusing token bucket: capacity perMinute (one minute of burst),
// refilled at perMinute/60 tokens per second, starting full. Callers hold
// Limiter.mu.
type bucket struct {
	perMinute float64
	tokens    float64
	last      time.Time
}

func newBucket(perMinute int, now time.Time) *bucket {
	return &bucket{perMinute: float64(perMinute), tokens: float64(perMinute), last: now}
}

// take refills to now and spends one token. When none is available it returns
// false and how long until one is, in whole seconds (at least one).
func (b *bucket) take(now time.Time) (time.Duration, bool) {
	if elapsed := now.Sub(b.last); elapsed > 0 {
		// Multiply before dividing so whole-second waits refill exact tokens.
		b.tokens = math.Min(b.perMinute, b.tokens+elapsed.Seconds()*b.perMinute/60)
		b.last = now
	}
	if b.tokens >= 1-tokenEpsilon {
		b.tokens = math.Max(b.tokens-1, 0)
		return 0, true
	}
	secs := math.Ceil((1-b.tokens)*60/b.perMinute - tokenEpsilon)
	if secs < 1 {
		secs = 1
	}
	return time.Duration(secs) * time.Second, false
}
