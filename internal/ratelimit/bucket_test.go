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

	// Wait charges 0 (adapter's estimate); Report charges the real 600.
	if err := b.Wait(ctx, "m", 0); err != nil {
		t.Fatal(err)
	}
	b.Report("m", 400, 200) // 600 tokens, empties the bucket

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
