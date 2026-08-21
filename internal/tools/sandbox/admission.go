package sandbox

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
)

// ErrSandboxAtCapacity is the refusal returned by Gate.Admit when the host is
// genuinely saturated: max_inflight Execs are running AND max_queued more are
// already parked behind them. It is a SENTINEL so a caller branches on identity,
// and it is deliberately distinct from a context-deadline error — a refusal
// means "the host is full", a deadline means "this caller ran out of time", and
// the two must stay separable (one is a capacity signal, the other is not).
var ErrSandboxAtCapacity = errors.New("sandbox: no execution slot available (at capacity)")

// AdmissionScope reports WHICH bound a caller waited on or was refused by. It is
// a closed enum, safe as a metric label.
type AdmissionScope string

const (
	// ScopeGlobal is the host-wide runaway backstop (max_inflight).
	ScopeGlobal AdmissionScope = "global"
	// ScopeTenant is one tenant's soft share (max_inflight_per_tenant).
	ScopeTenant AdmissionScope = "tenant"
)

// AdmissionInfo describes one notable admission decision for the emission seam.
// Like AcquireInfo it carries the tenant so the emitter can build the event's
// correlation envelope; nothing here becomes an unbounded payload field.
type AdmissionInfo struct {
	Tenant event.TenantID
	// Handler is the bounded substrate id that was full ("container"|"host"|
	// "microvm"), stamped by the gate from the Service's selection.
	Handler string
	// Scope is which bound the caller contended on. For a Queued info it is the
	// bound the caller waited longest on; for a Refused info it is the bound that
	// was full when the waiter overflow tripped.
	Scope AdmissionScope
	// Waited is how long the caller spent queued. Zero on a refusal (a refusal is
	// immediate — the caller never entered the queue).
	Waited time.Duration
}

// GateHooks is the gate's observer seam, shaped like PoolHooks: the gate reports
// notable admissions in bounded terms and something above it decides those
// become events. Nil funcs are fine. Only NOTABLE admissions fire a hook — a
// wait past the note threshold (Queued) or a capacity refusal (Refused). The
// uncontended fast path fires nothing: one event per Exec would double the
// sandbox stream's volume to record that nothing happened.
type GateHooks struct {
	// Queued fires after a caller was admitted having waited past the note
	// threshold.
	Queued func(AdmissionInfo)
	// Refused fires when a caller was refused because the waiter bound was full.
	Refused func(AdmissionInfo)
}

// Ticket is one admitted execution. Release returns the slot to the gate and is
// idempotent — a caller may Release on every return path, including the panic
// and deadline paths, without double-releasing.
type Ticket interface {
	Release()
}

// Gate is the sandbox-layer admission control on CONCURRENT in-flight Execs.
//
// It is substrate-agnostic: it counts executions, not containers, so it covers
// the container handler and a future microVM handler unchanged. It enforces two
// bounds — a global runaway backstop and a per-tenant soft share — plus a waiter
// overflow that is the only thing that ever REFUSES. Waiting is the normal
// outcome; refusal is reserved for the genuinely pathological case.
//
// A Gate is safe for concurrent use.
type Gate struct {
	global      *semaphore
	perTenant   int64
	maxQueued   int64
	noteThresh  time.Duration
	handlerName string // bounded handler id for emitted infos; set by the Service

	hooks GateHooks

	mu      sync.Mutex
	tenants map[event.TenantID]*tenantGate
	waiters int64 // total callers currently parked across all tenants
}

// tenantGate is one tenant's share plus a refcount so idle tenants can be reaped
// and the map does not grow without bound.
type tenantGate struct {
	sem *semaphore
	// refs is how many callers currently hold or are waiting on this tenant's
	// share. When it returns to zero at full capacity the entry is removed.
	refs int64
}

// newGate builds a Gate from resolved Concurrency config. Every field is assumed
// resolved (NewService fills defaults before this runs); a non-positive value is
// clamped to its default as a last-resort guard so a hand-built Config can never
// produce a zero backstop that refuses everything.
func newGate(c Concurrency) *Gate {
	maxInflight := valueOr(c.MaxInflight, defaultMaxInflight)
	perTenant := valueOr(c.MaxInflightPerTenant, defaultMaxInflightPerTenant)
	maxQueued := valueOr(c.MaxQueued, defaultMaxQueued)
	noteThresh := durationOr(c.NoteThreshold, DefaultNoteThreshold)

	// A per-tenant share larger than the global bound is meaningless (the global
	// bound would always bind first); clamp it so the two-semaphore accounting
	// stays honest.
	if perTenant > maxInflight {
		perTenant = maxInflight
	}

	return &Gate{
		global:     newSemaphore(maxInflight),
		perTenant:  perTenant,
		maxQueued:  maxQueued,
		noteThresh: noteThresh,
		tenants:    make(map[event.TenantID]*tenantGate),
	}
}

// withHandlerName stamps the bounded handler id onto emitted AdmissionInfos. It
// is set once by the Service after selection, so a refused/queued event names
// the substrate that was full.
func (g *Gate) withHandlerName(name string) *Gate {
	g.handlerName = name
	return g
}

// setHooks installs the emission observer. Called once at composition, before
// the gate is used concurrently.
func (g *Gate) setHooks(h GateHooks) { g.hooks = h }

// Admit blocks until a slot is free, ctx expires, or the waiter bound is
// exceeded. It reports how long the caller waited, so the layer above can decide
// whether that is worth telling the model about.
//
// Acquisition order is PER-TENANT slot first, then the GLOBAL slot — a fixed,
// global order, so there is no cycle and no deadlock. The direction matters: a
// caller blocked on the global bound holds only its own tenant's slot, so the
// blocking is self-inflicted. The reverse order would let a saturated tenant's
// waiters sit on global slots and stall every other tenant — the exact
// cross-tenant starvation the per-tenant share exists to prevent.
func (g *Gate) Admit(ctx context.Context, tenant event.TenantID) (Ticket, time.Duration, error) {
	tenant = event.NormalizeTenant(tenant)
	start := time.Now()

	// Reserve a waiter slot up front. If the queue is already full this is the
	// pathological case and we refuse IMMEDIATELY — the caller never enters a
	// semaphore wait.
	if !g.enterQueue() {
		g.fireRefused(AdmissionInfo{Tenant: tenant, Scope: ScopeGlobal})
		return nil, 0, ErrSandboxAtCapacity
	}
	// From here every return path must leaveQueue exactly once.

	tg := g.acquireTenantGate(tenant)

	// Per-tenant slot first.
	if err := tg.sem.acquire(ctx); err != nil {
		g.releaseTenantGate(tenant, tg)
		g.leaveQueue()
		return nil, time.Since(start), err
	}

	// Then the global slot. On failure release the tenant slot we already hold.
	if err := g.global.acquire(ctx); err != nil {
		tg.sem.release()
		g.releaseTenantGate(tenant, tg)
		g.leaveQueue()
		return nil, time.Since(start), err
	}

	waited := time.Since(start)
	g.leaveQueue()

	// A wait past the threshold is worth an operator's attention. The scope is
	// whichever bound the caller actually blocked on — reported as tenant when
	// the tenant share was contended, else global. We approximate with a probe:
	// if the tenant share is now saturated the tenant bound is the live pressure.
	if waited >= g.noteThresh {
		g.fireQueued(AdmissionInfo{Tenant: tenant, Scope: g.contendedScope(tg), Waited: waited})
	}

	return &gateTicket{gate: g, tenant: tenant, tg: tg}, waited, nil
}

// contendedScope reports which bound is the live pressure right now: tenant if
// this tenant's share is saturated, else global. It is a best-effort label for
// the emitted event, not a correctness-bearing decision.
func (g *Gate) contendedScope(tg *tenantGate) AdmissionScope {
	if tg.sem.full() {
		return ScopeTenant
	}
	return ScopeGlobal
}

// enterQueue reserves a waiter slot, refusing when the queue is already full.
func (g *Gate) enterQueue() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.waiters >= g.maxQueued {
		return false
	}
	g.waiters++
	return true
}

// leaveQueue releases a waiter slot. Called exactly once per successful
// enterQueue, whatever the outcome of the semaphore waits.
func (g *Gate) leaveQueue() {
	g.mu.Lock()
	if g.waiters > 0 {
		g.waiters--
	}
	g.mu.Unlock()
}

// acquireTenantGate returns the tenant's gate, creating it lazily, and bumps its
// refcount so a concurrent reaper cannot remove it out from under this caller.
func (g *Gate) acquireTenantGate(tenant event.TenantID) *tenantGate {
	g.mu.Lock()
	defer g.mu.Unlock()
	tg, ok := g.tenants[tenant]
	if !ok {
		tg = &tenantGate{sem: newSemaphore(g.perTenant)}
		g.tenants[tenant] = tg
	}
	tg.refs++
	return tg
}

// releaseTenantGate drops a refcount and reaps the entry when it is idle at full
// capacity, so the tenant map does not grow without bound. It is called on every
// path that acquired a gate but is NOT holding a live ticket (the acquire-failure
// paths); the ticket's own Release calls it for the happy path.
func (g *Gate) releaseTenantGate(tenant event.TenantID, tg *tenantGate) {
	g.mu.Lock()
	tg.refs--
	if tg.refs <= 0 {
		// Only remove if nobody else re-created/replaced it in the meantime.
		if cur, ok := g.tenants[tenant]; ok && cur == tg {
			delete(g.tenants, tenant)
		}
	}
	g.mu.Unlock()
}

func (g *Gate) fireQueued(i AdmissionInfo) {
	i.Scope = orScope(i.Scope, ScopeGlobal)
	i.Handler = g.handlerName
	if g.hooks.Queued != nil {
		g.hooks.Queued(i)
	}
}

func (g *Gate) fireRefused(i AdmissionInfo) {
	i.Handler = g.handlerName
	if g.hooks.Refused != nil {
		g.hooks.Refused(i)
	}
}

func orScope(s, fallback AdmissionScope) AdmissionScope {
	if s == "" {
		return fallback
	}
	return s
}

// gateTicket is one admitted execution. Its lifetime is strictly inside one
// Exec; nothing else holds one.
type gateTicket struct {
	gate   *Gate
	tenant event.TenantID
	tg     *tenantGate

	once sync.Once
}

// Release returns the global and tenant slots, in the reverse of acquisition
// order, and drops the tenant refcount. It is idempotent via sync.Once so a
// caller may Release on every return path including a deferred release after an
// explicit one.
func (t *gateTicket) Release() {
	t.once.Do(func() {
		t.gate.global.release()
		t.tg.sem.release()
		t.gate.releaseTenantGate(t.tenant, t.tg)
	})
}

func valueOr(p *int64, def int64) int64 {
	if p == nil || *p <= 0 {
		return def
	}
	return *p
}

func durationOr(p *time.Duration, def time.Duration) time.Duration {
	if p == nil || *p <= 0 {
		return def
	}
	return *p
}

// semaphore is a counting semaphore with a context-cancellable acquire. It is a
// buffered channel: acquire sends, release receives. A channel gives fair-ish
// FIFO-ish wakeups and a clean select against ctx.Done for free.
type semaphore struct {
	tokens chan struct{}
}

func newSemaphore(n int64) *semaphore {
	if n < 1 {
		n = 1
	}
	return &semaphore{tokens: make(chan struct{}, n)}
}

// acquire blocks until a slot is free or ctx is cancelled. It returns ctx.Err()
// on cancellation — never ErrSandboxAtCapacity, so a deadline stays
// distinguishable from a refusal.
func (s *semaphore) acquire(ctx context.Context) error {
	select {
	case s.tokens <- struct{}{}:
		return nil
	default:
	}
	select {
	case s.tokens <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *semaphore) release() {
	select {
	case <-s.tokens:
	default:
		// Balanced release/acquire means this cannot happen; the default keeps a
		// buggy double-release from panicking on an empty channel.
	}
}

// full reports whether the semaphore is at capacity right now. Advisory only —
// used to label an emitted event, never to gate.
func (s *semaphore) full() bool {
	return len(s.tokens) == cap(s.tokens)
}
