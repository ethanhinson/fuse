package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/loopauth"
)

// WARM-CONTAINER BLEED, ASSERTED ON THE MOUNT (change 0065, task 4).
//
// pool.go already re-asserts the PRINCIPAL of a warm entry on every cache hit
// (certifyEntry, and the two tests at TestPoolReAssertsPrincipalOnHit /
// TestPoolReAssertsEntryKeyOnHit). Today the mount follows from that for free:
// containerRunner.root is derived from the authenticated Principal at Acquire
// and never reassigned, so an entry certified for its principal structurally
// carries that principal's tenant root.
//
// That is an ARGUMENT, not a test — and an argument about code that can be
// refactored. The learning
// cache-over-tenant-scoped-source-reassert-key-on-hit is precisely about a
// cache over a tenant-scoped source returning the wrong tenant's object on a
// hit, and the object at stake here is not the Principal, it is the HOST
// DIRECTORY the container is handed. So the mount is made an independently
// asserted property: the pool records the mount a warm entry was certified with
// and re-asserts it on every hit, and the tests below fail if a warm entry can
// ever be handed out carrying a mount other than the one it was cold-started
// with.
//
// The observable surface is deliberately the argv's `-v <root>:/workspace`
// wherever a real handler is in play. That string is what actually reaches the
// container runtime, and an operator's isolation depends on it and on nothing
// else; a test that read only the unexported field could stay green while argv
// mounted something different.

// --- doubles ---------------------------------------------------------------

// mountedRunner is a fakeRunner that also reports a mount root, standing in for
// containerRunner's mountRoot(). Its root is mutable ONLY so a test can corrupt
// it the way TestPoolReAssertsPrincipalOnHit corrupts the owner; production
// Runners fix theirs at Acquire and never reassign it.
type mountedRunner struct {
	*fakeRunner

	mu   sync.Mutex
	root string
}

func (r *mountedRunner) mountRoot() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.root
}

func (r *mountedRunner) setMountRoot(root string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.root = root
}

// mountedSource hands out mountedRunners whose root is a function of the
// principal's TENANT — the same derivation the real handler performs, reduced
// to the one property under test. Two tenants therefore get two distinct,
// non-overlapping roots, and a hit that returns the wrong one is a
// cross-tenant mount.
type mountedSource struct {
	*fakeSource

	mu       sync.Mutex
	acquired []*mountedRunner
}

func newMountedSource(clock *fakeClock) *mountedSource {
	return &mountedSource{fakeSource: newFakeSource(clock)}
}

// rootForTenant is the double's tenant→root layout: siblings under one parent,
// mirroring TenantRoots' real layout so "one level up" would be a shared tree.
func rootForTenant(p loopauth.Principal) string {
	return "/srv/fuse/tenants/" + string(p.Tenant)
}

func (s *mountedSource) Acquire(ctx context.Context, p loopauth.Principal) (Runner, error) {
	inner, err := s.fakeSource.Acquire(ctx, p)
	if err != nil {
		return nil, err
	}
	r := &mountedRunner{fakeRunner: inner.(*fakeRunner), root: rootForTenant(p)}
	s.mu.Lock()
	s.acquired = append(s.acquired, r)
	s.mu.Unlock()
	return r, nil
}

func (s *mountedSource) mountedRunners() []*mountedRunner {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*mountedRunner(nil), s.acquired...)
}

// --- mutation-visible guard tests ------------------------------------------

// TestPoolReAssertsMountOnHit is the mutation-visible guard, in the idiom of
// TestPoolReAssertsPrincipalOnHit: it corrupts the pool's own cache so the
// entry filed under — and genuinely OWNED by — tenant A's principal now carries
// tenant B's mount root, and asserts the pool refuses to hand it over, discards
// it, and cold-starts instead.
//
// The corruption is deliberately mount-ONLY: the principal still certifies on
// both sides, so the existing principal re-assertion cannot be what saves this
// test. Only a re-assertion of the MOUNT can, which is the point — this is the
// state a refactor that decoupled the root from the Principal would produce,
// and it must fail loudly here rather than silently share a directory.
func TestPoolReAssertsMountOnHit(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	src := newMountedSource(clock)
	rec := newHookRecorder()
	pool := NewPool(src, withPoolClock(clock.now), withPoolReapInterval(time.Hour), WithPoolHooks(rec.hooks()))
	defer func() { _ = pool.Close(ctx) }()

	alice := principal("acme", "alice")
	mallory := principal("evil", "mallory")

	r, err := pool.Acquire(ctx, alice)
	if err != nil {
		t.Fatalf("Acquire(alice): %v", err)
	}
	tainted := Unwrap(r).(*mountedRunner)
	if err := pool.Release(ctx, alice, r, CauseReleased); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// The corruption: alice's own Runner now mounts mallory's tenant tree.
	// Everything else about the entry still certifies.
	tainted.setMountRoot(rootForTenant(mallory))

	got, err := pool.Acquire(ctx, alice)
	if err != nil {
		t.Fatalf("Acquire(alice) after corruption: %v", err)
	}
	if Unwrap(got) == Runner(tainted) {
		t.Fatalf("pool handed alice a warm Runner mounting %q", tainted.mountRoot())
	}
	if n := src.acquireCount(); n != 2 {
		t.Errorf("substrate Acquire called %d times, want 2 (no cold start after the mount mismatch)", n)
	}
	if n := tainted.releaseCount(); n != 1 {
		t.Errorf("mismatched Runner torn down %d times, want 1", n)
	}
	// The discard must take the SAME path the principal mismatch takes: a
	// mount mismatch is a stale checkout, not a quiet swap.
	var sawStale bool
	for _, rp := range rec.reaps() {
		if rp.Cause == CauseStaleCheckout {
			sawStale = true
		}
	}
	if !sawStale {
		t.Errorf("no reap hook with cause %q; saw %+v", CauseStaleCheckout, rec.reaps())
	}

	// And the replacement really is this tenant's tree, not merely a different
	// object: a cold start that re-derived the wrong root would satisfy every
	// assertion above.
	if root := Unwrap(got).(*mountedRunner).mountRoot(); root != rootForTenant(alice) {
		t.Errorf("replacement Runner mounts %q, want %q", root, rootForTenant(alice))
	}
}

// TestPoolReAssertsEntryMountOnHit corrupts the pool's ENTRY record rather than
// the Runner — the mirror of TestPoolReAssertsEntryKeyOnHit. An entry whose
// recorded mount no longer matches the Runner it holds is an entry nobody can
// reason about: one of the two is wrong, and the pool cannot know which, so the
// only safe answer is to discard it.
func TestPoolReAssertsEntryMountOnHit(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	src := newMountedSource(clock)
	rec := newHookRecorder()
	pool := NewPool(src, withPoolClock(clock.now), withPoolReapInterval(time.Hour), WithPoolHooks(rec.hooks()))
	defer func() { _ = pool.Close(ctx) }()

	alice := principal("acme", "alice")
	mallory := principal("evil", "mallory")

	r, err := pool.Acquire(ctx, alice)
	if err != nil {
		t.Fatalf("Acquire(alice): %v", err)
	}
	victim := Unwrap(r).(*mountedRunner)
	if err := pool.Release(ctx, alice, r, CauseReleased); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// Corrupt the pool's bookkeeping: the entry claims to have been certified
	// against mallory's tree while holding a Runner mounting alice's.
	pool.mu.Lock()
	pool.entries[alice].mountRoot = rootForTenant(mallory)
	pool.mu.Unlock()

	got, err := pool.Acquire(ctx, alice)
	if err != nil {
		t.Fatalf("Acquire(alice) after corruption: %v", err)
	}
	if Unwrap(got) == Runner(victim) {
		t.Fatal("pool handed alice an entry recorded against another tenant's mount")
	}
	if n := src.acquireCount(); n != 2 {
		t.Errorf("substrate Acquire called %d times, want 2 (no cold start after the recorded-mount mismatch)", n)
	}
	if n := victim.releaseCount(); n != 1 {
		t.Errorf("mis-recorded Runner torn down %d times, want 1", n)
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

// TestPoolWarmReuseKeepsItsOwnMount is the negative control for the two guards
// above: an UNCORRUPTED warm entry must still be reused, and must still carry
// the same mount it was cold-started with. Without it, a certifyEntry that
// refused every hit would pass both guard tests while silently destroying the
// warm pool.
func TestPoolWarmReuseKeepsItsOwnMount(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	src := newMountedSource(clock)
	pool := NewPool(src, withPoolClock(clock.now), withPoolReapInterval(time.Hour))
	defer func() { _ = pool.Close(ctx) }()

	alice := principal("acme", "alice")

	first, err := pool.Acquire(ctx, alice)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if err := pool.Release(ctx, alice, first, CauseReleased); err != nil {
		t.Fatalf("Release: %v", err)
	}
	second, err := pool.Acquire(ctx, alice)
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	if Unwrap(second) != Unwrap(first) {
		t.Fatal("warm entry was not reused; the mount re-assertion is refusing valid hits")
	}
	if n := src.acquireCount(); n != 1 {
		t.Errorf("substrate Acquire called %d times, want 1", n)
	}
	if root := Unwrap(second).(*mountedRunner).mountRoot(); root != rootForTenant(alice) {
		t.Errorf("reused Runner mounts %q, want %q", root, rootForTenant(alice))
	}
}

// --- the concurrent two-tenant mount test ----------------------------------

// concurrentRun is a thread-safe exec recorder. recordingRun is not: it is
// written by whichever goroutine reaches Exec, and every field crossing that
// boundary here is mutex-guarded on BOTH sides (learning:
// mutex-test-double-concurrent-provider). It records, per RUN invocation, the
// mount the assembled argv carried.
type concurrentRun struct {
	mu     sync.Mutex
	mounts map[string]int
	// byCmd maps the command string a goroutine passed — which encodes the
	// tenant it belongs to — to every mount that command was ever run with.
	byCmd map[string]map[string]struct{}
}

func newConcurrentRun() *concurrentRun {
	return &concurrentRun{mounts: map[string]int{}, byCmd: map[string]map[string]struct{}{}}
}

func (c *concurrentRun) run(_ context.Context, _ string, args ...string) ([]byte, int, error) {
	if len(args) > 0 && args[0] == "pull" {
		return nil, 0, nil
	}
	mount, _ := argValue(args, "-v")
	// The command is the LAST argument of the assembled run argv (see argv):
	// reading it back out of the recorded argv, rather than trusting a
	// side-channel, is what ties "this mount" to "this tenant's command".
	cmd := ""
	if len(args) > 0 {
		cmd = args[len(args)-1]
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mounts[mount]++
	if c.byCmd[cmd] == nil {
		c.byCmd[cmd] = map[string]struct{}{}
	}
	c.byCmd[cmd][mount] = struct{}{}
	return nil, 0, nil
}

func (c *concurrentRun) mountsFor(cmd string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.byCmd[cmd]))
	for m := range c.byCmd[cmd] {
		out = append(out, m)
	}
	return out
}

func (c *concurrentRun) totalRuns() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.mounts {
		n += v
	}
	return n
}

// TestPoolConcurrentTwoTenantMountIsolation is the containment test task 4
// exists for, and it is CONCURRENT on purpose.
//
// Two tenants drive Acquire → Exec → Release against ONE Pool at the same time,
// through the real Service, the real container handler and the real
// TenantRoots resolver, so the thing asserted is the assembled argv's
// `-v <root>:/workspace` — what actually reaches the runtime.
//
// A sequential version of this cannot surface the failure: handing tenant B a
// warm entry the pool holds for tenant A is a perfectly synchronised,
// perfectly data-race-free containment breach, so `-race` alone sees nothing
// without an interleaving to observe (learning:
// race-invisible-to-race-detector-without-concurrent-test). The interleaving is
// what makes the warm-pool hit path, the cold-start path, and the transient
// path all run against both tenants at once.
func TestPoolConcurrentTwoTenantMountIsolation(t *testing.T) {
	parent := trustedTestRoot(t)
	rootA := tenantTestRoot(t, parent, "tenanta")
	rootB := tenantTestRoot(t, parent, "tenantb")

	rec := newConcurrentRun()
	svc, err := NewService(DefaultConfig(),
		withContainerLookPath(fakeLookPath("docker")),
		withContainerExec(rec.run),
		// The trusted root is the SHARED PARENT deliberately: if the per-tenant
		// resolver were bypassed anywhere on the pooled path, both tenants
		// would mount the parent and see each other's tree, and this test would
		// catch exactly that rather than passing under both behaviours.
		WithTrustedRoot(parent),
		WithTenantRoots(NewTenantRoots(parent, false)),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	pool := NewPool(svc)
	defer func() { _ = pool.Close(context.Background()) }()

	tenants := []struct {
		p    loopauth.Principal
		root string
		cmd  string
	}{
		{principal("tenanta", "alice"), rootA, "echo tenanta"},
		{principal("tenantb", "bob"), rootB, "echo tenantb"},
	}

	const iterations = 150
	var (
		wg       sync.WaitGroup
		failMu   sync.Mutex
		failures []string
	)
	fail := func(format string, args ...any) {
		failMu.Lock()
		defer failMu.Unlock()
		if len(failures) < 10 {
			failures = append(failures, fmt.Sprintf(format, args...))
		}
	}

	// start gates every goroutine onto the same instant, so the two tenants
	// really do contend for the pool's single warm slot per principal rather
	// than trickling through it in turn.
	start := make(chan struct{})

	for _, tc := range tenants {
		for i := 0; i < iterations; i++ {
			wg.Add(1)
			go func(p loopauth.Principal, wantRoot, cmd string) {
				defer wg.Done()
				<-start
				ctx := context.Background()

				r, err := pool.Acquire(ctx, p)
				if err != nil {
					fail("Acquire(%v): %v", p, err)
					return
				}
				// The Runner's own root, before anything runs. A warm entry
				// that bled across tenants is caught here even if the Exec
				// below were to be reordered away.
				if got := Unwrap(r).(*containerRunner).root; got != wantRoot {
					fail("checkout for tenant %q carries root %q, want %q", p.Tenant, got, wantRoot)
				}
				if _, err := r.Exec(ctx, cmd, ""); err != nil {
					fail("Exec for tenant %q: %v", p.Tenant, err)
				}
				if err := pool.Release(ctx, p, r, CauseReleased); err != nil {
					fail("Release for tenant %q: %v", p.Tenant, err)
				}
			}(tc.p, tc.root, tc.cmd)
		}
	}
	close(start)
	wg.Wait()

	failMu.Lock()
	for _, f := range failures {
		t.Error(f)
	}
	failMu.Unlock()

	// THE ASSERTION THAT MATTERS: across every one of the interleaved runs,
	// each tenant's command was only ever run against that tenant's own mount.
	for _, tc := range tenants {
		want := tc.root + ":" + containerWorkspace
		got := rec.mountsFor(tc.cmd)
		if len(got) != 1 || got[0] != want {
			t.Errorf("tenant %q ran against mounts %v, want exactly [%q]", tc.p.Tenant, got, want)
		}
	}
	if n := rec.totalRuns(); n != 2*iterations {
		t.Errorf("recorded %d container runs, want %d", n, 2*iterations)
	}
	// And the two mounts are non-overlapping trees, not merely distinct
	// strings: a mount of the shared parent differs from a sibling's path
	// while still exposing it.
	assertDisjoint(t, rootA, rootB)
	assertDisjoint(t, rootB, rootA)
}

// TestPoolWarmHitKeepsTheTenantMountInArgv closes the loop between the pool's
// re-assertion and the observable surface: a WARM hit — the reuse path, where a
// bleed would actually happen — must still assemble argv against its own
// tenant's tree, and the other tenant's checkout in between must not move it.
func TestPoolWarmHitKeepsTheTenantMountInArgv(t *testing.T) {
	ctx := context.Background()
	parent := trustedTestRoot(t)
	rootA := tenantTestRoot(t, parent, "tenanta")
	rootB := tenantTestRoot(t, parent, "tenantb")

	rec := newConcurrentRun()
	svc, err := NewService(DefaultConfig(),
		withContainerLookPath(fakeLookPath("docker")),
		withContainerExec(rec.run),
		WithTrustedRoot(parent),
		WithTenantRoots(NewTenantRoots(parent, false)),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	pool := NewPool(svc)
	defer func() { _ = pool.Close(ctx) }()

	alice := principal("tenanta", "alice")
	bob := principal("tenantb", "bob")

	// Warm both tenants, then interleave: A, B, then A again off the warm slot.
	for _, step := range []struct {
		p    loopauth.Principal
		cmd  string
		want string
	}{
		{alice, "a-cold", rootA},
		{bob, "b-cold", rootB},
		{alice, "a-warm", rootA},
		{bob, "b-warm", rootB},
	} {
		r, err := pool.Acquire(ctx, step.p)
		if err != nil {
			t.Fatalf("Acquire(%v): %v", step.p, err)
		}
		if _, err := r.Exec(ctx, step.cmd, ""); err != nil {
			t.Fatalf("Exec(%s): %v", step.cmd, err)
		}
		if err := pool.Release(ctx, step.p, r, CauseReleased); err != nil {
			t.Fatalf("Release(%v): %v", step.p, err)
		}
		want := step.want + ":" + containerWorkspace
		if got := rec.mountsFor(step.cmd); len(got) != 1 || got[0] != want {
			t.Fatalf("%s ran against mounts %v, want [%q]", step.cmd, got, want)
		}
	}

	// The warm hits really were hits: only two cold starts happened, so the
	// argv assertions above were made on the reuse path and not on four fresh
	// Runners that could never have bled.
	if n := poolLiveEntries(pool); n != 2 {
		t.Errorf("pool holds %d live entries, want 2 (one warm slot per tenant)", n)
	}
}

// poolLiveEntries reports how many execution contexts the pool is tracking.
func poolLiveEntries(p *Pool) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.live)
}

// tenantDirsExist is a guard against the fixture silently degrading: TenantRoots
// with create=false resolves nothing for a tenant whose directory is missing,
// which would turn every mount assertion above into an Acquire failure rather
// than a containment result.
func TestTenantPoolFixtureProvisionsBothTrees(t *testing.T) {
	parent := trustedTestRoot(t)
	rootA := tenantTestRoot(t, parent, "tenanta")
	rootB := tenantTestRoot(t, parent, "tenantb")

	roots := NewTenantRoots(parent, false)
	for _, tc := range []struct {
		p    loopauth.Principal
		want string
	}{
		{principal("tenanta", "alice"), rootA},
		{principal("tenantb", "bob"), rootB},
	} {
		got, err := roots.Root(tc.p)
		if err != nil {
			t.Fatalf("Root(%v): %v", tc.p, err)
		}
		if got != tc.want {
			t.Errorf("Root(%v) = %q, want %q", tc.p, got, tc.want)
		}
		if info, err := os.Stat(got); err != nil || !info.IsDir() {
			t.Errorf("resolved root %q is not a directory: %v", got, err)
		}
		if !strings.HasPrefix(got, parent+string(filepath.Separator)) {
			t.Errorf("resolved root %q is not a child of the parent %q", got, parent)
		}
	}
}
