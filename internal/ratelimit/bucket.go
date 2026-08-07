// Package ratelimit provides the turn-level rate gate for the agent harness
// (change 0036, spec Acceptance 4). A Bucket smooths dispatch through the model
// gateway to a configured requests-per-minute / tokens-per-minute budget, with
// optional per-provider overrides, so N agents in tight turn loops cannot outrun
// a provider's real limits.
//
// It lives in a leaf package — importing nothing of the harness — rather than in
// internal/agent, because the gate is decoupled from the spawn tree: it reasons
// only over provider strings and token counts, holds no node/pool state, and is
// injected into internal/model's Adapter (which cannot import internal/agent).
// Keeping it standalone makes it independently testable with a fake clock and
// keeps the already-large internal/agent package unencumbered.
//
// The Bucket satisfies model.RateGate structurally (Wait + Report); it is wired
// config → Bucket → Adapter in cmd/fuse. When no rate axis is configured the
// Adapter's gate stays nil and Bucket is never constructed — the unlimited fast
// path with zero added latency.
package ratelimit

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// Limits describes one rate axis pair. A zero value on either axis means that
// axis is unlimited. RequestsPerMinute caps logical requests (the rpm axis);
// TokensPerMinute caps token spend (the tpm axis).
type Limits struct {
	RequestsPerMinute int
	TokensPerMinute   int
}

// Config configures a Bucket. Global holds the default axes; Providers overrides
// them per provider (a set override replaces the corresponding global axis for
// that provider only — an unset per-provider axis falls back to global).
type Config struct {
	Global    Limits
	Providers map[string]Limits
}

// Any reports whether any axis (global or per-provider) is set. cmd/fuse uses it
// to decide whether to construct a Bucket at all: none set ⇒ leave the adapter
// gate nil (fast path).
func (c Config) Any() bool {
	if c.Global.RequestsPerMinute > 0 || c.Global.TokensPerMinute > 0 {
		return true
	}
	for _, p := range c.Providers {
		if p.RequestsPerMinute > 0 || p.TokensPerMinute > 0 {
			return true
		}
	}
	return false
}

// axis is one continuous token bucket. capacity is the burst ceiling (equal to
// the per-minute rate: one minute of budget), refillPerSec is rate/60, and tokens
// is the current fill. A non-positive rate ⇒ unlimited (limited==false). Guarded
// by the owning Bucket's mu.
type axis struct {
	limited      bool
	capacity     float64
	refillPerSec float64
	tokens       float64
	last         time.Time
}

func newAxis(perMinute int, now time.Time) axis {
	if perMinute <= 0 {
		return axis{limited: false}
	}
	cap := float64(perMinute)
	return axis{
		limited:      true,
		capacity:     cap,
		refillPerSec: cap / 60.0,
		tokens:       cap, // start full: no artificial startup delay
		last:         now,
	}
}

// refill advances the axis to now, adding accrued tokens (clamped to capacity).
func (a *axis) refill(now time.Time) {
	if !a.limited {
		return
	}
	elapsed := now.Sub(a.last).Seconds()
	if elapsed <= 0 {
		return
	}
	a.tokens = math.Min(a.capacity, a.tokens+elapsed*a.refillPerSec)
	a.last = now
}

// providerAxes is the resolved rpm+tpm pair for one provider key.
type providerAxes struct {
	rpm axis
	tpm axis
}

// Bucket is the concurrent-safe token-bucket rate gate. It resolves a provider
// string to its axes (per-provider override replacing the matching global axis),
// blocks Wait until the request + token estimate fit, and reconciles the estimate
// against reported usage in Report. All time comes from clock so tests drive it
// with a fake clock and no real sleeps.
type Bucket struct {
	clock func() time.Time
	cfg   Config

	// providerKeys are the configured provider names, longest first, so provider
	// resolution prefers the most specific prefix match (e.g. "cloud/kimi" over
	// "cloud"). Immutable after construction.
	providerKeys []string

	mu sync.Mutex
	// axes holds the resolved, live axes per provider key, created lazily on first
	// use. The empty key "" holds the pure-global axes (no override applies).
	axes map[string]*providerAxes
	// wake is closed to broadcast a refill/refund to all blocked waiters; a new
	// channel replaces it each broadcast. Guarded by mu.
	wake chan struct{}

	// pollInterval bounds how long a blocked waiter sleeps before re-checking the
	// bucket on its own (belt-and-suspenders against a missed wake, and the only
	// thing that advances a real-clock waiter). Tests set it to 0 and drive
	// re-checks with Pump after advancing the fake clock, so no real sleep runs.
	pollInterval time.Duration
}

// New builds a Bucket. clock defaults to time.Now when nil. pollInterval is the
// real-time re-check cadence for blocked waiters (default 50ms); a Bucket used
// in tests is built with WithClock and Pump instead and never sleeps.
func New(cfg Config, clock func() time.Time) *Bucket {
	if clock == nil {
		clock = time.Now
	}
	keys := make([]string, 0, len(cfg.Providers))
	for k := range cfg.Providers {
		keys = append(keys, k)
	}
	// Longest key first ⇒ most-specific prefix wins in providerKey; equal-length
	// keys break lexically so resolution is deterministic (never map-order
	// dependent) when two configured keys are the same length.
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	return &Bucket{
		clock:        clock,
		cfg:          cfg,
		providerKeys: keys,
		axes:         map[string]*providerAxes{},
		wake:         make(chan struct{}),
		pollInterval: 50 * time.Millisecond,
	}
}

// SetPollInterval overrides the blocked-waiter re-check cadence. Tests set 0 to
// disable real-time polling and drive re-checks deterministically via Pump.
func (b *Bucket) SetPollInterval(d time.Duration) {
	b.mu.Lock()
	b.pollInterval = d
	b.mu.Unlock()
}

// keyBoundaries are the characters that may follow a matched provider key: a key
// matches a model ID only at a segment boundary, so "deepseek" matches
// "deepseek-flash" and "deepseek/x" but NOT "deepseek2-chat". Model IDs reaching
// the gate segment on exactly these separators ("cloud/kimi-k3", "deepseek-flash").
const keyBoundaries = "/-_.:"

// matchesKey reports whether key resolves provider: an exact match, or a prefix
// followed by a segment separator (keyBoundaries). Requiring the boundary stops
// a key from bleeding into an unrelated model that merely shares its leading
// characters (e.g. "deepseek" must not swallow "deepseek2-chat").
func matchesKey(provider, key string) bool {
	if !strings.HasPrefix(provider, key) {
		return false
	}
	if len(provider) == len(key) {
		return true // exact match
	}
	return strings.IndexByte(keyBoundaries, provider[len(key)]) >= 0
}

// providerKey resolves a request's provider string to the configured override
// key whose name prefixes it at a segment boundary (longest match, ties broken
// lexically by the providerKeys ordering), or "" for the pure-global axes. The
// model IDs reaching the gate look like "cloud/kimi-k3" or "deepseek-flash"; a
// provider override keyed "cloud" or "deepseek" matches by leading segment,
// which is how the spec's `providers: { deepseek: {...} }` maps onto them. The
// boundary requirement means "deepseek" resolves "deepseek-flash" and
// "deepseek/x" but not "deepseek2-chat".
func (b *Bucket) providerKey(provider string) string {
	for _, k := range b.providerKeys {
		if matchesKey(provider, k) {
			return k
		}
	}
	return ""
}

// axesFor returns the live axes for a provider, creating them on first use. The
// resolved rpm/tpm each take the per-provider override when set, else the global
// axis. Caller holds mu.
func (b *Bucket) axesFor(provider string) *providerAxes {
	key := b.providerKey(provider)
	if pa, ok := b.axes[key]; ok {
		return pa
	}
	now := b.clock()
	rpm, tpm := b.cfg.Global.RequestsPerMinute, b.cfg.Global.TokensPerMinute
	if ov, ok := b.cfg.Providers[key]; ok {
		if ov.RequestsPerMinute > 0 {
			rpm = ov.RequestsPerMinute
		}
		if ov.TokensPerMinute > 0 {
			tpm = ov.TokensPerMinute
		}
	}
	pa := &providerAxes{rpm: newAxis(rpm, now), tpm: newAxis(tpm, now)}
	b.axes[key] = pa
	return pa
}

// broadcast wakes all blocked waiters to re-check the bucket. Caller holds mu.
func (b *Bucket) broadcast() {
	close(b.wake)
	b.wake = make(chan struct{})
}

// Pump refills every live axis to the current clock and wakes blocked waiters.
// Tests call it after advancing the fake clock to let waiters re-check
// deterministically, standing in for the real-time poll that never fires under a
// fake clock.
func (b *Bucket) Pump() {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock()
	for _, pa := range b.axes {
		pa.rpm.refill(now)
		pa.tpm.refill(now)
	}
	b.broadcast()
}

// tryTake refills the resolved axes and, if both the one request and estTokens
// fit, consumes them and returns true. estTokens<0 is treated as 0 (never a
// negative charge). Caller holds mu.
func (b *Bucket) tryTake(provider string, estTokens int) bool {
	if estTokens < 0 {
		estTokens = 0
	}
	pa := b.axesFor(provider)
	now := b.clock()
	pa.rpm.refill(now)
	pa.tpm.refill(now)

	if pa.rpm.limited && pa.rpm.tokens < 1 {
		return false
	}
	if pa.tpm.limited && pa.tpm.tokens < float64(estTokens) {
		return false
	}
	if pa.rpm.limited {
		pa.rpm.tokens -= 1
	}
	if pa.tpm.limited {
		pa.tpm.tokens -= float64(estTokens)
	}
	return true
}

// Wait blocks until one request (charging estTokens against the tpm axis) fits
// the resolved provider budget, or ctx is cancelled. It is the model.RateGate
// Wait method. A waiter unblocks as the bucket refills; ctx cancellation returns
// ctx.Err() immediately so a gated call still stops on Ctrl-C (learning #12).
//
// Estimate semantics: callers pass what they cheaply know up front — the adapter
// passes 0 (it has no accurate token count before dispatch), so the tpm charge is
// applied almost entirely by Report after the gateway reports actual usage. A
// caller that can estimate should pass a positive estTokens to reserve ahead.
func (b *Bucket) Wait(ctx context.Context, provider string, estTokens int) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		b.mu.Lock()
		if b.tryTake(provider, estTokens) {
			b.mu.Unlock()
			return nil
		}
		wake := b.wake
		poll := b.pollInterval
		b.mu.Unlock()

		// Block until a refill/refund broadcast, the poll cadence elapses, or ctx
		// ends. With a fake clock (poll==0) only the broadcast (Pump) or ctx wakes
		// us — no real time passes.
		if poll > 0 {
			timer := time.NewTimer(poll)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-wake:
				timer.Stop()
			case <-timer.C:
			}
		} else {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-wake:
			}
		}
	}
}

// ProviderUtilization is a race-safe, point-in-time view of one provider key's
// live rate-gate fill for the observability surface (change 0036, Task 7). It is
// reported per resolved provider key ("" for the pure-global axes). A cap of 0
// means that axis is unlimited (unset); the display omits an unlimited axis.
// Remaining is the current token fill rounded down to a whole unit.
type ProviderUtilization struct {
	// Provider is the resolved provider key these axes belong to ("" = global).
	Provider string
	// RPMCap / RPMRemaining describe the requests-per-minute axis (cap 0 =
	// unlimited). Remaining is the whole requests currently available.
	RPMCap       int
	RPMRemaining int
	// TPMCap / TPMRemaining describe the tokens-per-minute axis (cap 0 =
	// unlimited). Remaining is the whole tokens currently available.
	TPMCap       int
	TPMRemaining int
}

// Utilization returns a race-safe snapshot of every provider key's live axis
// fill for the observability surface (change 0036, Task 7). Only provider keys
// that have been touched (an axis lazily created on first Wait/Report) appear —
// an untouched provider has no live fill to report. Each returned axis is
// refilled to the current clock first, so Remaining reflects "right now". A nil
// Bucket returns nil. The result is a pure copy; the caller needs no further
// locking.
func (b *Bucket) Utilization() []ProviderUtilization {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock()
	out := make([]ProviderUtilization, 0, len(b.axes))
	for key, pa := range b.axes {
		pa.rpm.refill(now)
		pa.tpm.refill(now)
		u := ProviderUtilization{Provider: key}
		if pa.rpm.limited {
			u.RPMCap = int(pa.rpm.capacity)
			if r := int(math.Floor(pa.rpm.tokens)); r > 0 {
				u.RPMRemaining = r
			}
		}
		if pa.tpm.limited {
			u.TPMCap = int(pa.tpm.capacity)
			if r := int(math.Floor(pa.tpm.tokens)); r > 0 {
				u.TPMRemaining = r
			}
		}
		out = append(out, u)
	}
	return out
}

// Report reconciles the pre-dispatch estimate with the gateway's actual usage.
// estTokens is the same estimate the matching Wait already charged against the
// tpm axis; Report charges only the DELTA so that estimate is never double-
// counted. The rule is one-directional (chosen for simplicity and to keep the
// axis ungameable):
//
//	delta = (inTokens + outTokens) - estTokens
//	  delta > 0 ⇒ the turn spent MORE than reserved: charge the shortfall now.
//	  delta ≤ 0 ⇒ the turn spent AT MOST what was reserved: charge nothing and
//	             refund nothing — the over-estimate is NOT returned to the bucket.
//
// Not refunding an over-estimate is deliberate: a refund would let a caller game
// the axis by over-reserving then reporting a tiny actual to inflate the fill.
// The floor at 0 means total charged over Wait+Report is max(estTokens, actuals)
// — always ≥ actuals, never below the reservation. A caller that passed
// estTokens=0 to Wait (no reservation) is charged the full actuals here, exactly
// the pre-estimate behavior. Report is the model.RateGate Report method.
func (b *Bucket) Report(provider string, estTokens, inTokens, outTokens int) {
	if estTokens < 0 {
		estTokens = 0
	}
	delta := inTokens + outTokens - estTokens
	if delta <= 0 {
		return // actuals within the already-charged estimate; no further charge, no refund
	}
	b.mu.Lock()
	pa := b.axesFor(provider)
	if pa.tpm.limited {
		pa.tpm.refill(b.clock())
		pa.tpm.tokens -= float64(delta)
	}
	// Only ever a tighten (delta > 0 here); still broadcast so a waiter re-evaluates
	// promptly if this resolved to a no-op axis.
	b.broadcast()
	b.mu.Unlock()
}
