package ratelimit

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeClock is a test clock advanced explicitly. It is read by the Bucket from
// waiter goroutines, so both the getter (Now) and the setter (Advance) lock mu
// (learning #10: any test double shared with goroutines locks both sides).
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Unix(0, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// newTestBucket builds a Bucket on a fake clock with polling disabled, so waiters
// only wake on Pump or ctx — no real sleeps anywhere in these tests.
func newTestBucket(cfg Config) (*Bucket, *fakeClock) {
	clk := newClock()
	b := New(cfg, clk.Now)
	b.SetPollInterval(0)
	return b, clk
}

// remainingTPM returns the whole tpm tokens currently available for provider,
// refilled to the current clock (a thin wrapper over Utilization for the Report
// reconciliation assertions).
func (b *Bucket) remainingTPM(provider string) int {
	key := b.providerKey(provider)
	for _, u := range b.Utilization() {
		if u.Provider == key {
			return u.TPMRemaining
		}
	}
	return 0
}

func TestAnyReportsConfiguredAxes(t *testing.T) {
	if (Config{}).Any() {
		t.Error("empty config reports Any")
	}
	if !(Config{Global: Limits{RequestsPerMinute: 1}}).Any() {
		t.Error("global rpm not reported")
	}
	if !(Config{Providers: map[string]Limits{"x": {TokensPerMinute: 1}}}).Any() {
		t.Error("provider tpm not reported")
	}
}

// TestRPMCapacityAndRefill: the rpm axis starts full (capacity = rpm) and one
// minute of elapsed clock refills it fully.
func TestRPMCapacityAndRefill(t *testing.T) {
	b, clk := newTestBucket(Config{Global: Limits{RequestsPerMinute: 3}})
	ctx := context.Background()

	// Burst of 3 admits immediately (bucket starts full).
	for i := 0; i < 3; i++ {
		if err := b.Wait(ctx, "m", 0); err != nil {
			t.Fatalf("burst %d: %v", i, err)
		}
	}
	// 4th must block: no tokens, no time passed. Prove it blocks then refills.
	done := make(chan error, 1)
	go func() { done <- b.Wait(ctx, "m", 0) }()
	select {
	case err := <-done:
		t.Fatalf("4th Wait returned early: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	// Advance 20s ⇒ 1 token accrued (3/min = 1 per 20s). Pump lets the waiter re-check.
	clk.Advance(20 * time.Second)
	b.Pump()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("after refill: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter did not unblock after refill")
	}
}

// TestTPMAxisChargesEstimate: the tpm axis blocks when the estimate exceeds the
// remaining fill and unblocks as tokens accrue.
func TestTPMAxisChargesEstimate(t *testing.T) {
	b, clk := newTestBucket(Config{Global: Limits{TokensPerMinute: 600}}) // 10 tok/sec
	ctx := context.Background()

	// Spend the whole 600-token burst in one Wait.
	if err := b.Wait(ctx, "m", 600); err != nil {
		t.Fatal(err)
	}
	// Next 100-token request must block until 10s accrue (100 / 10 tok/s).
	done := make(chan error, 1)
	go func() { done <- b.Wait(ctx, "m", 100) }()
	select {
	case <-done:
		t.Fatal("tpm Wait admitted with empty bucket")
	case <-time.After(20 * time.Millisecond):
	}
	clk.Advance(10 * time.Second)
	b.Pump()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("tpm waiter did not unblock")
	}
}

// TestUnlimitedAxisNeverBlocks: a zero axis is unlimited — any number of requests
// admit instantly regardless of clock.
func TestUnlimitedAxisNeverBlocks(t *testing.T) {
	b, _ := newTestBucket(Config{Global: Limits{RequestsPerMinute: 0, TokensPerMinute: 0}})
	ctx := context.Background()
	for i := 0; i < 1000; i++ {
		if err := b.Wait(ctx, "m", 1_000_000); err != nil {
			t.Fatalf("unlimited blocked at %d: %v", i, err)
		}
	}
}

// TestProviderOverrideSelection: a per-provider override replaces the matching
// global axis for models whose id it prefixes, leaving others on the global axis.
func TestProviderOverrideSelection(t *testing.T) {
	b, _ := newTestBucket(Config{
		Global:    Limits{RequestsPerMinute: 2},
		Providers: map[string]Limits{"cloud": {RequestsPerMinute: 1}},
	})
	ctx := context.Background()

	octx, ocancel := context.WithCancel(ctx)
	defer ocancel()

	// "cloud/kimi" resolves to the override (cap 1): one admit, second blocks.
	if err := b.Wait(ctx, "cloud/kimi-k3", 0); err != nil {
		t.Fatal(err)
	}
	blocked := make(chan error, 1)
	go func() { blocked <- b.Wait(octx, "cloud/kimi-k3", 0) }()
	select {
	case <-blocked:
		t.Fatal("override cap not applied to cloud/*")
	case <-time.After(20 * time.Millisecond):
	}

	// "deepseek-flash" has no override ⇒ global cap 2: two admits.
	for i := 0; i < 2; i++ {
		if err := b.Wait(ctx, "deepseek-flash", 0); err != nil {
			t.Fatalf("global-axis provider blocked at %d: %v", i, err)
		}
	}
	// Release the parked cloud waiter so the goroutine exits cleanly.
	ocancel()
	<-blocked
}

// TestLongestProviderPrefixWins: overlapping override keys resolve most-specific.
func TestLongestProviderPrefixWins(t *testing.T) {
	b, _ := newTestBucket(Config{
		Global: Limits{RequestsPerMinute: 5},
		Providers: map[string]Limits{
			"cloud":      {RequestsPerMinute: 3},
			"cloud/kimi": {RequestsPerMinute: 1},
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// cloud/kimi-k3 hits the length-1 override: 1 admit then block.
	if err := b.Wait(context.Background(), "cloud/kimi-k3", 0); err != nil {
		t.Fatal(err)
	}
	blocked := make(chan error, 1)
	go func() { blocked <- b.Wait(ctx, "cloud/kimi-k3", 0) }()
	select {
	case <-blocked:
		t.Fatal("longest-prefix override not selected")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	<-blocked
}

// TestProviderKeyBoundaryMatch asserts a provider key resolves a model ID only
// at a segment boundary (N-1): an exact match, or a prefix followed by one of
// "/-_.:". A key must NOT bleed into an unrelated model that merely shares its
// leading characters — "deepseek" resolves "deepseek-flash" and "deepseek/x"
// but not "deepseek2-chat".
func TestProviderKeyBoundaryMatch(t *testing.T) {
	b, _ := newTestBucket(Config{
		Providers: map[string]Limits{"deepseek": {RequestsPerMinute: 6}},
	})
	cases := []struct {
		provider string
		want     string
	}{
		{"deepseek", "deepseek"},       // exact
		{"deepseek-flash", "deepseek"}, // '-' boundary
		{"deepseek/x", "deepseek"},     // '/' boundary
		{"deepseek_v2", "deepseek"},    // '_' boundary
		{"deepseek.chat", "deepseek"},  // '.' boundary
		{"deepseek:1", "deepseek"},     // ':' boundary
		{"deepseek2-chat", ""},         // no boundary ⇒ falls through to global
		{"deepseekx", ""},              // no boundary
		{"unrelated", ""},              // no match at all
	}
	for _, tc := range cases {
		if got := b.providerKey(tc.provider); got != tc.want {
			t.Errorf("providerKey(%q) = %q, want %q", tc.provider, got, tc.want)
		}
	}
}

// TestProviderKeyEqualLengthTieIsLexical asserts equal-length override keys land
// in a deterministic (lexical-ascending) order in providerKeys rather than a
// map-iteration order, so resolution never depends on build-to-build hash seed.
func TestProviderKeyEqualLengthTieIsLexical(t *testing.T) {
	b, _ := newTestBucket(Config{
		Providers: map[string]Limits{
			"bbb": {RequestsPerMinute: 1},
			"aaa": {RequestsPerMinute: 1},
			"ccc": {RequestsPerMinute: 1},
		},
	})
	want := []string{"aaa", "bbb", "ccc"} // all equal length ⇒ lexical ascending
	if len(b.providerKeys) != len(want) {
		t.Fatalf("providerKeys = %v, want %v", b.providerKeys, want)
	}
	for i, k := range want {
		if b.providerKeys[i] != k {
			t.Errorf("providerKeys = %v, want deterministic lexical order %v", b.providerKeys, want)
			break
		}
	}
}

// TestContextCancellationUnblocksWaiter: a blocked waiter returns ctx.Err() when
// its context is cancelled, so a gated call still stops on Ctrl-C.
func TestContextCancellationUnblocksWaiter(t *testing.T) {
	b, _ := newTestBucket(Config{Global: Limits{RequestsPerMinute: 1}})
	if err := b.Wait(context.Background(), "m", 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Wait(ctx, "m", 0) }()
	select {
	case <-done:
		t.Fatal("Wait returned before cancel")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not unblock waiter")
	}
}

// TestReportChargesActualsAgainstTPM: Report applies real usage to the tpm axis,
// so a big turn's spend delays the next request until refills repay the deficit.
func TestReportChargesActualsAgainstTPM(t *testing.T) {
	b, clk := newTestBucket(Config{Global: Limits{TokensPerMinute: 600}}) // 10 tok/s
	ctx := context.Background()

	// Wait charges 0 (no reservation); Report charges the full real 600 (delta from
	// an est of 0 is the whole actuals — the pre-estimate behavior for est=0).
	if err := b.Wait(ctx, "m", 0); err != nil {
		t.Fatal(err)
	}
	b.Report("m", 0, 400, 200) // 600 tokens, empties the bucket

	// Next request needs 100 tokens ⇒ must block until 10s accrue.
	done := make(chan error, 1)
	go func() { done <- b.Wait(ctx, "m", 100) }()
	select {
	case <-done:
		t.Fatal("admitted despite Report-charged deficit")
	case <-time.After(20 * time.Millisecond):
	}
	clk.Advance(10 * time.Second)
	b.Pump()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("did not unblock after refill repaid Report deficit")
	}
}

// TestReportReconcilesEstimateNoDoubleCharge asserts Report charges only the
// delta over the estimate Wait already took, in BOTH directions (SF-2):
//   - under-estimate (actuals > est): the shortfall is charged so the total
//     charged over Wait+Report equals the actuals, not est+actuals.
//   - over-estimate (actuals < est): nothing more is charged AND nothing is
//     refunded — the reserved estimate stands (the axis stays at max(est,actuals)).
func TestReportReconcilesEstimateNoDoubleCharge(t *testing.T) {
	t.Run("under-estimate charges the shortfall only", func(t *testing.T) {
		b, _ := newTestBucket(Config{Global: Limits{TokensPerMinute: 1000}})
		ctx := context.Background()
		// Wait reserves 100 (est). Actuals 600 ⇒ Report charges 500 (delta), NOT 600.
		// Total charged = 600; remaining should be 1000 - 600 = 400.
		if err := b.Wait(ctx, "m", 100); err != nil {
			t.Fatal(err)
		}
		b.Report("m", 100, 400, 200) // est 100, actuals 600 ⇒ delta 500
		if got := b.remainingTPM("m"); got != 400 {
			t.Errorf("remaining tpm = %d, want 400 (charged actuals 600 once, not est+actuals)", got)
		}
	})
	t.Run("over-estimate charges and refunds nothing", func(t *testing.T) {
		b, _ := newTestBucket(Config{Global: Limits{TokensPerMinute: 1000}})
		ctx := context.Background()
		// Wait reserves 300 (est). Actuals 100 < est ⇒ Report is a no-op: no further
		// charge and NO refund. Remaining stays at 1000 - 300 = 700 (the reservation
		// stands; the over-estimate is not returned to the bucket).
		if err := b.Wait(ctx, "m", 300); err != nil {
			t.Fatal(err)
		}
		b.Report("m", 300, 60, 40) // est 300, actuals 100 ⇒ delta -200 ⇒ no-op
		if got := b.remainingTPM("m"); got != 700 {
			t.Errorf("remaining tpm = %d, want 700 (over-estimate not refunded)", got)
		}
	})
}

// TestConcurrentWaitersRaceClean drives many concurrent waiters against a small
// bucket while advancing the clock, catching data races under -race and asserting
// forward progress (all eventually admitted).
func TestConcurrentWaitersRaceClean(t *testing.T) {
	b, clk := newTestBucket(Config{Global: Limits{RequestsPerMinute: 60, TokensPerMinute: 6000}})
	ctx := context.Background()

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_ = b.Wait(ctx, "m", 50)
		}()
	}

	// Drive refills concurrently until all waiters have progressed.
	pumpDone := make(chan struct{})
	go func() {
		for {
			select {
			case <-pumpDone:
				return
			default:
				clk.Advance(time.Second)
				b.Pump()
				time.Sleep(time.Millisecond)
			}
		}
	}()

	waited := make(chan struct{})
	go func() { wg.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("waiters did not all progress")
	}
	close(pumpDone)
}

// TestUtilizationReportsFillPerProvider: Utilization exposes each touched
// provider key's cap and live remaining, refilled to the current clock, with the
// per-provider override reflected and untouched providers absent.
func TestUtilizationReportsFillPerProvider(t *testing.T) {
	b, clk := newTestBucket(Config{
		Global:    Limits{RequestsPerMinute: 60, TokensPerMinute: 600},
		Providers: map[string]Limits{"deepseek": {RequestsPerMinute: 6}},
	})
	ctx := context.Background()

	// No axis touched yet ⇒ nothing to report.
	if u := b.Utilization(); len(u) != 0 {
		t.Fatalf("fresh bucket utilization = %+v, want empty", u)
	}

	// One global request spends 1 rpm + 300 tpm on the global ("") key.
	if err := b.Wait(ctx, "m", 300); err != nil {
		t.Fatal(err)
	}
	// One deepseek request spends 1 rpm on the deepseek key (tpm falls back to
	// the global 600 cap, and 300 was already... no — the deepseek key is its own
	// axis pair, so its tpm starts full at 600 and is charged 100 here).
	if err := b.Wait(ctx, "deepseek-flash", 100); err != nil {
		t.Fatal(err)
	}

	byKey := map[string]ProviderUtilization{}
	for _, u := range b.Utilization() {
		byKey[u.Provider] = u
	}
	if len(byKey) != 2 {
		t.Fatalf("utilization keys = %+v, want global + deepseek", byKey)
	}

	g := byKey[""]
	if g.RPMCap != 60 || g.RPMRemaining != 59 {
		t.Errorf("global rpm = %d/%d remaining, want cap 60 remaining 59", g.RPMRemaining, g.RPMCap)
	}
	if g.TPMCap != 600 || g.TPMRemaining != 300 {
		t.Errorf("global tpm = %d/%d remaining, want cap 600 remaining 300", g.TPMRemaining, g.TPMCap)
	}

	d := byKey["deepseek"]
	if d.RPMCap != 6 || d.RPMRemaining != 5 {
		t.Errorf("deepseek rpm = %d/%d remaining, want cap 6 remaining 5", d.RPMRemaining, d.RPMCap)
	}
	if d.TPMCap != 600 || d.TPMRemaining != 500 {
		t.Errorf("deepseek tpm = %d/%d remaining, want cap 600 remaining 500", d.TPMRemaining, d.TPMCap)
	}

	// Refill: advancing a full minute tops both axes back to their caps.
	clk.Advance(time.Minute)
	byKey = map[string]ProviderUtilization{}
	for _, u := range b.Utilization() {
		byKey[u.Provider] = u
	}
	if g := byKey[""]; g.RPMRemaining != 60 || g.TPMRemaining != 600 {
		t.Errorf("after a minute global = rpm %d tpm %d, want 60/600 (full)", g.RPMRemaining, g.TPMRemaining)
	}
}
