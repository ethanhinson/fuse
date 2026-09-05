package sandbox

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/loopauth"
)

// TestClassifyExitDoesNotFabricate is the anti-fabrication guard for the whole
// health emitter. The single most damaging failure mode here is not a missing
// signal but a manufactured one: fuse_sandbox_unhealthy_total quietly becoming a
// count of user command failures, which an operator would then trust as a
// substrate signal.
//
// Every ordinary exit status a command can legitimately produce must classify as
// NOT a health event.
func TestClassifyExitDoesNotFabricate(t *testing.T) {
	tests := []struct {
		name       string
		exitCode   int
		startErr   error
		timedOut   bool
		wantReason HealthReason
		wantEvent  bool
	}{
		// --- the negative half: ordinary command results, never health events ---
		{name: "success", exitCode: 0},
		{name: "grep found nothing", exitCode: 1},
		{name: "misuse of a shell builtin", exitCode: 2},
		{name: "command not found", exitCode: 127},
		{name: "a test suite's own failure code", exitCode: 42},
		// 128 exactly is "invalid exit argument", still the command's own status.
		{name: "invalid exit argument", exitCode: 128},
		// A deadline kill arrives as a signal death but is the CALLER's bound
		// firing, already reported as Output.TimedOut. Counting it would make
		// every short-timeout bash call a health incident.
		{name: "deadline kill is the caller's bound", exitCode: 137, timedOut: true},
		{name: "deadline kill with a start error", startErr: errors.New("killed"), timedOut: true},

		// --- the positive half: genuine substrate failure ---
		{name: "oom kill", exitCode: 137, wantReason: HealthOOM, wantEvent: true},
		{name: "sigterm death", exitCode: 143, wantReason: HealthRuntimeExit, wantEvent: true},
		{name: "sigsegv death", exitCode: 139, wantReason: HealthRuntimeExit, wantEvent: true},
		{name: "runtime could not start the command", startErr: errors.New("no daemon"), exitCode: -1, wantReason: HealthRuntimeExit, wantEvent: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reason, got := classifyExit(tc.exitCode, tc.startErr, tc.timedOut)
			if got != tc.wantEvent {
				t.Fatalf("classifyExit(%d, %v, %v) event = %v, want %v", tc.exitCode, tc.startErr, tc.timedOut, got, tc.wantEvent)
			}
			if reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}

// recordHealth collects fired HealthInfos for assertion. Locked because Exec and
// Acquire may fire from any goroutine the caller is on.
type recordHealth struct {
	mu   sync.Mutex
	seen []HealthInfo
}

func (r *recordHealth) hooks() HealthHooks {
	return HealthHooks{Unhealthy: func(i HealthInfo) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.seen = append(r.seen, i)
	}}
}

func (r *recordHealth) reasons() []HealthReason {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]HealthReason, 0, len(r.seen))
	for _, i := range r.seen {
		out = append(out, i.Reason)
	}
	return out
}

// TestPoolReportsAcquireFailedAsHealth pins the acquire_failed emission at the
// one site that sees a cold start whole, and pins that a caller-deadline
// cancellation is NOT one — that is the caller's bound firing, not the substrate.
func TestPoolReportsAcquireFailedAsHealth(t *testing.T) {
	clock := newFakeClock()
	src := newFakeSource(clock)
	rec := &recordHealth{}
	src.setHealth(rec.hooks())

	pool := NewPool(src)
	defer func() { _ = pool.Close(context.Background()) }()

	p := loopauth.Principal{Tenant: "t", Subject: "s"}

	// A substrate failure: a cold start that produced no Runner.
	src.setErr(errors.New("no daemon"))
	if _, err := pool.Acquire(context.Background(), p); err == nil {
		t.Fatal("Acquire succeeded, want the injected failure")
	}
	if got := rec.reasons(); len(got) != 1 || got[0] != HealthAcquireFailed {
		t.Fatalf("reasons = %v, want exactly [acquire_failed]", got)
	}

	// The caller's own deadline expiring must add nothing.
	src.setErr(context.DeadlineExceeded)
	if _, err := pool.Acquire(context.Background(), p); err == nil {
		t.Fatal("Acquire succeeded, want the injected failure")
	}
	if got := rec.reasons(); len(got) != 1 {
		t.Fatalf("reasons = %v, want a deadline expiry to emit NOTHING further", got)
	}
}

// TestHealthHooksAreInertByDefault pins that a Service with no observer
// installed fires nothing and does not panic — the shape every binding without a
// per-loop event store runs in.
func TestHealthHooksAreInertByDefault(t *testing.T) {
	var h HealthHooks
	h.fire(HealthInfo{Reason: HealthOOM}) // must not panic

	var nilp *HealthHooks
	nilp.fire(HealthInfo{Reason: HealthOOM}) // must not panic

	var svc *Service
	svc.SetHealthHooks(h) // nil *Service is a supported shape; must not panic
}
