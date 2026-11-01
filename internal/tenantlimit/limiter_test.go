package tenantlimit

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
)

// fakeClock is a mutable clock shared by the limiter under test.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeCounts is a mutex-guarded live count per tenant; both get and set lock.
type fakeCounts struct {
	mu  sync.Mutex
	n   map[event.TenantID]int
	err error
}

func newFakeCounts() *fakeCounts { return &fakeCounts{n: map[event.TenantID]int{}} }

func (f *fakeCounts) set(t event.TenantID, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n[t] = n
}

func (f *fakeCounts) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeCounts) count(_ context.Context, t event.TenantID) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	return f.n[t], nil
}

func mustNew(t *testing.T, cfg Config, count LiveCounter, now func() time.Time) *Limiter {
	t.Helper()
	l, err := New(cfg, count, now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return l
}

func asRefused(t *testing.T, err error) *RefusedError {
	t.Helper()
	var r *RefusedError
	if !errors.As(err, &r) {
		t.Fatalf("want *RefusedError, got %T: %v", err, err)
	}
	return r
}

func TestConfigForPerAxisOverride(t *testing.T) {
	cfg := Config{
		Default: Limits{MaxLiveLoops: 2, StartsPerMinute: 3},
		Tenants: map[string]Limits{"big": {MaxLiveLoops: 10}},
	}
	if got := cfg.For("big"); got != (Limits{MaxLiveLoops: 10, StartsPerMinute: 3}) {
		t.Fatalf("For(big) = %+v", got)
	}
	if got := cfg.For("small"); got != (Limits{MaxLiveLoops: 2, StartsPerMinute: 3}) {
		t.Fatalf("For(small) = %+v", got)
	}
	if a, b := cfg.For(""), cfg.For(event.DefaultTenant); a != b || a != (Limits{2, 3}) {
		t.Fatalf("For(\"\") = %+v, For(_default) = %+v", a, b)
	}
	cfg.Tenants["_default"] = Limits{MaxLiveLoops: 7}
	if got := cfg.For(""); got.MaxLiveLoops != 7 || got.StartsPerMinute != 3 {
		t.Fatalf("For(\"\") with _default override = %+v", got)
	}
}

func TestConfigAny(t *testing.T) {
	if (Config{}).Any() {
		t.Fatal("zero config: Any() = true")
	}
	if !(Config{Default: Limits{StartsPerMinute: 1}}).Any() {
		t.Fatal("default-only config: Any() = false")
	}
	if !(Config{Tenants: map[string]Limits{"x": {MaxLiveLoops: 1}}}).Any() {
		t.Fatal("override-only config: Any() = false")
	}
	if (Config{Tenants: map[string]Limits{"x": {}}}).Any() {
		t.Fatal("all-zero override: Any() = true")
	}
}

func TestAdmitUnlimitedNeverCounts(t *testing.T) {
	counter := func(context.Context, event.TenantID) (int, error) {
		t.Fatal("counter called on an unlimited config")
		return 0, nil
	}
	l := mustNew(t, Config{}, counter, nil)
	release, err := l.Admit(context.Background(), "a")
	if err != nil || release == nil {
		t.Fatalf("Admit = (%v, %v), want no-op release and nil", release == nil, err)
	}
	release()
	release()

	var nl *Limiter
	release, err = nl.Admit(context.Background(), "a")
	if err != nil || release == nil {
		t.Fatalf("nil Limiter Admit = (%v, %v)", release == nil, err)
	}
	release()
	if got := nl.For("a"); got != (Limits{}) {
		t.Fatalf("nil Limiter For = %+v", got)
	}
}

func TestNewRefusesCapWithoutCounter(t *testing.T) {
	if _, err := New(Config{Default: Limits{MaxLiveLoops: 1}}, nil, nil); err == nil {
		t.Fatal("New with a default cap and nil counter: want error")
	}
	if _, err := New(Config{Tenants: map[string]Limits{"x": {MaxLiveLoops: 1}}}, nil, nil); err == nil {
		t.Fatal("New with an override cap and nil counter: want error")
	}
	if _, err := New(Config{Default: Limits{StartsPerMinute: 1}}, nil, nil); err != nil {
		t.Fatalf("New rate-only with nil counter: %v", err)
	}
}

func TestAdmitCapRefusesAtMax(t *testing.T) {
	counts := newFakeCounts()
	counts.set("a", 2)
	l := mustNew(t, Config{Default: Limits{MaxLiveLoops: 2}}, counts.count, nil)
	_, err := l.Admit(context.Background(), "a")
	r := asRefused(t, err)
	if r.Axis != AxisLiveLoops || r.Limit != 2 || r.RetryAfter != 0 || r.Tenant != "a" {
		t.Fatalf("refusal = %+v", r)
	}
	if msg := r.Error(); !strings.Contains(msg, "max_live_loops") || !strings.Contains(msg, "2") {
		t.Fatalf("message %q does not name the limit", msg)
	}
	release, err := l.Admit(context.Background(), "b")
	if err != nil {
		t.Fatalf("tenant b refused: %v", err)
	}
	release()
}

func TestAdmitCapCountsInFlightReservations(t *testing.T) {
	counts := newFakeCounts()
	l := mustNew(t, Config{Default: Limits{MaxLiveLoops: 2}}, counts.count, nil)
	ctx := context.Background()
	r1, err := l.Admit(ctx, "a")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := l.Admit(ctx, "a"); err != nil {
		t.Fatalf("second: %v", err)
	}
	if _, err := l.Admit(ctx, "a"); err == nil {
		t.Fatal("third: want refusal with two in-flight reservations")
	} else {
		asRefused(t, err)
	}
	r1()
	r1() // idempotent: must not free a second slot
	if _, err := l.Admit(ctx, "a"); err != nil {
		t.Fatalf("after release: %v", err)
	}
	if _, err := l.Admit(ctx, "a"); err == nil {
		t.Fatal("double release freed two slots")
	}
}

func TestAdmitCapCounterErrorRefuses(t *testing.T) {
	counts := newFakeCounts()
	boom := errors.New("boom")
	counts.setErr(boom)
	l := mustNew(t, Config{Default: Limits{MaxLiveLoops: 1}}, counts.count, nil)
	release, err := l.Admit(context.Background(), "a")
	if err == nil {
		t.Fatal("want error when the counter fails")
	}
	if release != nil {
		t.Fatal("want nil release on error")
	}
	var r *RefusedError
	if errors.As(err, &r) {
		t.Fatalf("counter error surfaced as a refusal: %v", err)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("error %v does not wrap boom", err)
	}
	counts.setErr(nil)
	if _, err := l.Admit(context.Background(), "a"); err != nil {
		t.Fatalf("reservation leaked by the failed count: %v", err)
	}
}

func TestAdmitRateRefusesWithRetryAfter(t *testing.T) {
	clk := newFakeClock()
	l := mustNew(t, Config{Default: Limits{StartsPerMinute: 3}}, nil, clk.Now)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		release, err := l.Admit(ctx, "a")
		if err != nil {
			t.Fatalf("start %d: %v", i+1, err)
		}
		release()
	}
	_, err := l.Admit(ctx, "a")
	r := asRefused(t, err)
	if r.Axis != AxisStartRate || r.Limit != 3 || r.RetryAfter != 20*time.Second {
		t.Fatalf("refusal = %+v", r)
	}
	if msg := r.Error(); !strings.Contains(msg, "starts_per_minute") || !strings.Contains(msg, "20s") {
		t.Fatalf("message %q", msg)
	}
	if _, err := l.Admit(ctx, "b"); err != nil {
		t.Fatalf("tenant b refused while a is exhausted: %v", err)
	}
	clk.Advance(20 * time.Second)
	if _, err := l.Admit(ctx, "a"); err != nil {
		t.Fatalf("after the advertised Retry-After: %v", err)
	}
}

func TestAdmitRateRetryAfterRoundsUpToWholeSeconds(t *testing.T) {
	clk := newFakeClock()
	l := mustNew(t, Config{Default: Limits{StartsPerMinute: 7}}, nil, clk.Now)
	ctx := context.Background()
	for i := 0; i < 7; i++ {
		if _, err := l.Admit(ctx, "a"); err != nil {
			t.Fatalf("start %d: %v", i+1, err)
		}
	}
	clk.Advance(time.Second)
	_, err := l.Admit(ctx, "a")
	r := asRefused(t, err)
	refill := 7.0 / 60.0
	tokens := 1 * refill
	want := time.Duration(math.Ceil((1-tokens)/refill)) * time.Second
	if r.RetryAfter != want || r.RetryAfter < time.Second || r.RetryAfter%time.Second != 0 {
		t.Fatalf("RetryAfter = %v, want %v (whole seconds, >= 1s)", r.RetryAfter, want)
	}
	// Retry-After is honest: waiting exactly that long admits the next start.
	clk.Advance(r.RetryAfter)
	if _, err := l.Admit(ctx, "a"); err != nil {
		t.Fatalf("after RetryAfter %v: %v", r.RetryAfter, err)
	}
}

func TestAdmitRateBucketRefillsToCapacityOnly(t *testing.T) {
	clk := newFakeClock()
	l := mustNew(t, Config{Default: Limits{StartsPerMinute: 2}}, nil, clk.Now)
	ctx := context.Background()
	clk.Advance(time.Hour) // a long idle must not bank more than one minute of burst
	for i := 0; i < 2; i++ {
		if _, err := l.Admit(ctx, "a"); err != nil {
			t.Fatalf("start %d: %v", i+1, err)
		}
	}
	if _, err := l.Admit(ctx, "a"); err == nil {
		t.Fatal("bucket overflowed its capacity after an idle hour")
	}
}

func TestAdmitRateCheckedBeforeCap(t *testing.T) {
	clk := newFakeClock()
	counts := newFakeCounts()
	counts.set("a", 1)
	l := mustNew(t, Config{Default: Limits{MaxLiveLoops: 1, StartsPerMinute: 1}}, counts.count, clk.Now)
	_, err := l.Admit(context.Background(), "a")
	if r := asRefused(t, err); r.Axis != AxisLiveLoops {
		t.Fatalf("first refusal axis = %s, want %s", r.Axis, AxisLiveLoops)
	}
	_, err = l.Admit(context.Background(), "a")
	if r := asRefused(t, err); r.Axis != AxisStartRate {
		t.Fatalf("second refusal axis = %s, want %s (first attempt spent the token)", r.Axis, AxisStartRate)
	}
}

func TestAdmitNormalizesTenant(t *testing.T) {
	counts := newFakeCounts()
	l := mustNew(t, Config{Default: Limits{MaxLiveLoops: 1}}, counts.count, nil)
	if _, err := l.Admit(context.Background(), ""); err != nil {
		t.Fatalf("empty tenant: %v", err)
	}
	// The empty tenant and _default share one reservation pool.
	if _, err := l.Admit(context.Background(), event.DefaultTenant); err == nil {
		t.Fatal("_default admitted beside an in-flight empty-tenant start")
	}
}

func TestAdmitCancelledWhileWaitingForTenantLock(t *testing.T) {
	entered := make(chan struct{})
	unblock := make(chan struct{})
	var once sync.Once
	counter := func(ctx context.Context, _ event.TenantID) (int, error) {
		once.Do(func() { close(entered) })
		<-unblock
		return 0, nil
	}
	l := mustNew(t, Config{Default: Limits{MaxLiveLoops: 5}}, counter, nil)
	done := make(chan error, 1)
	go func() {
		_, err := l.Admit(context.Background(), "a")
		done <- err
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	release, err := l.Admit(ctx, "a")
	if !errors.Is(err, context.Canceled) || release != nil {
		t.Fatalf("waiter with a cancelled ctx = (%v, %v), want context.Canceled", release != nil, err)
	}
	close(unblock)
	if err := <-done; err != nil {
		t.Fatalf("holder: %v", err)
	}
}

// listRegistry is a List-only event.LoopRegistry fake.
type listRegistry struct {
	event.LoopRegistry
	recs map[event.TenantID][]event.LoopRecord
	err  error
}

func (r *listRegistry) List(_ context.Context, tenant event.TenantID) ([]event.LoopRecord, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.recs[event.NormalizeTenant(tenant)], nil
}

func TestRegistryCounterCountsLiveUnexpired(t *testing.T) {
	clk := newFakeClock()
	now := clk.Now()
	key := func(tenant event.TenantID, id string) event.StreamKey {
		return event.StreamKey{Tenant: tenant, Loop: event.LoopID(id)}
	}
	reg := &listRegistry{recs: map[event.TenantID][]event.LoopRecord{
		"a": {
			{Key: key("a", "future"), Live: true, LeaseExpiry: now.Add(time.Minute)},
			{Key: key("a", "nolease"), Live: true},
			{Key: key("a", "atexpiry"), Live: true, LeaseExpiry: now},
			{Key: key("a", "expired"), Live: true, LeaseExpiry: now.Add(-time.Second)},
			{Key: key("a", "finished"), Live: false, LeaseExpiry: now.Add(time.Minute)},
			// A record keyed to another tenant is never counted, even if a backend
			// returned it for "a".
			{Key: key("b", "stray"), Live: true},
		},
		"b": {{Key: key("b", "x"), Live: true}},
	}}
	count := RegistryCounter(reg, clk.Now)
	n, err := count(context.Background(), "a")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Fatalf("count(a) = %d, want 3 (future lease, no lease, lease at now)", n)
	}
	reg.err = errors.New("list down")
	if _, err := count(context.Background(), "a"); !errors.Is(err, reg.err) {
		t.Fatalf("List error not propagated: %v", err)
	}
	if RegistryCounter(nil, clk.Now) != nil {
		t.Fatal("RegistryCounter(nil) should be nil so New refuses an inert cap")
	}
}

func TestAdmitConcurrentNeverExceedsCap(t *testing.T) {
	const max, workers = 5, 50
	var (
		mu         sync.Mutex
		registered int
		peak       int
	)
	counter := func(context.Context, event.TenantID) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return registered, nil
	}
	l := mustNew(t, Config{Default: Limits{MaxLiveLoops: max}}, counter, nil)

	var (
		wg       sync.WaitGroup
		admitted int
		amu      sync.Mutex
		start    = make(chan struct{})
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			release, err := l.Admit(context.Background(), "a")
			if err != nil {
				asRefused(t, err)
				return
			}
			amu.Lock()
			admitted++
			amu.Unlock()
			mu.Lock()
			registered++ // the runtime's Register: the loop stays live
			if registered > peak {
				peak = registered
			}
			mu.Unlock()
			release()
		}()
	}
	close(start)
	wg.Wait()
	if admitted != max {
		t.Fatalf("admitted %d starts, want exactly %d", admitted, max)
	}
	if peak > max {
		t.Fatalf("registered peaked at %d, above the cap %d", peak, max)
	}
	// Every release ran: no reservation outlives its start.
	l.mu.Lock()
	defer l.mu.Unlock()
	if p := l.pending["a"]; p != 0 {
		t.Fatalf("pending[a] = %d after every release, want 0", p)
	}
}
