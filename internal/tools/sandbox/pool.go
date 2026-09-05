package sandbox

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/ethanhinson/fuse/internal/loopauth"
)

// Errors returned by a Pool. They are sentinels so callers branch on identity
// rather than on message text.
var (
	// ErrPoolClosed is returned by Acquire after Close. A closed pool never
	// hands out another Runner; it does not silently re-open.
	ErrPoolClosed = errors.New("sandbox: pool is closed")

	// ErrRunnerReleased is returned by Exec on a Runner that has already been
	// handed back. Using a spent checkout is a use-after-free in disguise: the
	// underlying execution context may already belong to a different principal.
	ErrRunnerReleased = errors.New("sandbox: runner was already released")

	// ErrPrincipalMismatch is returned when a Runner is released under a
	// principal other than the one it was acquired for. The Runner is torn down
	// rather than re-pooled, because the caller has demonstrated it does not
	// know whose context it is holding.
	ErrPrincipalMismatch = errors.New("sandbox: runner released under a different principal")

	// ErrForeignRunner is returned when a Runner that this pool did not hand
	// out is offered back to it.
	ErrForeignRunner = errors.New("sandbox: runner was not acquired from this pool")
)

// ReleaseCause is the bounded reason an execution context stopped being held.
// It is a closed enum so it is safe as an event or metric label.
type ReleaseCause string

const (
	// CauseReleased is the ordinary path: the caller finished with the Runner
	// and handed it back, and it stays warm.
	CauseReleased ReleaseCause = "released"

	// CauseLoopEnd is teardown driven by the loop (or the pool) ending —
	// including Pool.Close, which tears down even Runners still checked out.
	CauseLoopEnd ReleaseCause = "loop_end"

	// CauseEarlyReturn is a release from an error path that returned before the
	// work completed. It is distinguished from CauseReleased so a leak on an
	// early return is visible in the projection rather than indistinguishable
	// from normal traffic.
	CauseEarlyReturn ReleaseCause = "early_return"

	// CauseIdleTTL is the reaper: a Runner sat idle past the configured TTL and
	// was torn down so a crashed or abandoned loop cannot leak a container.
	CauseIdleTTL ReleaseCause = "idle_ttl"

	// CauseStaleCheckout is a warm Runner the pool could NOT certify for reuse
	// and therefore discarded instead of handing out. Two situations produce
	// it, and both are security-relevant rather than merely operational:
	//
	//  1. The re-asserted principal did not match the key it was filed under.
	//  2. The Runner's environment could not be re-applied, so it would have
	//     been handed out carrying whatever it was first acquired with.
	//
	// In both cases the pool falls back to a fresh Acquire. This cause firing
	// at all means an invariant was violated somewhere; it should be alerted
	// on, not tuned.
	CauseStaleCheckout ReleaseCause = "stale_checkout"
)

// AcquireInfo describes one checkout for the emission seam.
//
// Principal is carried so the emitter can build the event's correlation
// ENVELOPE (tenant + loop); it is deliberately not part of any payload, which
// stays payload-free: bounded enums and bounded ids only, never the command,
// never environment values, never output.
type AcquireInfo struct {
	Principal   loopauth.Principal
	Handler     string        // bounded: "container" | "host"
	Runtime     string        // bounded: "docker" | "nerdctl" | "podman" | ""
	ContainerID string        // bounded id; empty when the substrate has none
	Reused      bool          // true when a warm Runner was re-certified and reused
	ColdStart   time.Duration // zero on reuse; substrate start time otherwise
}

// ReleaseInfo describes one Runner ceasing to be held, whether the holder
// handed it back or the pool took it away.
type ReleaseInfo struct {
	Principal   loopauth.Principal
	Handler     string
	Runtime     string
	ContainerID string
	Cause       ReleaseCause
}

// PoolHooks is the internal observer seam the pool emits through.
//
// It exists so this package stays a LEAF with respect to emission: the pool
// reports what happened in bounded terms and the composition root decides that
// these become events (change #0063 T8/T9). Nothing here reaches into
// internal/event or internal/observe, and the shape is deliberately additive so
// wiring real events later does not reshape the Pool.
//
// Hooks are invoked WITHOUT the pool's lock held, so an implementation may call
// back into the pool without deadlocking. They must not block indefinitely: a
// hook that hangs stalls the reaper goroutine.
type PoolHooks struct {
	// Acquired fires after a Runner has been handed to a caller.
	Acquired func(AcquireInfo)
	// Released fires when the holder hands a Runner back.
	Released func(ReleaseInfo)
	// Reaped fires when the POOL takes a Runner away — idle TTL, Close, or a
	// failed reuse certification.
	Reaped func(ReleaseInfo)
}

// PoolSource is what a Pool acquires execution contexts from. *Service is the
// only implementation, and the unexported method makes that structural: nothing
// outside this package can substitute its own selection logic for the Service's
// (which is the thing that decides containment).
type PoolSource interface {
	// Acquire obtains a fresh execution context for p, with the environment
	// resolved at the moment of the call.
	Acquire(ctx context.Context, p loopauth.Principal) (Runner, error)
	// HandlerName is the bounded substrate identifier.
	HandlerName() string
	// Runtime is the bounded container CLI name, or "".
	Runtime() string
	// IdleTTL is the reaper backstop from the resolved config.
	IdleTTL() time.Duration
	// resolveEnv re-resolves the complete environment a command may observe.
	// Unexported: sealing the interface here is what guarantees the environment
	// the pool re-applies came from the trusted, load-once config.
	resolveEnv() Env
	// gateFor is the PROCESS-SCOPED admission gate (change 0077). Unexported so
	// only *Service can supply it: a per-loop Pool must admit through the one
	// host-wide gate, not a gate it could construct for itself (which would bound
	// nothing). Never nil.
	gateFor() *Gate
	// healthHooks is the substrate-health observer the composition root
	// installed (change 0065, task 7). Unexported for the same reason gateFor
	// is: only *Service may supply it, so a Pool cannot install a health
	// observer of its own devising.
	healthHooks() HealthHooks
}

// EnvResetter is the optional reset-on-checkout seam.
//
// A pooled Runner outlives the environment it was first acquired with: a
// credential rotates, a passthrough variable changes, an operator edits the
// allowlist. Re-applying the freshly resolved Env on every checkout is what
// stops a warm Runner from becoming a snapshot of a world that no longer
// exists.
//
// A Runner that does NOT implement this cannot be certified for reuse, and the
// pool discards it rather than handing it out with a stale environment. That
// direction is deliberate: the failure mode of the safe choice is a cold start,
// and the failure mode of the other one is a command observing a credential it
// should no longer see.
type EnvResetter interface {
	ResetEnv(Env) error
}

// principalScoped is implemented by Runners that can report the principal they
// were acquired for. It is defence in depth: the pool already re-asserts its
// own recorded principal against the requested key on every hit, and this lets
// it additionally ask the RUNNER whether it agrees. A Runner that cannot answer
// is not penalised — the recorded re-assertion still ran.
type principalScoped interface {
	acquiredFor() loopauth.Principal
}

// mountScoped is implemented by Runners that can report the HOST directory
// their containers bind-mount.
//
// It is the mount half of the same defence principalScoped provides for
// identity (change 0065). The pool cannot re-derive a tenant's root for itself
// — the layout is the composition root's, and the resolution happened at
// Acquire with the authenticated Principal in hand — so what it CAN do, and
// does, is pin the root a warm entry was cold-started with and refuse any hit
// whose Runner no longer agrees with it.
//
// That is narrower than re-resolving, and deliberately so: it turns "this warm
// container's mount silently changed" from an invisible cross-tenant disclosure
// into a discarded entry and a cold start. A Runner that cannot answer is not
// penalised (there is no mount to re-assert), matching principalScoped.
type mountScoped interface {
	mountRoot() string
}

// containerIdentified is implemented by Runners backed by a durable container.
// Nothing implements it today (the container handler uses `run --rm`, so no
// container outlives an Exec); it is the seam the bounded container id will
// arrive through when a persistent-container substrate lands.
type containerIdentified interface {
	ContainerID() string
}

// poolEntry is one warm execution context and everything the pool knows about
// it. The principal is recorded on the ENTRY, independently of the map key it
// is filed under, precisely so the two can be compared.
type poolEntry struct {
	principal   loopauth.Principal
	runner      Runner
	handler     string
	runtime     string
	containerID string

	// mountRoot is the HOST directory this entry's Runner was cold-started
	// against, recorded on the ENTRY independently of the Runner's own copy —
	// for exactly the reason principal is recorded here independently of the
	// map key it is filed under: two records that can be compared are what make
	// a re-assertion possible at all (change 0065).
	//
	// "" means the Runner does not report a mount (the host handler, and every
	// non-container substrate), in which case there is nothing to re-assert and
	// certifyEntry says so.
	mountRoot string

	// pooled is false for a TRANSIENT entry — one created because the
	// principal's warm slot was already checked out. A transient is torn down
	// on release instead of being kept warm, so a principal never accumulates
	// more than one idle Runner.
	pooled bool
	// busy is true while the entry is checked out. A busy entry is never
	// reaped: the reaper's job is leaked idle substrate, not live work.
	busy bool
	// lastUsed is when the entry was last handed back. Meaningful only while
	// !busy.
	lastUsed time.Time
	// torn records that some caller has claimed this entry's teardown. Two
	// callers can reach the same entry — Close sweeping a checked-out entry
	// while its holder is releasing it — and tearing down twice would double
	// every teardown event and double-Release the substrate.
	torn bool
}

// claimTeardown marks e as being torn down and reports whether THIS caller is
// the one that must do it. Callers must hold the pool lock.
func (e *poolEntry) claimTeardown() bool {
	if e.torn {
		return false
	}
	e.torn = true
	return true
}

// Pool hands out warm execution contexts, at most one idle Runner per
// principal.
//
// # Why this is keyed by principal and never by availability
//
// The obvious warm-pool shape — "hand out any available Runner" — is a
// containment breach here. A Runner is a live shell: it carries a resolved
// environment, a mount, and whatever state a previous command left behind. Two
// principals sharing one is a cross-tenant leak in the most literal sense, and
// it is invisible in testing because the pool behaves perfectly right up until
// two loops overlap.
//
// So there is no "available" set. There is a map from principal to that
// principal's own Runner, and every checkout RE-ASSERTS the key on the hit
// before returning it (learning: cache-over-tenant-scoped-source-reassert-key-on-hit).
// A cache keyed by a tenant-scoped value must re-verify the key it matched,
// because the map is the very thing a keying bug corrupts — a lookup that
// "found something" is not evidence it found the right thing. On any mismatch
// the pool discards the entry and falls through to a fresh Acquire; there is no
// branch on which another principal's Runner is returned.
//
// A Pool is safe for concurrent use.
type Pool struct {
	src   PoolSource
	hooks PoolHooks

	// gate is the process-scoped admission gate, captured from src at
	// construction (change 0077). Every Exec on a checkout from this pool passes
	// through it, so the host-wide concurrency ceiling holds across every loop
	// even though the Pool itself is per-loop.
	gate *Gate

	idleTTL  time.Duration
	interval time.Duration
	now      func() time.Time

	mu      sync.Mutex
	entries map[loopauth.Principal]*poolEntry
	live    map[*poolEntry]struct{}
	closed  bool

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// PoolOption configures a Pool at construction.
type PoolOption func(*Pool)

// WithPoolIdleTTL overrides the reaper's idle backstop. A non-positive value is
// ignored: zero would mean "reap on release", tearing down every warm Runner
// the instant it is handed back.
func WithPoolIdleTTL(d time.Duration) PoolOption {
	return func(p *Pool) {
		if d > 0 {
			p.idleTTL = d
		}
	}
}

// WithPoolHooks installs the emission observer. Nil hook funcs are fine and are
// simply not called.
func WithPoolHooks(h PoolHooks) PoolOption {
	return func(p *Pool) { p.hooks = h }
}

// withPoolClock injects the clock (tests).
func withPoolClock(now func() time.Time) PoolOption {
	return func(p *Pool) {
		if now != nil {
			p.now = now
		}
	}
}

// withPoolReapInterval injects how often the reaper wakes (tests).
func withPoolReapInterval(d time.Duration) PoolOption {
	return func(p *Pool) {
		if d > 0 {
			p.interval = d
		}
	}
}

// reaper interval bounds. The reaper is a backstop against leaked substrate,
// not a scheduler, so it wakes rarely: a Runner living a fraction of the TTL
// past its deadline costs nothing, while a tight loop over every pool costs
// wakeups forever.
const (
	minReapInterval = time.Second
	maxReapInterval = time.Minute
)

// NewPool returns a Pool over src and starts its reaper goroutine.
//
// The returned Pool MUST be Closed, or the reaper goroutine and every warm
// Runner leak. Close is idempotent, so releasing it from a teardown hook that
// may run twice is safe.
func NewPool(src PoolSource, opts ...PoolOption) *Pool {
	p := &Pool{
		src:     src,
		gate:    src.gateFor(),
		now:     time.Now,
		entries: make(map[loopauth.Principal]*poolEntry),
		live:    make(map[*poolEntry]struct{}),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}

	p.idleTTL = src.IdleTTL()
	if p.idleTTL <= 0 {
		p.idleTTL = DefaultIdleTTL
	}
	for _, opt := range opts {
		if opt != nil {
			opt(p)
		}
	}
	if p.interval <= 0 {
		p.interval = clampDuration(p.idleTTL/4, minReapInterval, maxReapInterval)
	}

	go p.reapLoop()
	return p
}

func clampDuration(d, lo, hi time.Duration) time.Duration {
	switch {
	case d < lo:
		return lo
	case d > hi:
		return hi
	default:
		return d
	}
}

// Acquire returns an execution context for principal.
//
// It reuses that principal's warm Runner when — and only when — the entry can
// be CERTIFIED for reuse: the recorded principal must re-assert against the
// requested key, the Runner itself must agree when it can answer, and the
// freshly resolved environment must be successfully re-applied. Anything less
// discards the entry and cold-starts a new one. The environment is re-resolved
// on every call, never read from what the Runner was first acquired with.
func (p *Pool) Acquire(ctx context.Context, principal loopauth.Principal) (Runner, error) {
	env := p.src.resolveEnv()

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrPoolClosed
	}

	var (
		stale *poolEntry
		warm  *poolEntry
	)
	if e, ok := p.entries[principal]; ok && !e.busy {
		switch {
		case !certifyEntry(e, principal):
			// The map handed back an entry belonging to someone else, or one
			// whose mount no longer matches what it was certified with. Never
			// return it; drop it and pay for a cold start. A mount mismatch
			// takes this SAME discard path — claimTeardown, dropLocked,
			// CauseStaleCheckout — deliberately: it is the same class of fault
			// as an identity mismatch, and giving it a quieter path would make
			// the one failure an operator most needs to see the one they see
			// least.
			if e.claimTeardown() {
				stale = e
			}
			p.dropLocked(e)

		default:
			// Re-apply the CURRENT environment. Done under the pool lock so the
			// write is ordered against the previous holder's Release and the
			// next holder's first Exec.
			if err := resetRunnerEnv(e.runner, env); err != nil {
				if e.claimTeardown() {
					stale = e
				}
				p.dropLocked(e)
			} else {
				e.busy = true
				warm = e
			}
		}
	}
	p.mu.Unlock()

	if stale != nil {
		// The teardown error is deliberately dropped: we are already on the
		// path where this Runner is not being reused, and failing to tear down
		// a substrate we have stopped tracking must not deny the caller a fresh
		// one. The reap hook records that it happened.
		_ = p.teardown(ctx, stale, CauseStaleCheckout, true)
	}
	if warm != nil {
		p.fireAcquired(AcquireInfo{
			Principal:   warm.principal,
			Handler:     warm.handler,
			Runtime:     warm.runtime,
			ContainerID: warm.containerID,
			Reused:      true,
		})
		return &pooledRunner{pool: p, entry: warm}, nil
	}

	return p.acquireFresh(ctx, principal)
}

// acquireFresh cold-starts a Runner. The substrate Acquire happens OUTSIDE the
// pool lock: starting a container can take seconds, and holding the lock across
// it would make one principal's cold start block every other principal's warm
// checkout.
func (p *Pool) acquireFresh(ctx context.Context, principal loopauth.Principal) (Runner, error) {
	start := p.now()
	runner, err := p.src.Acquire(ctx, principal)
	if err != nil {
		// acquire_failed (change 0065, task 7). This is the one place a cold
		// start is seen whole: the substrate could not produce a Runner at all,
		// so nothing this principal asked for can run.
		//
		// A caller-deadline expiry is excluded — that is the caller's bound
		// firing, not the substrate failing — as is a pull failure, which the
		// handler already reported as pull_failed at its own site and which
		// would otherwise be double-counted under two reasons for one incident.
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, errPullFailed) {
			hooks := p.src.healthHooks()
			hooks.fire(HealthInfo{
				Principal: principal,
				Handler:   p.src.HandlerName(),
				Runtime:   p.src.Runtime(),
				Reason:    HealthAcquireFailed,
			})
		}
		// Propagate untouched: a refusal from selection (ErrRefusedUncontained)
		// must reach the caller as a refusal, never as a pool-flavoured error
		// that invites a fallback.
		return nil, err
	}
	coldStart := p.now().Sub(start)

	e := &poolEntry{
		principal:   principal,
		runner:      runner,
		handler:     p.src.HandlerName(),
		runtime:     p.src.Runtime(),
		containerID: runnerContainerID(runner),
		// Pinned HERE, at the cold start, because this is the one moment the
		// entry's mount is known to be right: the substrate has just resolved
		// it from this principal's authenticated identity. Every later hit is
		// measured against this value (see certifyEntry).
		mountRoot: runnerMountRoot(runner),
		busy:      true,
	}

	p.mu.Lock()
	if p.closed {
		// Closed while we were starting. Do not register it, or Close has
		// already run its teardown sweep and this one leaks.
		p.mu.Unlock()
		_ = runner.Release(ctx)
		return nil, ErrPoolClosed
	}
	if _, occupied := p.entries[principal]; !occupied {
		// This principal has no warm slot: take it. Otherwise stay transient,
		// so a principal never accumulates more than one idle Runner.
		p.entries[principal] = e
		e.pooled = true
	}
	p.live[e] = struct{}{}
	p.mu.Unlock()

	p.fireAcquired(AcquireInfo{
		Principal:   e.principal,
		Handler:     e.handler,
		Runtime:     e.runtime,
		ContainerID: e.containerID,
		Reused:      false,
		ColdStart:   coldStart,
	})
	return &pooledRunner{pool: p, entry: e}, nil
}

// Release hands a Runner back, keeping it warm for the same principal.
//
// cause is the caller's account of WHY, and it is required rather than inferred
// so that an early-return release is distinguishable from a completed one in
// the projection. Release is idempotent: a second call for the same Runner is a
// no-op, which is what lets a caller release on an error path and again in a
// defer.
//
// Releasing under a principal other than the one the Runner was acquired for
// returns ErrPrincipalMismatch AND tears the Runner down. It is not re-pooled
// under either key: a caller confused about whose context it holds is exactly
// the situation in which keeping the context alive is dangerous.
func (p *Pool) Release(ctx context.Context, principal loopauth.Principal, r Runner, cause ReleaseCause) error {
	pr, ok := r.(*pooledRunner)
	if !ok || pr.pool != p {
		return ErrForeignRunner
	}
	return pr.releaseTo(ctx, principal, cause, true)
}

// Close tears down every Runner — idle or still checked out — and stops the
// reaper goroutine, waiting for it to exit so the caller can rely on no
// lingering goroutine after Close returns.
//
// A Runner still checked out at Close is the abandoned-loop shape: nobody is
// coming back for it, so it is torn down rather than waited on. Close is
// idempotent and never blocks on a second call.
func (p *Pool) Close(ctx context.Context) error {
	p.stopOnce.Do(func() { close(p.stop) })
	<-p.done

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	victims := make([]*poolEntry, 0, len(p.live))
	for e := range p.live {
		if e.claimTeardown() {
			victims = append(victims, e)
		}
	}
	p.entries = make(map[loopauth.Principal]*poolEntry)
	p.live = make(map[*poolEntry]struct{})
	p.mu.Unlock()

	var firstErr error
	for _, e := range victims {
		if err := p.teardown(ctx, e, CauseLoopEnd, true); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// reapLoop is the idle-TTL backstop.
//
// Without it a loop that crashes between Acquire and Release leaves its
// substrate alive forever, and the leak is silent — the pool looks idle, the
// container is not. The goroutine is stopped by Close and signals its exit on
// done, so "the reaper is gone" is observable rather than assumed.
func (p *Pool) reapLoop() {
	defer close(p.done)

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			p.reapIdle(p.now())
		}
	}
}

// reapIdle tears down every pooled entry idle past the TTL as of now. Only idle
// entries are eligible: a checked-out Runner is live work, however long it has
// been running.
func (p *Pool) reapIdle(now time.Time) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	var victims []*poolEntry
	for _, e := range p.entries {
		if e.busy {
			continue
		}
		if now.Sub(e.lastUsed) >= p.idleTTL && e.claimTeardown() {
			victims = append(victims, e)
			p.dropLocked(e)
		}
	}
	p.mu.Unlock()

	// Teardown and hooks run outside the lock: both can block, and a hook is
	// caller code that may re-enter the pool.
	ctx := context.Background()
	for _, e := range victims {
		_ = p.teardown(ctx, e, CauseIdleTTL, true)
	}
}

// dropLocked removes e from the pool's bookkeeping. It never touches the
// Runner; teardown does that, outside the lock.
func (p *Pool) dropLocked(e *poolEntry) {
	if cur, ok := p.entries[e.principal]; ok && cur == e {
		delete(p.entries, e.principal)
	}
	delete(p.live, e)
}

// teardown releases the underlying substrate context and fires the appropriate
// hook. reap distinguishes "the pool took it away" from "the holder gave it
// back", which is the distinction T8's event kinds draw.
func (p *Pool) teardown(ctx context.Context, e *poolEntry, cause ReleaseCause, reap bool) error {
	err := e.runner.Release(ctx)
	info := ReleaseInfo{
		Principal:   e.principal,
		Handler:     e.handler,
		Runtime:     e.runtime,
		ContainerID: e.containerID,
		Cause:       cause,
	}
	if reap {
		p.fireReaped(info)
	} else {
		p.fireReleased(info)
	}
	return err
}

func (p *Pool) fireAcquired(i AcquireInfo) {
	if p.hooks.Acquired != nil {
		p.hooks.Acquired(i)
	}
}

func (p *Pool) fireReleased(i ReleaseInfo) {
	if p.hooks.Released != nil {
		p.hooks.Released(i)
	}
}

func (p *Pool) fireReaped(i ReleaseInfo) {
	if p.hooks.Reaped != nil {
		p.hooks.Reaped(i)
	}
}

// certifyEntry re-asserts, on a cache HIT, that the entry really belongs to the
// principal asking for it AND still mounts the host tree it was cold-started
// against.
//
// This is the whole point of the learning
// cache-over-tenant-scoped-source-reassert-key-on-hit: the map lookup already
// "succeeded", and trusting that is how one principal ends up inside another's
// shell. Two independent records are compared for each property, so no single
// corrupted one can certify itself.
//
// # Why the MOUNT is asserted separately from the principal
//
// Today the mount follows from the principal for free: containerRunner.root is
// derived from the authenticated Principal at Acquire and never reassigned
// (change 0065), so an entry certified for its principal structurally carries
// that principal's tenant root. That is an argument about the CURRENT shape of
// the code, and the thing an operator's isolation actually depends on is the
// host directory in argv's `-v`, not the identity it happens to be derived
// from. Asserting the derived value as well as its source is what makes a
// future refactor that decouples the two fail loudly HERE — one discarded entry
// and one cold start — instead of silently handing a warm container another
// tenant's files.
//
// It is a PIN, not a re-resolution: the pool cannot re-derive a tenant's root
// (the layout belongs to the composition root, and the resolution needs the
// authenticated Principal that Acquire had in hand). Pinning what was resolved
// at the cold start and refusing anything that no longer matches is the
// strongest statement this layer can make on its own, and it is exactly the
// statement that catches drift.
func certifyEntry(e *poolEntry, principal loopauth.Principal) bool {
	if e.principal != principal {
		return false
	}
	if ps, ok := e.runner.(principalScoped); ok {
		if ps.acquiredFor() != principal {
			return false
		}
	}
	// A Runner that reports no mount has nothing to re-assert (the host
	// handler, and every non-container substrate); it is not penalised, exactly
	// as a Runner that cannot report its principal is not.
	if ms, ok := e.runner.(mountScoped); ok {
		if ms.mountRoot() != e.mountRoot {
			return false
		}
	}
	return true
}

// resetRunnerEnv re-applies env to a warm Runner. A Runner that cannot be reset
// is reported as uncertifiable so the caller discards it; see EnvResetter.
func resetRunnerEnv(r Runner, env Env) error {
	re, ok := r.(EnvResetter)
	if !ok {
		return errNoEnvReset
	}
	return re.ResetEnv(env)
}

var errNoEnvReset = errors.New("sandbox: runner cannot re-apply its environment")

// runnerContainerID reports a Runner's bounded container id, or "".
func runnerContainerID(r Runner) string {
	if ci, ok := r.(containerIdentified); ok {
		return ci.ContainerID()
	}
	return ""
}

// runnerMountRoot reports a Runner's host bind-mount root, or "" when the
// Runner does not have one to report. See mountScoped.
func runnerMountRoot(r Runner) string {
	if ms, ok := r.(mountScoped); ok {
		return ms.mountRoot()
	}
	return ""
}

// Unwrap returns the substrate Runner underneath a pooled checkout, or r itself
// when it did not come from a pool. It exists for tests and for diagnostics;
// production code has no reason to reach past the wrapper, and reaching past it
// forfeits the use-after-release guard.
func Unwrap(r Runner) Runner {
	if pr, ok := r.(*pooledRunner); ok {
		return pr.entry.runner
	}
	return r
}

// pooledRunner is one checkout: a handle to an entry that is valid until it is
// released.
//
// The wrapper is not ceremony. It is what makes a checkout SPENT on release: a
// caller holding the raw substrate Runner could keep executing after handing it
// back, driving an execution context the pool may have already re-certified and
// handed to the next holder. The wrapper turns that into an error instead of a
// silent cross-checkout write.
type pooledRunner struct {
	pool  *Pool
	entry *poolEntry

	mu       sync.Mutex
	released bool
}

// Exec runs cmd in the checked-out context, refusing once released.
//
// It is the ONE place the admission gate is acquired (change 0077): a ticket is
// taken around the substrate Exec and released on EVERY return path, so a ticket
// never outlives one Exec. Gating here — around the actual container process,
// which the container handler spawns per Exec — counts real concurrent
// executions exactly, with no over- or under-count. Gating pool CHECKOUT instead
// would be both wrong (an idle warm Runner would hold a slot) and deadlock-prone
// (a loop holding a checkout while waiting for a slot would wait on a resource it
// holds).
func (r *pooledRunner) Exec(ctx context.Context, cmd string, workingDir string) (Output, error) {
	r.mu.Lock()
	released := r.released
	r.mu.Unlock()
	if released {
		// ExitCode -1, never 0: a caller that only reads ExitCode still fails
		// closed, matching the substrate contract.
		return Output{ExitCode: -1}, ErrRunnerReleased
	}

	ticket, waited, err := r.pool.gate.Admit(ctx, r.entry.principal.Tenant)
	if err != nil {
		// A capacity refusal (ErrSandboxAtCapacity) or a caller-deadline error.
		// Either way the command did not run: ExitCode -1 so an ExitCode-only
		// caller fails closed, and the error is propagated for bash.go to render.
		return Output{ExitCode: -1}, err
	}
	// Released on every path, including a panic in the substrate Exec.
	defer ticket.Release()

	out, execErr := r.entry.runner.Exec(ctx, cmd, workingDir)
	// The wait is a scheduling fact, recorded even on an error/timeout result so
	// the caller can still surface backpressure.
	out.Waited = waited
	return out, execErr
}

// Release hands this checkout back to the pool under the principal it was
// acquired for, with the ordinary cause. It satisfies the Runner contract, so
// callers holding only a Runner can release on every early-return path without
// knowing a pool exists. Use Pool.Release to record a more specific cause.
func (r *pooledRunner) Release(ctx context.Context) error {
	return r.releaseTo(ctx, r.entry.principal, CauseReleased, false)
}

// releaseTo is the single release path. checkPrincipal is false only for the
// self-release above, which sources the principal from the entry itself.
func (r *pooledRunner) releaseTo(ctx context.Context, principal loopauth.Principal, cause ReleaseCause, checkPrincipal bool) error {
	r.mu.Lock()
	if r.released {
		r.mu.Unlock()
		return nil
	}
	r.released = true
	r.mu.Unlock()

	e := r.entry
	mismatch := checkPrincipal && principal != e.principal
	if mismatch {
		cause = CauseStaleCheckout
	}

	p := r.pool
	p.mu.Lock()
	e.busy = false
	// Keep it warm only when everything is ordinary: the pool is open, this is
	// the principal's own warm slot, and the caller knew whose context it held.
	keep := !p.closed && e.pooled && !mismatch && !e.torn
	tearDown := false
	if keep {
		e.lastUsed = p.now()
	} else {
		// Close may already have claimed this entry while we were unwinding; in
		// that case it is already torn down and this release is bookkeeping
		// only, not a second teardown.
		tearDown = e.claimTeardown()
		p.dropLocked(e)
	}
	p.mu.Unlock()

	if !keep {
		if !tearDown {
			if mismatch {
				return ErrPrincipalMismatch
			}
			return nil
		}
		// A discarded checkout is a teardown, and when the reason is a mismatch
		// the pool took it away rather than merely accepting it back.
		err := p.teardown(ctx, e, cause, mismatch)
		if mismatch {
			return ErrPrincipalMismatch
		}
		return err
	}

	p.fireReleased(ReleaseInfo{
		Principal:   e.principal,
		Handler:     e.handler,
		Runtime:     e.runtime,
		ContainerID: e.containerID,
		Cause:       cause,
	})
	return nil
}
