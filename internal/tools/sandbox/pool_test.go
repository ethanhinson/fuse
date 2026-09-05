package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
)

// --- doubles ---------------------------------------------------------------
//
// Every double here is shared with goroutines by at least one test, so EVERY
// field crossing that boundary is mutex-guarded on BOTH the getter and the
// setter (learning: mutex-test-double-concurrent-provider). A read-only-looking
// accessor that skips the lock is exactly the shape that makes a -race run
// green for the wrong reason.

// fakeClock is an injectable clock. The reaper goroutine reads it while the
// test advances it, so both sides lock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeRunner is one substrate execution context.
type fakeRunner struct {
	mu       sync.Mutex
	owner    loopauth.Principal
	env      map[string]string
	execs    []string
	releases int
}

func (r *fakeRunner) Exec(_ context.Context, cmd string, _ string) (Output, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.execs = append(r.execs, cmd)
	return Output{Combined: []byte(cmd)}, nil
}

func (r *fakeRunner) Release(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.releases++
	return nil
}

// ResetEnv is the reset-on-checkout seam the pool re-applies through.
func (r *fakeRunner) ResetEnv(env Env) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.env = map[string]string{}
	for k, v := range env.Allow {
		r.env[k] = v
	}
	return nil
}

func (r *fakeRunner) acquiredFor() loopauth.Principal {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.owner
}

func (r *fakeRunner) setOwner(p loopauth.Principal) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.owner = p
}

func (r *fakeRunner) envSnapshot() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]string{}
	for k, v := range r.env {
		out[k] = v
	}
	return out
}

func (r *fakeRunner) releaseCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.releases
}

// fakeSource stands in for *Service. It hands out a fresh fakeRunner per
// Acquire and resolves a mutable environment, so a test can change the world
// between two checkouts.
type fakeSource struct {
	clock     *fakeClock
	coldStart time.Duration

	mu       sync.Mutex
	env      map[string]string
	acquired []*fakeRunner
	err      error
	gate     *Gate
	health   HealthHooks
}

func newFakeSource(clock *fakeClock) *fakeSource {
	return &fakeSource{
		clock:     clock,
		coldStart: 10 * time.Millisecond,
		env:       map[string]string{"PATH": "/bin"},
		// A gate with the built-in defaults: generous enough that pool tests
		// exercising warm reuse and teardown never contend on it. Tests that
		// want contention build their own Gate (admission_test.go).
		gate: newGate(Concurrency{}),
	}
}

func (s *fakeSource) gateFor() *Gate { return s.gate }

// healthHooks satisfies PoolSource's health seam. Read under the same lock the
// error is, so a test that installs hooks and then trips a failure from another
// goroutine is race-clean.
func (s *fakeSource) healthHooks() HealthHooks {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.health
}

// setHealth installs the observer a pool-level health test asserts on.
func (s *fakeSource) setHealth(h HealthHooks) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.health = h
}

// setGate swaps in a tighter gate so a composition test can force contention
// between the pool and the admission gate.
func (s *fakeSource) setGate(g *Gate) { s.gate = g }

func (s *fakeSource) Acquire(_ context.Context, p loopauth.Principal) (Runner, error) {
	s.mu.Lock()
	err := s.err
	env := map[string]string{}
	for k, v := range s.env {
		env[k] = v
	}
	s.mu.Unlock()

	if err != nil {
		return nil, err
	}
	if s.clock != nil {
		// Cold start costs measurable time on the injected clock, so the
		// reused/cold-start assertions are exact rather than timing-dependent.
		s.clock.advance(s.coldStart)
	}

	r := &fakeRunner{owner: p, env: env}
	s.mu.Lock()
	s.acquired = append(s.acquired, r)
	s.mu.Unlock()
	return r, nil
}

func (s *fakeSource) resolveEnv() Env {
	s.mu.Lock()
	defer s.mu.Unlock()
	allow := map[string]string{}
	for k, v := range s.env {
		allow[k] = v
	}
	return Env{Allow: allow}
}

func (s *fakeSource) HandlerName() string    { return HandlerContainer }
func (s *fakeSource) Runtime() string        { return "docker" }
func (s *fakeSource) IdleTTL() time.Duration { return DefaultIdleTTL }
func (s *fakeSource) setEnv(k, v string)     { s.mu.Lock(); s.env[k] = v; s.mu.Unlock() }
func (s *fakeSource) setErr(err error)       { s.mu.Lock(); s.err = err; s.mu.Unlock() }

func (s *fakeSource) acquireCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.acquired)
}

func (s *fakeSource) runners() []*fakeRunner {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*fakeRunner(nil), s.acquired...)
}

// hookRecorder captures the emission seam T8/T9 will wire real events onto.
type hookRecorder struct {
	mu       sync.Mutex
	acquired []AcquireInfo
	released []ReleaseInfo
	reaped   []ReleaseInfo

	reapCh chan ReleaseInfo
}

func newHookRecorder() *hookRecorder {
	return &hookRecorder{reapCh: make(chan ReleaseInfo, 64)}
}

func (h *hookRecorder) hooks() PoolHooks {
	return PoolHooks{
		Acquired: func(i AcquireInfo) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.acquired = append(h.acquired, i)
		},
		Released: func(i ReleaseInfo) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.released = append(h.released, i)
		},
		Reaped: func(i ReleaseInfo) {
			h.mu.Lock()
			h.reaped = append(h.reaped, i)
			h.mu.Unlock()
			select {
			case h.reapCh <- i:
			default:
			}
		},
	}
}

func (h *hookRecorder) acquires() []AcquireInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]AcquireInfo(nil), h.acquired...)
}

func (h *hookRecorder) releases() []ReleaseInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]ReleaseInfo(nil), h.released...)
}

func (h *hookRecorder) reaps() []ReleaseInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]ReleaseInfo(nil), h.reaped...)
}

func (h *hookRecorder) waitReap(t *testing.T, want ReleaseCause) ReleaseInfo {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case info := <-h.reapCh:
			if info.Cause == want {
				return info
			}
		case <-deadline:
			t.Fatalf("timed out waiting for reap hook with cause %q; saw %+v", want, h.reaps())
		}
	}
}

func principal(tenant, subject string) loopauth.Principal {
	return loopauth.Principal{Tenant: event.TenantID(tenant), Subject: subject}
}

// --- tests -----------------------------------------------------------------

// TestPoolConcurrentTwoPrincipalIsolation is the containment test.
//
// The two principals check out CONCURRENTLY. A sequential version of this test
// cannot observe the interleaving that produces a cross-principal handout, and
// -race alone cannot either: handing principal B the Runner the pool holds for
// principal A is a perfectly synchronized, perfectly data-race-free containment
// breach (learning: race-invisible-to-race-detector-without-concurrent-test).
func TestPoolConcurrentTwoPrincipalIsolation(t *testing.T) {
	clock := newFakeClock()
	src := newFakeSource(clock)
	pool := NewPool(src, withPoolClock(clock.now), withPoolReapInterval(time.Hour))
	defer func() { _ = pool.Close(context.Background()) }()

	principals := []loopauth.Principal{
		principal("acme", "alice"),
		principal("globex", "bob"),
	}

	const iterations = 200
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var failures []string

	fail := func(format string, args ...any) {
		errMu.Lock()
		defer errMu.Unlock()
		if len(failures) < 10 {
			failures = append(failures, fmt.Sprintf(format, args...))
		}
	}

	// seen records which principal each underlying Runner was handed to, across
	// the whole run. One Runner appearing under two principals is the breach.
	var seenMu sync.Mutex
	seen := map[*fakeRunner]loopauth.Principal{}

	for _, p := range principals {
		for i := 0; i < iterations; i++ {
			wg.Add(1)
			go func(p loopauth.Principal) {
				defer wg.Done()
				ctx := context.Background()

				r, err := pool.Acquire(ctx, p)
				if err != nil {
					fail("Acquire(%v) error: %v", p, err)
					return
				}

				under, ok := Unwrap(r).(*fakeRunner)
				if !ok {
					fail("Unwrap returned %T, want *fakeRunner", Unwrap(r))
					return
				}
				if got := under.acquiredFor(); got != p {
					fail("checkout for %v returned a Runner owned by %v", p, got)
				}

				seenMu.Lock()
				if prev, ok := seen[under]; ok && prev != p {
					fail("Runner previously handed to %v was handed to %v", prev, p)
				}
				seen[under] = p
				seenMu.Unlock()

				if _, err := r.Exec(ctx, "echo hi", ""); err != nil {
					fail("Exec error: %v", err)
				}
				if err := pool.Release(ctx, p, r, CauseReleased); err != nil {
					fail("Release error: %v", err)
				}
			}(p)
		}
	}
	wg.Wait()

	errMu.Lock()
	defer errMu.Unlock()
	for _, f := range failures {
		t.Error(f)
	}
}

// TestPoolResetOnCheckoutReResolvesEnv proves the pool re-resolves the
// environment on EVERY checkout rather than reusing the one cached when the
// Runner was first acquired.
func TestPoolResetOnCheckoutReResolvesEnv(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	src := newFakeSource(clock)
	pool := NewPool(src, withPoolClock(clock.now), withPoolReapInterval(time.Hour))
	defer func() { _ = pool.Close(ctx) }()

	p := principal("acme", "alice")

	first, err := pool.Acquire(ctx, p)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if err := pool.Release(ctx, p, first, CauseReleased); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// The world changes between checkouts: the operator's passthrough now
	// resolves to a different value, and a key that did not exist appears.
	src.setEnv("PATH", "/usr/bin")
	src.setEnv("FUSE_ROTATED", "v2")

	second, err := pool.Acquire(ctx, p)
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	if Unwrap(second) != Unwrap(first) {
		t.Fatalf("expected the warm Runner to be reused")
	}

	got := Unwrap(second).(*fakeRunner).envSnapshot()
	if got["PATH"] != "/usr/bin" {
		t.Errorf("PATH = %q, want the re-resolved %q (stale env was reused)", got["PATH"], "/usr/bin")
	}
	if got["FUSE_ROTATED"] != "v2" {
		t.Errorf("FUSE_ROTATED = %q, want %q (env was not re-applied on checkout)", got["FUSE_ROTATED"], "v2")
	}
}

func TestPoolWarmReuseReportsColdStartOnlyOnce(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	src := newFakeSource(clock)
	rec := newHookRecorder()
	pool := NewPool(src,
		withPoolClock(clock.now),
		withPoolReapInterval(time.Hour),
		WithPoolHooks(rec.hooks()),
	)
	defer func() { _ = pool.Close(ctx) }()

	p := principal("acme", "alice")

	r1, err := pool.Acquire(ctx, p)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if err := pool.Release(ctx, p, r1, CauseReleased); err != nil {
		t.Fatalf("Release: %v", err)
	}
	r2, err := pool.Acquire(ctx, p)
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	defer func() { _ = pool.Release(ctx, p, r2, CauseReleased) }()

	if n := src.acquireCount(); n != 1 {
		t.Fatalf("substrate Acquire called %d times, want 1 (warm Runner was not reused)", n)
	}

	got := rec.acquires()
	if len(got) != 2 {
		t.Fatalf("acquired hook fired %d times, want 2", len(got))
	}
	if got[0].Reused {
		t.Errorf("first acquire reported Reused=true, want false")
	}
	if got[0].ColdStart != src.coldStart {
		t.Errorf("first acquire ColdStart = %v, want %v", got[0].ColdStart, src.coldStart)
	}
	if !got[1].Reused {
		t.Errorf("second acquire reported Reused=false, want true (no warm reuse)")
	}
	if got[1].ColdStart != 0 {
		t.Errorf("warm acquire ColdStart = %v, want 0 (paid a cold start on reuse)", got[1].ColdStart)
	}
	for i, a := range got {
		if a.Handler != HandlerContainer || a.Runtime != "docker" {
			t.Errorf("acquire[%d] labels = %q/%q, want %q/%q", i, a.Handler, a.Runtime, HandlerContainer, "docker")
		}
		if a.Principal != p {
			t.Errorf("acquire[%d] principal = %v, want %v", i, a.Principal, p)
		}
	}
}

func TestPoolReleaseThenReacquireIsUsable(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	src := newFakeSource(clock)
	rec := newHookRecorder()
	pool := NewPool(src, withPoolClock(clock.now), withPoolReapInterval(time.Hour), WithPoolHooks(rec.hooks()))
	defer func() { _ = pool.Close(ctx) }()

	p := principal("acme", "alice")

	r1, err := pool.Acquire(ctx, p)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := pool.Release(ctx, p, r1, CauseReleased); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// A released wrapper is spent: using it after handing it back would let a
	// stale holder drive a Runner the pool has since given to someone else.
	if _, err := r1.Exec(ctx, "echo stale", ""); !errors.Is(err, ErrRunnerReleased) {
		t.Errorf("Exec after Release err = %v, want ErrRunnerReleased", err)
	}

	r2, err := pool.Acquire(ctx, p)
	if err != nil {
		t.Fatalf("re-Acquire: %v", err)
	}
	out, err := r2.Exec(ctx, "echo hi", "")
	if err != nil {
		t.Fatalf("Exec on re-acquired Runner: %v", err)
	}
	if string(out.Combined) != "echo hi" {
		t.Errorf("Exec output = %q, want %q", out.Combined, "echo hi")
	}
	if err := pool.Release(ctx, p, r2, CauseReleased); err != nil {
		t.Fatalf("second Release: %v", err)
	}

	rels := rec.releases()
	if len(rels) != 2 {
		t.Fatalf("released hook fired %d times, want 2", len(rels))
	}
	for i, rel := range rels {
		if rel.Cause != CauseReleased {
			t.Errorf("release[%d] cause = %q, want %q", i, rel.Cause, CauseReleased)
		}
	}
	// Nothing was torn down: the Runner is warm, not discarded.
	if n := src.runners()[0].releaseCount(); n != 0 {
		t.Errorf("underlying Runner torn down %d times on a plain Release, want 0", n)
	}
}

// TestPoolIdleTTLReaperTearsDownIdleRunner proves a crashed or abandoned loop
// cannot leak a warm substrate forever. It uses an injected clock and a short
// tick, not a real multi-second sleep.
func TestPoolIdleTTLReaperTearsDownIdleRunner(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	src := newFakeSource(clock)
	rec := newHookRecorder()
	pool := NewPool(src,
		WithPoolIdleTTL(30*time.Second),
		withPoolClock(clock.now),
		withPoolReapInterval(time.Millisecond),
		WithPoolHooks(rec.hooks()),
	)
	defer func() { _ = pool.Close(ctx) }()

	p := principal("acme", "alice")
	r, err := pool.Acquire(ctx, p)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := pool.Release(ctx, p, r, CauseReleased); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// Not yet idle past the TTL: the reaper must leave it alone.
	clock.advance(10 * time.Second)
	time.Sleep(20 * time.Millisecond)
	if reaps := rec.reaps(); len(reaps) != 0 {
		t.Fatalf("reaped a Runner that was not idle past the TTL: %+v", reaps)
	}

	clock.advance(30 * time.Second)
	info := rec.waitReap(t, CauseIdleTTL)
	if info.Principal != p {
		t.Errorf("reap principal = %v, want %v", info.Principal, p)
	}
	if info.Handler != HandlerContainer {
		t.Errorf("reap handler = %q, want %q", info.Handler, HandlerContainer)
	}

	under := src.runners()[0]
	waitFor(t, "underlying Runner torn down", func() bool { return under.releaseCount() == 1 })

	// The reaped entry is gone, so the next checkout is a genuine cold start.
	if _, err := pool.Acquire(ctx, p); err != nil {
		t.Fatalf("Acquire after reap: %v", err)
	}
	if n := src.acquireCount(); n != 2 {
		t.Errorf("substrate Acquire called %d times, want 2 (reaped entry was still cached)", n)
	}
}

func TestPoolCloseTearsDownEverythingAndStopsReaper(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	src := newFakeSource(clock)
	rec := newHookRecorder()
	pool := NewPool(src,
		withPoolClock(clock.now),
		withPoolReapInterval(time.Millisecond),
		WithPoolHooks(rec.hooks()),
	)

	alice := principal("acme", "alice")
	bob := principal("globex", "bob")

	ra, err := pool.Acquire(ctx, alice)
	if err != nil {
		t.Fatalf("Acquire(alice): %v", err)
	}
	if err := pool.Release(ctx, alice, ra, CauseReleased); err != nil {
		t.Fatalf("Release(alice): %v", err)
	}
	// Bob's Runner is still CHECKED OUT when Close lands — the abandoned-loop
	// shape. Close must tear it down too, or it leaks.
	if _, err := pool.Acquire(ctx, bob); err != nil {
		t.Fatalf("Acquire(bob): %v", err)
	}

	if err := pool.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The reaper goroutine is stopped, not merely told to stop: Close waits for
	// it, so its done channel is closed by the time Close returns.
	select {
	case <-pool.done:
	default:
		t.Fatal("reaper goroutine still running after Close (goroutine leak)")
	}

	for i, r := range src.runners() {
		if n := r.releaseCount(); n != 1 {
			t.Errorf("runner[%d] torn down %d times on Close, want 1", i, n)
		}
	}
	if n := len(rec.reaps()); n != 2 {
		t.Errorf("Close fired %d reap hooks, want 2", n)
	}

	if _, err := pool.Acquire(ctx, alice); !errors.Is(err, ErrPoolClosed) {
		t.Errorf("Acquire after Close err = %v, want ErrPoolClosed", err)
	}
	// Close is idempotent and must not block on a second call.
	if err := pool.Close(ctx); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestPoolCloseThenReleaseTearsDownOnce: Close sweeps entries that are still
// checked out, so the holder's later Release must be bookkeeping only. Tearing
// down twice would double-Release the substrate and double-count the teardown.
func TestPoolCloseThenReleaseTearsDownOnce(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	src := newFakeSource(clock)
	rec := newHookRecorder()
	pool := NewPool(src, withPoolClock(clock.now), withPoolReapInterval(time.Hour), WithPoolHooks(rec.hooks()))

	p := principal("acme", "alice")
	r, err := pool.Acquire(ctx, p)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := pool.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := pool.Release(ctx, p, r, CauseEarlyReturn); err != nil {
		t.Fatalf("Release after Close: %v", err)
	}

	if n := src.runners()[0].releaseCount(); n != 1 {
		t.Errorf("underlying Runner torn down %d times, want 1", n)
	}
	if n := len(rec.reaps()) + len(rec.releases()); n != 1 {
		t.Errorf("teardown hooks fired %d times, want 1 (reaps=%+v releases=%+v)", n, rec.reaps(), rec.releases())
	}
}

// TestPoolReAssertsPrincipalOnHit is the mutation-visible guard test.
//
// It corrupts the pool's own cache so that the entry filed under alice belongs
// to mallory — the state a keying bug would produce — and asserts the pool
// refuses to hand it over, discards it, and cold-starts a fresh one instead.
// Removing the re-assertion in Acquire turns this red.
func TestPoolReAssertsPrincipalOnHit(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	src := newFakeSource(clock)
	rec := newHookRecorder()
	pool := NewPool(src, withPoolClock(clock.now), withPoolReapInterval(time.Hour), WithPoolHooks(rec.hooks()))
	defer func() { _ = pool.Close(ctx) }()

	alice := principal("acme", "alice")
	mallory := principal("evil", "mallory")

	r, err := pool.Acquire(ctx, alice)
	if err != nil {
		t.Fatalf("Acquire(alice): %v", err)
	}
	tainted := Unwrap(r).(*fakeRunner)
	if err := pool.Release(ctx, alice, r, CauseReleased); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// Corrupt the cache: the Runner filed under alice now belongs to mallory.
	tainted.setOwner(mallory)

	got, err := pool.Acquire(ctx, alice)
	if err != nil {
		t.Fatalf("Acquire(alice) after corruption: %v", err)
	}
	if Unwrap(got) == Runner(tainted) {
		t.Fatal("pool handed alice a Runner owned by mallory")
	}
	if n := src.acquireCount(); n != 2 {
		t.Errorf("substrate Acquire called %d times, want 2 (no fresh cold start after the mismatch)", n)
	}
	if n := tainted.releaseCount(); n != 1 {
		t.Errorf("mismatched Runner torn down %d times, want 1", n)
	}

	var sawStale bool
	for _, rp := range rec.reaps() {
		if rp.Cause == CauseStaleCheckout {
			sawStale = true
		}
	}
	if !sawStale {
		t.Errorf("no reap hook with cause %q; saw %+v", CauseStaleCheckout, rec.reaps())
	}
}

// envOnlyRunner can re-apply its environment but cannot report its owner. It
// isolates the pool's OWN key re-assertion: with no acquiredFor to fall back on
// and a working ResetEnv, the entry's recorded principal is the only thing
// standing between one principal and another's shell.
type envOnlyRunner struct{ inner *fakeRunner }

func (r *envOnlyRunner) Exec(ctx context.Context, cmd string, wd string) (Output, error) {
	return r.inner.Exec(ctx, cmd, wd)
}
func (r *envOnlyRunner) Release(ctx context.Context) error { return r.inner.Release(ctx) }
func (r *envOnlyRunner) ResetEnv(env Env) error            { return r.inner.ResetEnv(env) }

// TestPoolReAssertsEntryKeyOnHit corrupts the pool's MAP rather than the
// Runner: an entry belonging to mallory is filed under alice's key, which is
// exactly the state a keying bug produces. Defeating the entry-record
// re-assertion in Acquire turns this red.
func TestPoolReAssertsEntryKeyOnHit(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	src := newFakeSource(clock)
	rec := newHookRecorder()
	pool := NewPool(src, withPoolClock(clock.now), withPoolReapInterval(time.Hour), WithPoolHooks(rec.hooks()))
	defer func() { _ = pool.Close(ctx) }()

	alice := principal("acme", "alice")
	mallory := principal("evil", "mallory")

	victim := &fakeRunner{owner: mallory, env: map[string]string{}}
	tainted := &envOnlyRunner{inner: victim}
	e := &poolEntry{
		principal: mallory,
		runner:    tainted,
		handler:   HandlerContainer,
		runtime:   "docker",
		pooled:    true,
		lastUsed:  clock.now(),
	}
	pool.mu.Lock()
	pool.entries[alice] = e
	pool.live[e] = struct{}{}
	pool.mu.Unlock()

	got, err := pool.Acquire(ctx, alice)
	if err != nil {
		t.Fatalf("Acquire(alice): %v", err)
	}
	if Unwrap(got) == Runner(tainted) {
		t.Fatal("pool handed alice an entry recorded as belonging to mallory")
	}
	if n := src.acquireCount(); n != 1 {
		t.Errorf("substrate Acquire called %d times, want 1 (no cold start after the key mismatch)", n)
	}
	if n := victim.releaseCount(); n != 1 {
		t.Errorf("mis-keyed Runner torn down %d times, want 1", n)
	}
	var sawStale bool
	for _, rp := range rec.reaps() {
		if rp.Cause == CauseStaleCheckout {
			sawStale = true
		}
	}
	if !sawStale {
		t.Errorf("no reap hook with cause %q; saw %+v", CauseStaleCheckout, rec.reaps())
	}
}

// TestPoolDiscardsRunnerItCannotResetEnvOn: a Runner whose environment cannot
// be re-applied cannot be certified for reuse, so it must be discarded rather
// than handed out carrying whatever it was acquired with.
func TestPoolDiscardsRunnerItCannotResetEnvOn(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	src := &unresettableSource{fakeSource: newFakeSource(clock)}
	rec := newHookRecorder()
	pool := NewPool(src, withPoolClock(clock.now), withPoolReapInterval(time.Hour), WithPoolHooks(rec.hooks()))
	defer func() { _ = pool.Close(ctx) }()

	p := principal("acme", "alice")
	r, err := pool.Acquire(ctx, p)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := pool.Release(ctx, p, r, CauseReleased); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := pool.Acquire(ctx, p); err != nil {
		t.Fatalf("second Acquire: %v", err)
	}

	if n := src.acquireCount(); n != 2 {
		t.Errorf("substrate Acquire called %d times, want 2 (a Runner that cannot be reset was reused)", n)
	}
	acqs := rec.acquires()
	if len(acqs) != 2 || acqs[1].Reused {
		t.Errorf("second acquire reported Reused=%v, want false", acqs[1].Reused)
	}
}

// unresettableSource hands out Runners with no ResetEnv method.
type unresettableSource struct{ *fakeSource }

func (s *unresettableSource) Acquire(ctx context.Context, p loopauth.Principal) (Runner, error) {
	r, err := s.fakeSource.Acquire(ctx, p)
	if err != nil {
		return nil, err
	}
	return plainRunner{r}, nil
}

// plainRunner implements only the Runner contract: no ResetEnv, no
// acquiredFor.
type plainRunner struct{ inner Runner }

func (p plainRunner) Exec(ctx context.Context, cmd string, wd string) (Output, error) {
	return p.inner.Exec(ctx, cmd, wd)
}
func (p plainRunner) Release(ctx context.Context) error { return p.inner.Release(ctx) }

// TestPoolReleaseRejectsForeignPrincipal: releasing under the wrong principal
// is a caller bug that must never re-pool the Runner under that key.
func TestPoolReleaseRejectsForeignPrincipal(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	src := newFakeSource(clock)
	pool := NewPool(src, withPoolClock(clock.now), withPoolReapInterval(time.Hour))
	defer func() { _ = pool.Close(ctx) }()

	alice := principal("acme", "alice")
	mallory := principal("evil", "mallory")

	r, err := pool.Acquire(ctx, alice)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	under := Unwrap(r).(*fakeRunner)

	if err := pool.Release(ctx, mallory, r, CauseReleased); !errors.Is(err, ErrPrincipalMismatch) {
		t.Fatalf("Release under a foreign principal err = %v, want ErrPrincipalMismatch", err)
	}
	if n := under.releaseCount(); n != 1 {
		t.Errorf("Runner torn down %d times after a mismatched Release, want 1", n)
	}
	if _, err := pool.Acquire(ctx, mallory); err != nil {
		t.Fatalf("Acquire(mallory): %v", err)
	}
	if got := src.runners(); len(got) != 2 {
		t.Fatalf("substrate Acquire called %d times, want 2", len(got))
	}
	if src.runners()[1] == under {
		t.Error("mallory received alice's Runner")
	}
}

// TestPoolReleaseIsIdempotent guards double-release, which T12's early-return
// path can produce (release on the error path plus release in a defer).
func TestPoolReleaseIsIdempotent(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	src := newFakeSource(clock)
	rec := newHookRecorder()
	pool := NewPool(src, withPoolClock(clock.now), withPoolReapInterval(time.Hour), WithPoolHooks(rec.hooks()))
	defer func() { _ = pool.Close(ctx) }()

	p := principal("acme", "alice")
	r, err := pool.Acquire(ctx, p)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := pool.Release(ctx, p, r, CauseEarlyReturn); err != nil {
			t.Fatalf("Release #%d: %v", i, err)
		}
	}
	if n := len(rec.releases()); n != 1 {
		t.Errorf("released hook fired %d times for 3 Releases, want 1", n)
	}
}

// TestPoolRunnerReleaseReturnsToPool: the Runner's own Release satisfies the
// substrate contract by returning to the pool, so callers holding only a Runner
// can release on every early-return path without knowing about the pool.
func TestPoolRunnerReleaseReturnsToPool(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	src := newFakeSource(clock)
	pool := NewPool(src, withPoolClock(clock.now), withPoolReapInterval(time.Hour))
	defer func() { _ = pool.Close(ctx) }()

	p := principal("acme", "alice")
	r, err := pool.Acquire(ctx, p)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := r.Release(ctx); err != nil {
		t.Fatalf("Runner.Release: %v", err)
	}
	if _, err := pool.Acquire(ctx, p); err != nil {
		t.Fatalf("re-Acquire: %v", err)
	}
	if n := src.acquireCount(); n != 1 {
		t.Errorf("substrate Acquire called %d times, want 1 (Runner.Release did not return it to the pool)", n)
	}
}

// TestPoolAcquireErrorIsNotSwallowed: a refusing Service (no container runtime)
// must surface through the pool untouched — never as a nil Runner and nil error.
func TestPoolAcquireErrorPropagates(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	src := newFakeSource(clock)
	src.setErr(ErrRefusedUncontained)
	pool := NewPool(src, withPoolClock(clock.now), withPoolReapInterval(time.Hour))
	defer func() { _ = pool.Close(ctx) }()

	r, err := pool.Acquire(ctx, principal("acme", "alice"))
	if !errors.Is(err, ErrRefusedUncontained) {
		t.Fatalf("Acquire err = %v, want ErrRefusedUncontained", err)
	}
	if r != nil {
		t.Fatalf("Acquire returned a Runner alongside a refusal: %T", r)
	}
}

// TestServiceSatisfiesPoolSource pins the production wiring: the pool's source
// seam is exactly what *Service provides.
func TestServiceSatisfiesPoolSource(t *testing.T) {
	var _ PoolSource = (*Service)(nil)
}

// waitFor polls cond until it holds or the deadline expires. It exists so the
// reaper assertions do not sleep for a fixed pessimistic duration.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
