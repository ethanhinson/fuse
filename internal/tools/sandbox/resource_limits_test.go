package sandbox

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
)

// i64 and dur are pointer constructors for building a Limits/Concurrency by hand
// in a test.
func i64(v int64) *int64                 { return &v }
func str(v string) *string               { return &v }
func dur(v time.Duration) *time.Duration { return &v }

// argvFor runs one Exec through a handler built with the given limits and
// returns the recorded container-run argv (pre-pull excluded).
func argvFor(t *testing.T, limits Limits) []string {
	t.Helper()
	rec := &recordingRun{}
	cfg := DefaultConfig()
	h := newTestHandler(t, cfg, rec)
	h.limits = limits // as withLimits would set at construction

	r, err := h.Acquire(context.Background(), loopauth.Principal{}, Env{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := r.Exec(context.Background(), "true", ""); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	return rec.args
}

// --- Argv caps, table-driven -------------------------------------------------

func TestArgvAllCapsPresent(t *testing.T) {
	argv := argvFor(t, Limits{
		MemoryBytes: i64(2 << 30),
		CPUs:        str("2.0"),
		Pids:        i64(512),
		NoFile:      i64(4096),
		FsizeBytes:  i64(1 << 30),
	})

	wantFlag(t, argv, "--memory", "2147483648")
	wantFlag(t, argv, "--memory-swap", "2147483648") // pinned equal
	wantFlag(t, argv, "--cpus", "2.0")
	wantFlag(t, argv, "--pids-limit", "512")
	// --ulimit appears twice; assert both values are present.
	if !containsPair(argv, "--ulimit", "nofile=4096:4096") {
		t.Errorf("argv missing --ulimit nofile=4096:4096: %#v", argv)
	}
	if !containsPair(argv, "--ulimit", "fsize=1073741824") {
		t.Errorf("argv missing --ulimit fsize=<bytes>: %#v", argv)
	}
	// The caps must sit AFTER --rm -i and BEFORE the image / -v mount marker.
	if idxOf(argv, "--memory") > idxOf(argv, "-v") {
		t.Errorf("caps rendered after the mount flag: %#v", argv)
	}
	// --pull=never is always present.
	if !contains(argv, "--pull=never") {
		t.Errorf("argv missing --pull=never: %#v", argv)
	}
}

// Each limit individually unset ⇒ THAT flag absent, the others unchanged.
func TestArgvEachCapIndividuallyUnset(t *testing.T) {
	full := Limits{
		MemoryBytes: i64(2 << 30),
		CPUs:        str("2.0"),
		Pids:        i64(512),
		NoFile:      i64(4096),
		FsizeBytes:  i64(1 << 30),
	}

	t.Run("no memory", func(t *testing.T) {
		l := full
		l.MemoryBytes = nil
		argv := argvFor(t, l)
		if contains(argv, "--memory") || contains(argv, "--memory-swap") {
			t.Errorf("--memory/--memory-swap present though unset: %#v", argv)
		}
		wantFlag(t, argv, "--cpus", "2.0") // others unchanged
	})
	t.Run("no cpus", func(t *testing.T) {
		l := full
		l.CPUs = nil
		argv := argvFor(t, l)
		if contains(argv, "--cpus") {
			t.Errorf("--cpus present though unset: %#v", argv)
		}
		wantFlag(t, argv, "--memory", "2147483648")
	})
	t.Run("no pids", func(t *testing.T) {
		l := full
		l.Pids = nil
		argv := argvFor(t, l)
		if contains(argv, "--pids-limit") {
			t.Errorf("--pids-limit present though unset: %#v", argv)
		}
	})
	t.Run("no fsize keeps nofile", func(t *testing.T) {
		l := full
		l.FsizeBytes = nil
		argv := argvFor(t, l)
		if containsPair(argv, "--ulimit", "fsize=1073741824") {
			t.Errorf("fsize ulimit present though unset: %#v", argv)
		}
		if !containsPair(argv, "--ulimit", "nofile=4096:4096") {
			t.Errorf("nofile ulimit should remain: %#v", argv)
		}
	})
}

// All caps unset ⇒ argv is the #0063 baseline plus ONLY --pull=never. The
// "we did not change the uncontained case" guard.
func TestArgvNoCapsIsBaselinePlusPullNever(t *testing.T) {
	argv := argvFor(t, Limits{})

	for _, flag := range []string{"--memory", "--memory-swap", "--cpus", "--pids-limit", "--ulimit"} {
		if contains(argv, flag) {
			t.Errorf("uncapped argv carries %s: %#v", flag, argv)
		}
	}
	if !contains(argv, "--pull=never") {
		t.Errorf("argv missing --pull=never: %#v", argv)
	}
}

// --memory-swap is present and equal WHENEVER --memory is.
func TestArgvMemorySwapPinnedEqual(t *testing.T) {
	argv := argvFor(t, Limits{MemoryBytes: i64(512 << 20)})
	m, _ := argValue(argv, "--memory")
	ms, ok := argValue(argv, "--memory-swap")
	if !ok || ms != m {
		t.Errorf("--memory-swap = %q, want equal to --memory %q", ms, m)
	}
}

// --- Trust boundary: caps are not model-reachable ----------------------------

// A working_dir, a command string, and an environment variable that all name
// plausible cap values change nothing about argv. The regression guard that caps
// are not model-reachable.
func TestArgvCapsNotModelReachable(t *testing.T) {
	rec := &recordingRun{}
	// The handler is capped by TRUSTED config only.
	h := newTestHandler(t, DefaultConfig(), rec)
	h.limits = Limits{MemoryBytes: i64(2 << 30), Pids: i64(512)}

	// The model supplies an env var that looks like a cap and a command that
	// names one. None of it may alter argv's caps.
	env := Env{Allow: map[string]string{"MEMORY": "9999g", "PIDS_LIMIT": "1"}}
	r, err := h.Acquire(context.Background(), loopauth.Principal{}, env)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := r.Exec(context.Background(), "--memory 9999g --pids-limit 999999", ""); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	wantFlag(t, rec.args, "--memory", "2147483648") // trusted value, unchanged
	wantFlag(t, rec.args, "--pids-limit", "512")
	if contains(rec.args, "9999g") {
		t.Errorf("a model-supplied value leaked into a cap: %#v", rec.args)
	}
}

// --- Posture defaults --------------------------------------------------------

// Unset + hosted ⇒ every hosted default applied; argv carries every cap flag.
func TestPostureHostedFillsCapsAndArgv(t *testing.T) {
	cli := &recordingExec{}
	svc, err := NewService(DefaultConfig(),
		WithHostedPosture(true),
		WithTrustedRoot(trustedTestRoot(t)),
		withContainerLookPath(fakeLookPath("docker")),
		withContainerExec(cli.run),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	l := svc.Limits()
	if l.MemoryBytes == nil || *l.MemoryBytes != defaultHostedMemoryBytes {
		t.Errorf("hosted memory = %v, want %d", l.MemoryBytes, defaultHostedMemoryBytes)
	}
	if l.CPUs == nil || *l.CPUs != defaultHostedCPUs {
		t.Errorf("hosted cpus = %v, want %q", l.CPUs, defaultHostedCPUs)
	}
	if l.Pids == nil || *l.Pids != defaultHostedPids {
		t.Errorf("hosted pids = %v", l.Pids)
	}
	if l.PullTimeout == nil || *l.PullTimeout != DefaultPullTimeout {
		t.Errorf("pull_timeout = %v, want %v", l.PullTimeout, DefaultPullTimeout)
	}

	runner, err := svc.Acquire(context.Background(), testPrincipal())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := runner.Exec(context.Background(), "true", ""); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	argv := cli.runCalls()[0]
	for _, flag := range []string{"--memory", "--cpus", "--pids-limit", "--ulimit"} {
		if !contains(argv, flag) {
			t.Errorf("hosted argv missing %s: %#v", flag, argv)
		}
	}
}

// Unset + local ⇒ NO cap flags at all, but the concurrency backstop and pull
// timeout ARE applied in both postures.
func TestPostureLocalEmitsNoCapsButKeepsConcurrency(t *testing.T) {
	cli := &recordingExec{}
	svc, err := NewService(DefaultConfig(),
		WithHostedPosture(false),
		WithTrustedRoot(trustedTestRoot(t)),
		withContainerLookPath(fakeLookPath("docker")),
		withContainerExec(cli.run),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	l := svc.Limits()
	if l.MemoryBytes != nil || l.CPUs != nil || l.Pids != nil || l.NoFile != nil || l.FsizeBytes != nil {
		t.Errorf("local posture applied caps: %+v", l)
	}
	// The pull timeout applies in BOTH postures.
	if l.PullTimeout == nil || *l.PullTimeout != DefaultPullTimeout {
		t.Errorf("local pull_timeout = %v, want %v (applies in both postures)", l.PullTimeout, DefaultPullTimeout)
	}
	// The concurrency backstop applies in BOTH postures (the gate is non-nil and
	// bounded).
	if svc.gate == nil {
		t.Fatal("gate is nil under local posture")
	}
	if got := cap(svc.gate.global.tokens); int64(got) != defaultMaxInflight {
		t.Errorf("global backstop = %d, want %d in both postures", got, defaultMaxInflight)
	}

	runner, err := svc.Acquire(context.Background(), testPrincipal())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := runner.Exec(context.Background(), "true", ""); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	argv := cli.runCalls()[0]
	for _, flag := range []string{"--memory", "--cpus", "--pids-limit", "--ulimit"} {
		if contains(argv, flag) {
			t.Errorf("local argv carries cap flag %s: %#v", flag, argv)
		}
	}
	if !contains(argv, "--pull=never") {
		t.Errorf("local argv missing --pull=never: %#v", argv)
	}
}

// Explicit config is honoured identically in both postures — posture only fills
// UNSET fields.
func TestPostureExplicitConfigHonouredInBothPostures(t *testing.T) {
	for _, hosted := range []bool{true, false} {
		cfg := DefaultConfig()
		cfg.Limits = Limits{MemoryBytes: i64(512 << 20)} // an explicit small cap

		svc, err := NewService(cfg,
			WithHostedPosture(hosted),
			WithTrustedRoot(trustedTestRoot(t)),
			withContainerLookPath(fakeLookPath("docker")),
		)
		if err != nil {
			t.Fatalf("NewService(hosted=%v): %v", hosted, err)
		}
		if got := svc.Limits().MemoryBytes; got == nil || *got != 512<<20 {
			t.Errorf("hosted=%v: explicit memory not honoured: %v", hosted, got)
		}
	}
}

// --- Pre-pull ----------------------------------------------------------------

// pullControl is an execRunner whose pull blocks until released, so a test can
// prove the pull is bounded independent of the caller's deadline and is
// single-flight.
type pullControl struct {
	mu       sync.Mutex
	pulls    int
	release  chan struct{}
	pullErr  error
	blockAll bool // when true, the pull blocks on release
}

func newPullControl() *pullControl {
	return &pullControl{release: make(chan struct{})}
}

func (p *pullControl) run(ctx context.Context, name string, args ...string) ([]byte, int, error) {
	if len(args) > 0 && args[0] == "pull" {
		p.mu.Lock()
		p.pulls++
		block := p.blockAll
		err := p.pullErr
		p.mu.Unlock()
		if block {
			select {
			case <-p.release:
			case <-ctx.Done():
				return nil, -1, ctx.Err()
			}
		}
		return nil, 0, err
	}
	return nil, 0, nil
}

func (p *pullControl) pullCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pulls
}

// A failed pull is an Acquire failure and is RETRIED on a later Acquire rather
// than cached as permanent.
func TestPrePullFailureIsAcquireErrorAndRetried(t *testing.T) {
	pc := newPullControl()
	pc.pullErr = errors.New("registry unreachable")
	h := newTestHandler(t, DefaultConfig(), &recordingRun{})
	h.run = pc.run

	_, err := h.Acquire(context.Background(), loopauth.Principal{}, Env{})
	if err == nil {
		t.Fatal("want an Acquire error when the pull fails")
	}
	if !strings.Contains(err.Error(), "pull") {
		t.Errorf("error %q does not mention the pull", err)
	}

	// A later Acquire retries the pull (not cached as permanent). This time it
	// succeeds.
	pc.mu.Lock()
	pc.pullErr = nil
	pc.mu.Unlock()
	if _, err := h.Acquire(context.Background(), loopauth.Principal{}, Env{}); err != nil {
		t.Fatalf("retry Acquire: %v", err)
	}
	if pc.pullCount() < 2 {
		t.Errorf("pull count = %d, want the failed pull to have been retried", pc.pullCount())
	}
}

// Concurrent Acquires trigger EXACTLY ONE pull invocation.
func TestPrePullIsSingleFlight(t *testing.T) {
	pc := newPullControl()
	pc.blockAll = true
	h := newTestHandler(t, DefaultConfig(), &recordingRun{})
	h.run = pc.run

	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, _ = h.Acquire(context.Background(), loopauth.Principal{}, Env{})
		}()
	}

	// Give the goroutines time to all join the single in-flight pull, then
	// release it.
	waitFor(t, "first pull to start", func() bool { return pc.pullCount() >= 1 })
	time.Sleep(20 * time.Millisecond) // let stragglers join
	close(pc.release)
	wg.Wait()

	if got := pc.pullCount(); got != 1 {
		t.Errorf("pull invocations = %d, want exactly 1 (single-flight)", got)
	}
}

// A caller with a SHORTER deadline than the pull returns a timeout while the
// pull is still running; the next Acquire finds it complete.
func TestPrePullCallerDeadlineDoesNotCancelPull(t *testing.T) {
	pc := newPullControl()
	pc.blockAll = true
	h := newTestHandler(t, DefaultConfig(), &recordingRun{})
	h.run = pc.run

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := h.Acquire(ctx, loopauth.Principal{}, Env{})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want a deadline error for the short caller, got %v", err)
	}

	// The pull is still in flight (not cancelled by the short caller). Release it
	// and a fresh Acquire finds the image warm with no NEW pull.
	close(pc.release)
	waitFor(t, "pull to complete", func() bool {
		h.pullMu.Lock()
		done := h.pullDone
		h.pullMu.Unlock()
		return done
	})
	before := pc.pullCount()
	if _, err := h.Acquire(context.Background(), loopauth.Principal{}, Env{}); err != nil {
		t.Fatalf("Acquire after warm pull: %v", err)
	}
	if pc.pullCount() != before {
		t.Errorf("a warm image was pulled again: before=%d after=%d", before, pc.pullCount())
	}
}

// SetGateHooks must tolerate a nil *Service: NewBash(nil) is a supported
// fail-closed shape, so a composition root wiring the bash tool over a nil
// Service (no substrate selected) calls SetGateHooks unconditionally. A nil
// receiver must be a no-op, never a panic (regression: the loop-server StartLoop
// path called this on a nil Service and crashed the connection).
func TestSetGateHooksNilServiceIsNoop(t *testing.T) {
	var svc *Service
	// Must not panic.
	svc.SetGateHooks(GateHooks{Queued: func(AdmissionInfo) {}, Refused: func(AdmissionInfo) {}})
}

// --- Gate/pool composition ---------------------------------------------------

// A checked-out warm Runner held across a gap consumes NO slot while idle: the
// gate is acquired around Exec, never around checkout. If it were around
// checkout, an idle held Runner would starve a single-slot gate.
func TestPoolIdleCheckoutConsumesNoSlot(t *testing.T) {
	clock := newFakeClock()
	src := newFakeSource(clock)
	src.setGate(newGate(gateCfg(1, 1, 8, time.Hour))) // ONE slot
	pool := NewPool(src, withPoolClock(clock.now), withPoolReapInterval(time.Hour))
	defer func() { _ = pool.Close(context.Background()) }()

	p := principal("acme", "alice")

	// Check out and HOLD a runner without executing — an idle checkout.
	r1, err := pool.Acquire(context.Background(), p)
	if err != nil {
		t.Fatalf("Acquire r1: %v", err)
	}

	// A DIFFERENT principal executes. With a single global slot, this proves the
	// idle checkout above is holding no slot: the Exec admits instantly.
	r2, err := pool.Acquire(context.Background(), principal("globex", "bob"))
	if err != nil {
		t.Fatalf("Acquire r2: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	out, err := r2.Exec(ctx, "true", "")
	if err != nil {
		t.Fatalf("r2 Exec blocked though r1 is only idle-held: %v (out=%+v)", err, out)
	}
	_ = pool.Release(context.Background(), principal("globex", "bob"), r2, CauseReleased)
	_ = pool.Release(context.Background(), p, r1, CauseReleased)
}

// Two Execs on the SAME Runner consume one slot at a time and never deadlock: a
// ticket's lifetime is strictly inside one Exec, released before the next.
func TestPoolSequentialExecsOnSameRunnerReleaseSlot(t *testing.T) {
	clock := newFakeClock()
	src := newFakeSource(clock)
	src.setGate(newGate(gateCfg(1, 1, 8, time.Hour))) // ONE slot
	pool := NewPool(src, withPoolClock(clock.now), withPoolReapInterval(time.Hour))
	defer func() { _ = pool.Close(context.Background()) }()

	r, err := pool.Acquire(context.Background(), principal("acme", "alice"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	for i := 0; i < 3; i++ {
		if _, err := r.Exec(ctx, "true", ""); err != nil {
			t.Fatalf("Exec %d deadlocked or errored (a ticket outlived its Exec?): %v", i, err)
		}
	}
	_ = pool.Release(context.Background(), principal("acme", "alice"), r, CauseReleased)
}

// A pool Close with a checkout BLOCKED in Admit does not hang. The single gate
// slot is held directly (saturating the gate), so a pooled Exec parks in Admit;
// Close must still return promptly, because it holds no gate ticket and the
// parked Exec unblocks via its own context, not via Close.
func TestPoolCloseWithBlockedAdmitDoesNotHang(t *testing.T) {
	clock := newFakeClock()
	src := newFakeSource(clock)
	gate := newGate(gateCfg(1, 1, 8, time.Hour)) // ONE slot
	src.setGate(gate)
	pool := NewPool(src, withPoolClock(clock.now), withPoolReapInterval(time.Hour))

	// Saturate the single slot directly, outside the pool.
	hold, _, err := gate.Admit(context.Background(), event.TenantID("acme"))
	if err != nil {
		t.Fatalf("hold gate slot: %v", err)
	}

	// A pooled checkout whose Exec parks in Admit (no slot free).
	r, err := pool.Acquire(context.Background(), principal("acme", "alice"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	bCtx, bCancel := context.WithCancel(context.Background())
	parked := make(chan struct{})
	go func() {
		close(parked)
		_, _ = r.Exec(bCtx, "true", "") // parks in Admit
	}()
	<-parked
	waitFor(t, "pooled Exec to park in Admit", func() bool {
		gate.mu.Lock()
		w := gate.waiters
		gate.mu.Unlock()
		return w >= 1
	})

	// Close must return promptly despite the parked Exec.
	closed := make(chan error, 1)
	go func() { closed <- pool.Close(context.Background()) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("pool.Close hung with a checkout blocked in Admit")
	}

	// Cleanup: unblock the parked Exec and drop the held slot.
	bCancel()
	hold.Release()
}

// --- small argv helpers ------------------------------------------------------

func contains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func containsPair(args []string, flag, val string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == val {
			return true
		}
	}
	return false
}

func idxOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}
