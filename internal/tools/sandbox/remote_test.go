package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/loopauth"
)

// remoteStub is a RemoteSubstrate double in the microvm_conformance_test.go
// spirit: it never talks to a control plane, and its whole job is to prove the
// remote seam type-checks and to let the ADAPTER's behaviour be asserted without
// a cluster.
//
// It is mutex-guarded because the adapter drives it from several goroutines at
// once — the caller's Exec, the per-Runner heartbeat, the per-handler reaper
// (learning: mutex-test-double-concurrent-provider).
type remoteStub struct {
	mu sync.Mutex

	// verifyErr, when non-nil, is what Verify reports. verifyCalls counts how
	// many times it was CALLED, which is the sticky-refusal assertion: a
	// refusal must be cached, not re-probed per Acquire.
	verifyErr   error
	verifyCalls int

	provisionErr   error
	provisionCalls int

	reapCalls []time.Duration
	// reapCount is how many orphans each Reap reports having deleted. It drives
	// the reap-event assertion: the reaper must report one event PER orphan, with
	// cause orphan.
	reapCount int

	// mount overrides what a provisioned sandbox reports as its MountRoot. Empty
	// means the default "/workspace"; the marker "-" means report the EMPTY
	// string, which is a substrate that gave the adapter no root to contain
	// against.
	mount string

	sandboxes []*remoteSandboxStub
}

// Compile-time seam conformance: the remote seam accommodates a control-plane
// substrate with no widening of Handler or Runner.
var _ RemoteSubstrate = (*remoteStub)(nil)
var _ RemoteSandbox = (*remoteSandboxStub)(nil)

func (s *remoteStub) Name() string { return "remote-stub" }

func (s *remoteStub) Verify(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.verifyCalls++
	return s.verifyErr
}

func (s *remoteStub) Provision(_ context.Context, p loopauth.Principal, spec RemoteSpec) (RemoteSandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.provisionCalls++
	if s.provisionErr != nil {
		return nil, s.provisionErr
	}
	mount := "/workspace"
	switch s.mount {
	case "":
	case "-":
		mount = ""
	default:
		mount = s.mount
	}
	sb := &remoteSandboxStub{
		id:        fmt.Sprintf("ns/pod-%d", s.provisionCalls),
		mount:     mount,
		principal: p,
		spec:      spec,
	}
	s.sandboxes = append(s.sandboxes, sb)
	return sb, nil
}

func (s *remoteStub) Reap(_ context.Context, staleAfter time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reapCalls = append(s.reapCalls, staleAfter)
	return s.reapCount, nil
}

func (s *remoteStub) stats() (verify, provision int, reaps []time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.verifyCalls, s.provisionCalls, append([]time.Duration(nil), s.reapCalls...)
}

func (s *remoteStub) only(t *testing.T) *remoteSandboxStub {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sandboxes) != 1 {
		t.Fatalf("provisioned %d sandboxes, want exactly 1", len(s.sandboxes))
	}
	return s.sandboxes[0]
}

// remoteExec records one Exec the adapter routed through to the substrate.
type remoteExec struct {
	env        []string
	cmd        string
	workingDir string
}

type remoteSandboxStub struct {
	id        string
	mount     string
	principal loopauth.Principal
	spec      RemoteSpec

	mu sync.Mutex
	// block, when non-nil, is waited on inside Exec so a test can hold an Exec
	// open across a deadline.
	block      chan struct{}
	execs      []remoteExec
	heartbeats int
	teardowns  int
	out        Output
	execErr    error
}

func (s *remoteSandboxStub) ID() string                    { return s.id }
func (s *remoteSandboxStub) MountRoot() string             { return s.mount }
func (s *remoteSandboxStub) Principal() loopauth.Principal { return s.principal }

func (s *remoteSandboxStub) Exec(ctx context.Context, env Env, cmd, workingDir string) (Output, error) {
	s.mu.Lock()
	s.execs = append(s.execs, remoteExec{env: renderEnv(env), cmd: cmd, workingDir: workingDir})
	block := s.block
	out, execErr := s.out, s.execErr
	s.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return Output{ExitCode: -1}, ctx.Err()
		}
	}
	return out, execErr
}

func (s *remoteSandboxStub) Heartbeat(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.heartbeats++
	return nil
}

func (s *remoteSandboxStub) Teardown(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.teardowns++
	return nil
}

func (s *remoteSandboxStub) observed() (execs []remoteExec, heartbeats, teardowns int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]remoteExec(nil), s.execs...), s.heartbeats, s.teardowns
}

// testRemoteHandler builds an adapter over sub with the reaper stopped, which is
// the default for tests that are not about reaping.
func testRemoteHandler(t *testing.T, sub RemoteSubstrate, cfg Config, opts ...RemoteOption) Handler {
	t.Helper()
	h, err := NewRemoteHandler(sub, cfg, opts...)
	if err != nil {
		t.Fatalf("NewRemoteHandler: %v", err)
	}
	t.Cleanup(func() { _ = h.(*remoteHandler).Close() })
	return h
}

// --- seam conformance --------------------------------------------------------

// The adapter's Runner must satisfy every seam the Pool reaches for, INCLUDING
// containerIdentified — which nothing satisfied before this change, so the
// Pool's certifyEntry/runnerContainerID paths were dead code for want of an
// implementor. These are compile-time assertions; the runtime ones are below.
func TestRemoteRunnerSatisfiesEveryPoolSeam(t *testing.T) {
	var r any = &remoteRunner{}

	for name, ok := range map[string]bool{
		"Runner":              func() bool { _, ok := r.(Runner); return ok }(),
		"EnvResetter":         func() bool { _, ok := r.(EnvResetter); return ok }(),
		"principalScoped":     func() bool { _, ok := r.(principalScoped); return ok }(),
		"mountScoped":         func() bool { _, ok := r.(mountScoped); return ok }(),
		"containerIdentified": func() bool { _, ok := r.(containerIdentified); return ok }(),
	} {
		if !ok {
			t.Errorf("*remoteRunner does not satisfy %s", name)
		}
	}
}

// The handler's identity is the substrate's bounded name, and it is never the
// host's. A remote handler that reported "host" would make Service.Contained()
// false and quietly unwind containment for a substrate that IS contained.
func TestRemoteHandlerNameIsTheSubstrateAndNeverHost(t *testing.T) {
	h := testRemoteHandler(t, &remoteStub{}, DefaultConfig())
	if got := h.Name(); got != "remote-stub" {
		t.Fatalf("Name() = %q, want the substrate's name", got)
	}
	if h.Name() == HandlerHost {
		t.Fatal("a remote handler must never report the host identity")
	}
}

// A substrate with no name cannot be constructed: Name() is an event and metric
// label, and an empty one is an unlabelled series.
func TestNewRemoteHandlerRefusesANamelessSubstrate(t *testing.T) {
	if _, err := NewRemoteHandler(&namelessSubstrate{}, DefaultConfig()); err == nil {
		t.Fatal("NewRemoteHandler accepted a substrate with an empty Name()")
	}
	if _, err := NewRemoteHandler(nil, DefaultConfig()); err == nil {
		t.Fatal("NewRemoteHandler accepted a nil substrate")
	}
}

type namelessSubstrate struct{ remoteStub }

func (*namelessSubstrate) Name() string { return "" }

// --- acquire / exec / release ------------------------------------------------

// The happy path, end to end through the adapter: Verify once, Provision, then
// an Exec that reaches the substrate carrying the resolved environment and the
// CONTAINED working directory.
func TestRemoteHandlerAcquireExecRelease(t *testing.T) {
	sub := &remoteStub{}
	h := testRemoteHandler(t, sub, DefaultConfig())
	p := loopauth.Principal{Tenant: "t-1", Subject: "s-1"}

	runner, err := h.Acquire(context.Background(), p, Env{Allow: map[string]string{"PATH": "/bin"}})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := runner.Exec(context.Background(), "echo hi", ""); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	sb := sub.only(t)
	execs, _, _ := sb.observed()
	if len(execs) != 1 {
		t.Fatalf("substrate saw %d Execs, want 1", len(execs))
	}
	if execs[0].cmd != "echo hi" {
		t.Fatalf("cmd = %q, want %q", execs[0].cmd, "echo hi")
	}
	if execs[0].workingDir != sb.MountRoot() {
		t.Fatalf("workingDir = %q, want the sandbox mount root %q", execs[0].workingDir, sb.MountRoot())
	}
	if len(execs[0].env) != 1 || execs[0].env[0] != "PATH=/bin" {
		t.Fatalf("env = %#v, want exactly [PATH=/bin]", execs[0].env)
	}

	if err := runner.Release(context.Background()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, _, teardowns := sb.observed(); teardowns != 1 {
		t.Fatalf("teardowns = %d after Release, want 1", teardowns)
	}
}

// Release is idempotent: bash.go releases explicitly AND in a defer, so a second
// call must not tear down a sandbox a second time (which, on a real substrate,
// is a second Delete against a name that may by then belong to someone else).
func TestRemoteRunnerReleaseIsIdempotent(t *testing.T) {
	sub := &remoteStub{}
	h := testRemoteHandler(t, sub, DefaultConfig())

	runner, err := h.Acquire(context.Background(), loopauth.Principal{Tenant: "t"}, Env{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := runner.Release(context.Background()); err != nil {
			t.Fatalf("Release #%d: %v", i, err)
		}
	}
	if _, _, teardowns := sub.only(t).observed(); teardowns != 1 {
		t.Fatalf("teardowns = %d after 3 Releases, want 1", teardowns)
	}
}

// The containment check runs in the ADAPTER, against the sandbox's own
// MountRoot(), and a refusal never reaches the substrate. This is ADR-0044's
// gate 4 for a remote sandbox: the model's working_dir is a subpath request, and
// an escape is refused, not clamped.
//
// This test runs the SHIPPED configuration and nothing else — a plain
// NewRemoteHandler, no options. That is the whole point of it: the adapter
// previously resolved containment through resolveWorkspace, which canonicalises
// against fuse's OWN filesystem, so its only coverage was a test-only host root
// (a real t.TempDir()) that never ships. In the shipped binary "/workspace" is a
// path in the Pod and not in fuse's container, so every honoured case below
// refused and the feature was inoperative.
func TestRemoteRunnerExecContainsTheWorkingDirInTheShippedConfiguration(t *testing.T) {
	sub := &remoteStub{}
	h := testRemoteHandler(t, sub, DefaultConfig())

	runner, err := h.Acquire(context.Background(), loopauth.Principal{Tenant: "t"}, Env{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	sb := sub.only(t)
	mount := sb.MountRoot() // "/workspace" — a path fuse's own filesystem does not have.

	// --- HONOURED. A non-empty working_dir must actually work. --------------
	for _, tc := range []struct {
		workingDir string
		want       string
	}{
		{"internal/tools", mount + "/internal/tools"},
		{mount + "/internal/tools", mount + "/internal/tools"},
		{mount, mount},
		{".", mount},
	} {
		t.Run("honoured "+tc.workingDir, func(t *testing.T) {
			out, err := runner.Exec(context.Background(), "echo hi", tc.workingDir)
			if err != nil {
				t.Fatalf("Exec(working_dir=%q) = %v; a contained in-Pod subpath must be honoured, "+
					"and it is NOT resolvable on fuse's own filesystem", tc.workingDir, err)
			}
			if out.ExitCode != 0 {
				t.Fatalf("ExitCode = %d, want 0", out.ExitCode)
			}
			execs, _, _ := sb.observed()
			got := execs[len(execs)-1].workingDir
			if got != tc.want {
				t.Fatalf("the substrate was asked to run in %q, want %q", got, tc.want)
			}
		})
	}

	before, _, _ := sb.observed()

	// --- REFUSED, and refused before the control plane is touched. ----------
	for _, escape := range []string{"..", "../..", "/etc", "/", mount + "/../etc", mount + "/pkg/../..", "/workspaceXXX"} {
		t.Run("refused "+escape, func(t *testing.T) {
			out, err := runner.Exec(context.Background(), "cat /etc/shadow", escape)
			if !errors.Is(err, ErrWorkingDirRefused) {
				t.Fatalf("err = %v, want it to wrap ErrWorkingDirRefused", err)
			}
			if out.ExitCode != -1 {
				t.Fatalf("ExitCode = %d, want -1 so an ExitCode-only caller fails closed", out.ExitCode)
			}
		})
	}

	// The load-bearing assertion: not one refusal reached the substrate. A
	// refusal that still issued the exec would have already run the command.
	after, _, _ := sb.observed()
	if len(after) != len(before) {
		t.Fatalf("%d refusals still reached the substrate: %#v", len(after)-len(before), after[len(before):])
	}
}

// A substrate that reports no MountRoot at all has given the adapter nothing to
// contain a working_dir against. The refusal must be ErrNoTrustedRoot and it must
// happen before the control plane is touched — promoting the model's own path to
// the working directory is the fail-open direction ADR-0044 forbids.
func TestRemoteRunnerExecRefusesWhenTheSubstrateReportsNoMountRoot(t *testing.T) {
	sub := &remoteStub{mount: "-"} // "-" is the stub's "report an empty MountRoot" marker.
	h := testRemoteHandler(t, sub, DefaultConfig())

	runner, err := h.Acquire(context.Background(), loopauth.Principal{Tenant: "t"}, Env{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	for _, workingDir := range []string{"", "pkg", "/workspace"} {
		out, err := runner.Exec(context.Background(), "echo hi", workingDir)
		if !errors.Is(err, ErrNoTrustedRoot) {
			t.Fatalf("Exec(working_dir=%q) err = %v, want it to wrap ErrNoTrustedRoot", workingDir, err)
		}
		if out.ExitCode != -1 {
			t.Fatalf("ExitCode = %d, want -1", out.ExitCode)
		}
	}
	if execs, _, _ := sub.only(t).observed(); len(execs) != 0 {
		t.Fatalf("the refusal still reached the substrate: %#v", execs)
	}
}

// --- sticky Verify -----------------------------------------------------------

// Verify runs ONCE and its refusal is STICKY: every later Acquire refuses
// without re-probing, and nothing is ever provisioned. A substrate whose floor
// is unproven is disqualified for the handler's lifetime (ADR-0058 rule 3),
// exactly as a kvm-absent host is.
func TestRemoteHandlerVerifyRefusalIsStickyAndProvisionsNothing(t *testing.T) {
	floorErr := errors.New("canary leg 2 reached the api server")
	sub := &remoteStub{verifyErr: floorErr}
	h := testRemoteHandler(t, sub, DefaultConfig())

	for i := 0; i < 4; i++ {
		runner, err := h.Acquire(context.Background(), loopauth.Principal{Tenant: "t"}, Env{})
		if err == nil {
			t.Fatalf("Acquire #%d succeeded (%T), want a refusal", i, runner)
		}
		if runner != nil {
			t.Fatalf("Acquire #%d returned a non-nil Runner alongside the refusal", i)
		}
		if !errors.Is(err, floorErr) {
			t.Fatalf("Acquire #%d err = %v, want it to wrap the Verify failure", i, err)
		}
		if !errors.Is(err, ErrRefusedUncontained) {
			t.Fatalf("Acquire #%d err = %v, want it to wrap ErrRefusedUncontained", i, err)
		}
	}

	verify, provision, _ := sub.stats()
	if verify != 1 {
		t.Fatalf("Verify was called %d times, want exactly 1 — a refusal must be cached, not re-probed", verify)
	}
	if provision != 0 {
		t.Fatalf("Provision was called %d times on the unverified-floor path, want 0", provision)
	}
}

// A successful Verify is likewise cached: N Acquires probe the floor once.
func TestRemoteHandlerVerifyRunsOnceOnTheHappyPath(t *testing.T) {
	sub := &remoteStub{}
	h := testRemoteHandler(t, sub, DefaultConfig())

	for i := 0; i < 3; i++ {
		if _, err := h.Acquire(context.Background(), loopauth.Principal{Tenant: "t"}, Env{}); err != nil {
			t.Fatalf("Acquire #%d: %v", i, err)
		}
	}
	verify, provision, _ := sub.stats()
	if verify != 1 {
		t.Fatalf("Verify was called %d times, want exactly 1", verify)
	}
	if provision != 3 {
		t.Fatalf("Provision was called %d times, want 3", provision)
	}
}

// A Provision failure is a refusal, and — unlike a Verify failure — it is NOT
// sticky: a transient scheduling failure must not disqualify the substrate for
// the process's life.
func TestRemoteHandlerProvisionFailureRefusesWithoutARunner(t *testing.T) {
	boom := errors.New("pod never reached Running")
	sub := &remoteStub{provisionErr: boom}
	h := testRemoteHandler(t, sub, DefaultConfig())

	runner, err := h.Acquire(context.Background(), loopauth.Principal{Tenant: "t"}, Env{})
	if err == nil {
		t.Fatalf("Acquire succeeded (%T), want a refusal", runner)
	}
	if runner != nil {
		t.Fatalf("Acquire returned a non-nil Runner alongside the refusal")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the provision failure", err)
	}

	sub.mu.Lock()
	sub.provisionErr = nil
	sub.mu.Unlock()
	if _, err := h.Acquire(context.Background(), loopauth.Principal{Tenant: "t"}, Env{}); err != nil {
		t.Fatalf("a later Acquire after a transient provision failure: %v", err)
	}
}

// --- timeout, gone -----------------------------------------------------------

// A deadline tears the sandbox DOWN and marks the Runner gone. That is the
// run-`--rm` guarantee carried across a network: a timed-out command never
// leaves a process behind, and it is kept by construction (delete the sandbox)
// rather than by trusting an in-image timeout binary.
func TestRemoteRunnerTimeoutTearsDownAndMarksGone(t *testing.T) {
	sub := &remoteStub{}
	h := testRemoteHandler(t, sub, DefaultConfig())

	runner, err := h.Acquire(context.Background(), loopauth.Principal{Tenant: "t"}, Env{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	sb := sub.only(t)
	sb.mu.Lock()
	sb.block = make(chan struct{}) // never closed: the Exec outlives its deadline
	sb.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	out, err := runner.Exec(ctx, "sleep 100", "")
	if err != nil {
		t.Fatalf("a timed-out Exec must report TimedOut, not an error: %v", err)
	}
	if !out.TimedOut {
		t.Fatal("TimedOut = false, want true")
	}
	if out.ExitCode == 0 {
		t.Fatal("ExitCode = 0 on a timed-out Exec; a killed command reporting success is not a result we pass on")
	}
	if _, _, teardowns := sb.observed(); teardowns != 1 {
		t.Fatalf("teardowns = %d after a deadline, want 1 — the sandbox must be GONE", teardowns)
	}

	// Gone: every later Exec refuses, and none of them reaches the substrate.
	before, _, _ := sb.observed()
	for i := 0; i < 2; i++ {
		out, err := runner.Exec(context.Background(), "echo hi", "")
		if !errors.Is(err, ErrSandboxGone) {
			t.Fatalf("Exec #%d after the teardown err = %v, want ErrSandboxGone", i, err)
		}
		if out.ExitCode != -1 {
			t.Fatalf("ExitCode = %d on a gone sandbox, want -1", out.ExitCode)
		}
	}
	after, _, teardowns := sb.observed()
	if len(after) != len(before) {
		t.Fatalf("an Exec on a gone sandbox still reached the substrate: %#v", after[len(before):])
	}

	// Release on a gone Runner is a NO-OP teardown: the sandbox is already
	// deleted, and a second Delete would target a name that may by then belong
	// to someone else.
	if err := runner.Release(context.Background()); err != nil {
		t.Fatalf("Release on a gone Runner: %v", err)
	}
	if _, _, n := sb.observed(); n != teardowns {
		t.Fatalf("Release on a gone Runner tore down again (%d -> %d)", teardowns, n)
	}
}

// --- ResetEnv ----------------------------------------------------------------

// The Pool's reset-on-checkout must be MEANINGFUL on a remote sandbox whose
// container env cannot change: the fresh allowlist is stored on the Runner and
// it is what the NEXT Exec renders. Without this a warm Pod would keep serving
// the environment it was first acquired with — a rotated credential lingering in
// every later command.
func TestRemoteRunnerResetEnvIsWhatTheNextExecRenders(t *testing.T) {
	sub := &remoteStub{}
	h := testRemoteHandler(t, sub, DefaultConfig())

	runner, err := h.Acquire(context.Background(), loopauth.Principal{Tenant: "t"},
		Env{Allow: map[string]string{"TOKEN": "old", "PATH": "/bin"}})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := runner.Exec(context.Background(), "first", ""); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	re, ok := runner.(EnvResetter)
	if !ok {
		t.Fatal("the remote Runner does not implement EnvResetter, so the Pool cannot certify it for reuse")
	}
	if err := re.ResetEnv(Env{Allow: map[string]string{"TOKEN": "new"}}); err != nil {
		t.Fatalf("ResetEnv: %v", err)
	}
	if _, err := runner.Exec(context.Background(), "second", ""); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	execs, _, _ := sub.only(t).observed()
	if len(execs) != 2 {
		t.Fatalf("substrate saw %d Execs, want 2", len(execs))
	}
	wantFirst := []string{"PATH=/bin", "TOKEN=old"}
	wantSecond := []string{"TOKEN=new"}
	if got := execs[0].env; !equalStrings(got, wantFirst) {
		t.Fatalf("first Exec env = %#v, want %#v", got, wantFirst)
	}
	if got := execs[1].env; !equalStrings(got, wantSecond) {
		t.Fatalf("second Exec env = %#v, want %#v — the RESET allowlist, not the one Acquire rendered", got, wantSecond)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- heartbeat and reaper ----------------------------------------------------

// A live Runner is heartbeaten so another instance's reaper does not mistake it
// for an orphan, and the heartbeat STOPS at Release (a heartbeat on a torn-down
// sandbox would keep an orphan looking alive).
func TestRemoteRunnerHeartbeatsWhileLiveAndStopsOnRelease(t *testing.T) {
	sub := &remoteStub{}
	h := testRemoteHandler(t, sub, DefaultConfig(), withRemoteHeartbeatInterval(time.Millisecond))

	runner, err := h.Acquire(context.Background(), loopauth.Principal{Tenant: "t"}, Env{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	sb := sub.only(t)

	waitFor(t, "a heartbeat", func() bool {
		_, beats, _ := sb.observed()
		return beats > 0
	})

	if err := runner.Release(context.Background()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// Let any in-flight tick land, then pin the count: it must not grow again.
	time.Sleep(20 * time.Millisecond)
	_, settled, _ := sb.observed()
	time.Sleep(20 * time.Millisecond)
	if _, beats, _ := sb.observed(); beats != settled {
		t.Fatalf("heartbeats grew from %d to %d after Release — the goroutine outlived the sandbox", settled, beats)
	}
}

// The per-handler reaper calls Reap with 2*IdleTTL. It is on the HANDLER and not
// the Pool deliberately: orphans exist precisely when no Pool remembers them.
func TestRemoteHandlerReaperCallsReapWithTwiceTheIdleTTL(t *testing.T) {
	sub := &remoteStub{}
	cfg := DefaultConfig()
	cfg.IdleTTL = 30 * time.Second
	testRemoteHandler(t, sub, cfg, withRemoteReapInterval(time.Millisecond))

	waitFor(t, "a Reap call", func() bool {
		_, _, reaps := sub.stats()
		return len(reaps) > 0
	})

	_, _, reaps := sub.stats()
	if want := 2 * cfg.IdleTTL; reaps[0] != want {
		t.Fatalf("Reap staleAfter = %v, want 2*IdleTTL = %v", reaps[0], want)
	}
}

// Close stops the reaper. Without it every Service construction in a long-lived
// process leaks a goroutine that keeps issuing control-plane list calls.
func TestRemoteHandlerCloseStopsTheReaper(t *testing.T) {
	sub := &remoteStub{}
	h, err := NewRemoteHandler(sub, DefaultConfig(), withRemoteReapInterval(time.Millisecond))
	if err != nil {
		t.Fatalf("NewRemoteHandler: %v", err)
	}
	waitFor(t, "a Reap call", func() bool {
		_, _, reaps := sub.stats()
		return len(reaps) > 0
	})
	if err := h.(*remoteHandler).Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, _, settled := sub.stats()
	time.Sleep(20 * time.Millisecond)
	if _, _, reaps := sub.stats(); len(reaps) != len(settled) {
		t.Fatalf("Reap calls grew from %d to %d after Close", len(settled), len(reaps))
	}
}

// --- the Pool sees a remote Runner whole ------------------------------------

// containerIdentified had NO implementor before this change, so the Pool's
// runnerContainerID and certifyEntry paths were never exercised against a
// substrate that reports one. A warm remote sandbox is the first, and both
// halves — the container id on the acquire record, and the mount re-assertion on
// a hit — must work for real.
func TestPoolCertifiesARemoteRunnersIdentityAndMount(t *testing.T) {
	sub := &remoteStub{}
	h := testRemoteHandler(t, sub, DefaultConfig())
	cfg := DefaultConfig()
	cfg.Handler = "remote-stub"
	svc, err := NewService(cfg, WithHandlerFactory("remote-stub", func(Config) (Handler, error) { return h, nil }))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	var acquired []AcquireInfo
	var mu sync.Mutex
	pool := NewPool(svc, WithPoolHooks(PoolHooks{
		Acquired: func(i AcquireInfo) {
			mu.Lock()
			defer mu.Unlock()
			acquired = append(acquired, i)
		},
	}))
	defer pool.Close(context.Background())

	p := loopauth.Principal{Tenant: "t-1", Subject: "s-1"}
	first, err := pool.Acquire(context.Background(), p)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	sb := sub.only(t)
	if err := pool.Release(context.Background(), p, first, CauseReleased); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// The warm entry is re-certified and REUSED: same sandbox, no second
	// Provision.
	second, err := pool.Acquire(context.Background(), p)
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	if _, provisions, _ := sub.stats(); provisions != 1 {
		t.Fatalf("Provision was called %d times, want 1 — the warm remote sandbox was not reused", provisions)
	}

	mu.Lock()
	records := append([]AcquireInfo(nil), acquired...)
	mu.Unlock()
	if len(records) != 2 {
		t.Fatalf("saw %d Acquired records, want 2", len(records))
	}
	if records[0].ContainerID != sb.ID() {
		t.Fatalf("cold-start ContainerID = %q, want the sandbox id %q", records[0].ContainerID, sb.ID())
	}
	if records[1].ContainerID != sb.ID() || !records[1].Reused {
		t.Fatalf("warm-hit record = %+v, want Reused with the sandbox id %q", records[1], sb.ID())
	}
	if got := runnerMountRoot(Unwrap(second)); got != sb.MountRoot() {
		t.Fatalf("runnerMountRoot = %q, want the sandbox mount root %q", got, sb.MountRoot())
	}

	// And the mount half of certifyEntry bites: corrupt the Runner's reported
	// mount and the warm entry must be DISCARDED rather than handed out.
	if err := pool.Release(context.Background(), p, second, CauseReleased); err != nil {
		t.Fatalf("Release: %v", err)
	}
	rr, ok := Unwrap(second).(*remoteRunner)
	if !ok {
		t.Fatalf("Unwrap gave %T, want *remoteRunner", Unwrap(second))
	}
	rr.mu.Lock()
	rr.mount = "/somewhere-else"
	rr.mu.Unlock()

	if _, err := pool.Acquire(context.Background(), p); err != nil {
		t.Fatalf("third Acquire: %v", err)
	}
	if _, provisions, _ := sub.stats(); provisions != 2 {
		t.Fatalf("Provision was called %d times, want 2 — a drifted mount must force a cold start", provisions)
	}
}

// --- the handler factory and the security property it must not break --------

// The registered factory is consulted for the named handler, and the Service it
// yields reports that handler's identity — which is what makes `handler:
// kubernetes` observable at the composition root rather than silently inert.
func TestServiceHandlerFactoryIsConsultedForTheNamedHandler(t *testing.T) {
	sub := &remoteStub{}
	cfg := DefaultConfig()
	cfg.Handler = "kubernetes"

	var got Config
	svc, err := NewService(cfg,
		WithHandlerFactory("kubernetes", func(c Config) (Handler, error) {
			got = c
			return NewRemoteHandler(sub, c)
		}),
		withContainerLookPath(fakeLookPath()), // nothing on PATH: the container factory MUST NOT be the answer
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if !svc.Available() {
		t.Fatal("Available() = false, want true — the registered factory was never consulted")
	}
	if name := svc.HandlerName(); name != "remote-stub" {
		t.Fatalf("HandlerName() = %q, want the factory's handler", name)
	}
	if got.Handler != "kubernetes" {
		t.Fatalf("the factory was handed Config.Handler = %q, want %q", got.Handler, "kubernetes")
	}
	if !svc.Contained() {
		t.Fatal("Contained() = false for a remote handler, want true")
	}
}

// THE SECURITY REGRESSION TEST for this change.
//
// selectHandler's shape — the ABSENCE of a host-fallback branch — is a
// documented structural property, not a check: "unreachable from this branch by
// construction, not by a check that a later edit could invert". Adding a third
// branch for named factories is exactly the edit that could invert it, so this
// pins that a named handler whose construction FAILS refuses, and that the
// refusal never yields the host handler — nor the container one.
func TestServiceNamedHandlerFactoryFailureRefusesAndNeverReachesTheHost(t *testing.T) {
	boom := errors.New("no kubeconfig and not in a cluster")

	for _, tc := range []struct {
		name   string
		hosted bool
		file   string
	}{
		{name: "local"},
		{name: "hosted", hosted: true},
		// The nastiest shape: the operator ALSO wrote the off-switch. handler:
		// is authoritative and named kubernetes, so contained:false must not
		// become a fallback the failed construction falls into.
		{name: "local with the off-switch also set", file: "contained: false\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Handler = "kubernetes"
			if tc.file != "" {
				cfg.Contained = false
			}

			host := &recordingHandler{name: HandlerHost}
			container := &recordingHandler{name: HandlerContainer}

			svc, err := NewService(cfg,
				WithHostedPosture(tc.hosted),
				WithHandlerFactory("kubernetes", func(Config) (Handler, error) { return nil, boom }),
				withHostHandler(host),
				withContainerFactory(func(Config, ...containerOption) (Handler, error) { return container, nil }),
			)
			if err != nil {
				t.Fatalf("NewService: unexpected construction error: %v", err)
			}

			if svc.Available() {
				t.Error("Available() = true after the named handler failed to construct")
			}
			if name := svc.HandlerName(); name != "" {
				t.Errorf("HandlerName() = %q, want %q when nothing was selected", name, "")
			}
			if svc.Contained() {
				t.Error("Contained() = true when nothing was selected")
			}

			runner, err := svc.Acquire(context.Background(), testPrincipal())
			switch {
			case err == nil:
				t.Errorf("Acquire succeeded (%T), want a refusal", runner)
			default:
				if runner != nil {
					t.Errorf("Acquire returned a non-nil Runner (%T) alongside the refusal", runner)
				}
				if !errors.Is(err, ErrRefusedUncontained) {
					t.Errorf("err = %v, want it to wrap ErrRefusedUncontained", err)
				}
				if !errors.Is(err, boom) {
					t.Errorf("err = %v, want it to wrap the factory's failure so the operator learns WHY", err)
				}
			}
			if _, isHost := runner.(*hostRunner); isHost {
				t.Error("Acquire handed out a host Runner after a named handler failed")
			}

			// The load-bearing assertion: neither substitute was TOUCHED. Not
			// "the error was right" — nothing ran, and nothing was even
			// acquired from.
			if acquires, execs := host.counts(); acquires != 0 || len(execs) != 0 {
				t.Errorf("host handler saw %d Acquires and %d Execs; the failed-named-handler path must never reach the host",
					acquires, len(execs))
			}
			if acquires, execs := container.counts(); acquires != 0 || len(execs) != 0 {
				t.Errorf("container handler saw %d Acquires and %d Execs; a named handler that fails must not be SUBSTITUTED either",
					acquires, len(execs))
			}
		})
	}
}

// A named handler with no factory registered refuses too: the operator asked
// for a substrate this binary cannot build, which is the
// `security-knob-inert-at-composition-root` shape — and the answer is a refusal,
// never the container handler quietly standing in.
func TestServiceNamedHandlerWithNoFactoryRefuses(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Handler = "kubernetes"

	container := &recordingHandler{name: HandlerContainer}
	host := &recordingHandler{name: HandlerHost}
	svc, err := NewService(cfg,
		withHostHandler(host),
		withContainerFactory(func(Config, ...containerOption) (Handler, error) { return container, nil }),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if svc.Available() {
		t.Fatal("Available() = true for a named handler with no registered factory")
	}
	if _, err := svc.Acquire(context.Background(), testPrincipal()); !errors.Is(err, ErrRefusedUncontained) {
		t.Fatalf("err = %v, want ErrRefusedUncontained", err)
	}
	if acquires, _ := container.counts(); acquires != 0 {
		t.Errorf("container handler saw %d Acquires; an unbuildable named handler must not fall back to it", acquires)
	}
	if acquires, _ := host.counts(); acquires != 0 {
		t.Errorf("host handler saw %d Acquires", acquires)
	}
}

// A registered factory must never be reachable for the HOST handler: the
// off-switch's authorization path is the host handler and nothing else, so a
// factory registered under "host" (by mistake or by a future edit) must not
// intercept it.
func TestServiceHandlerFactoryNeverInterceptsTheHostOrContainerHandler(t *testing.T) {
	for _, handler := range []string{HandlerHost, HandlerContainer} {
		t.Run(handler, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Handler = handler
			cfg.Contained = handler != HandlerHost

			var called bool
			container := &recordingHandler{name: HandlerContainer}
			host := &recordingHandler{name: HandlerHost}
			svc, err := NewService(cfg,
				WithHandlerFactory(handler, func(Config) (Handler, error) {
					called = true
					return &recordingHandler{name: "hijacked"}, nil
				}),
				withHostHandler(host),
				withContainerFactory(func(Config, ...containerOption) (Handler, error) { return container, nil }),
			)
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			if called {
				t.Fatalf("a factory registered under %q intercepted the built-in handler", handler)
			}
			if got := svc.HandlerName(); got != handler {
				t.Fatalf("HandlerName() = %q, want %q", got, handler)
			}
		})
	}
}

// WithHandlerFactory refuses to register anything that could shadow a built-in
// decision or produce an unlabelled Service: an empty name, the two reserved
// names, and a nil function are all ignored.
func TestWithHandlerFactoryIgnoresUnusableRegistrations(t *testing.T) {
	o := &serviceOptions{}
	for _, tc := range []struct {
		name string
		key  string
		fn   func(Config) (Handler, error)
	}{
		{name: "empty name", key: "", fn: func(Config) (Handler, error) { return nil, nil }},
		{name: "whitespace name", key: "   ", fn: func(Config) (Handler, error) { return nil, nil }},
		{name: "host is reserved", key: HandlerHost, fn: func(Config) (Handler, error) { return nil, nil }},
		{name: "container is reserved", key: HandlerContainer, fn: func(Config) (Handler, error) { return nil, nil }},
		{name: "nil function", key: "kubernetes", fn: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			WithHandlerFactory(tc.key, tc.fn)(o)
			if _, ok := o.handlerFactories[tc.key]; ok {
				t.Fatalf("WithHandlerFactory registered %q", tc.key)
			}
		})
	}
}

// withContainerFactory substitutes the container substrate's FACTORY (tests).
//
// It exists for the same reason withHostHandler does — so a refusal test can
// prove the path never touched the substitute at all, which a real
// containerHandler (which records nothing, and which on a machine WITH docker
// installed would construct successfully) cannot show.
func withContainerFactory(fn func(Config, ...containerOption) (Handler, error)) ServiceOption {
	return func(o *serviceOptions) {
		if fn != nil {
			o.newContainer = fn
		}
	}
}

// The adapter adds two goroutines per handler-plus-Runner — the heartbeat and the
// orphan reaper — and both touch state a caller's Exec touches. The race detector
// sees nothing without a test that actually drives them at once (learning:
// race-invisible-to-race-detector-without-concurrent-test), so this is that test:
// concurrent Execs and ResetEnvs against a fast heartbeat and a fast reaper.
//
// Run it under -race; it asserts nothing beyond "no data race and no panic",
// which is exactly what it is for.
func TestRemoteRunnerExecRacesHeartbeatAndReaper(t *testing.T) {
	sub := &remoteStub{}
	cfg := DefaultConfig()
	cfg.IdleTTL = time.Second
	h := testRemoteHandler(t, sub, cfg,
		withRemoteHeartbeatInterval(time.Millisecond),
		withRemoteReapInterval(time.Millisecond),
	)

	runner, err := h.Acquire(context.Background(), loopauth.Principal{Tenant: "t"}, Env{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	re, ok := runner.(EnvResetter)
	if !ok {
		t.Fatal("the remote Runner does not implement EnvResetter")
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; n < 40; n++ {
				if _, err := runner.Exec(context.Background(), fmt.Sprintf("cmd-%d-%d", i, n), ""); err != nil {
					t.Errorf("Exec: %v", err)
					return
				}
				if err := re.ResetEnv(Env{Allow: map[string]string{"N": fmt.Sprint(n)}}); err != nil {
					t.Errorf("ResetEnv: %v", err)
					return
				}
				// Reading the Pool's seams concurrently too: certifyEntry calls
				// these from whichever goroutine is checking out.
				_ = runnerMountRoot(runner)
				_ = runnerContainerID(runner)
			}
		}(i)
	}
	wg.Wait()

	if err := runner.Release(context.Background()); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

// --- the floor_unverified health event and the orphan reap event (task 9) -----

// A REFUSED FLOOR FIRES sandbox.health WITH floor_unverified, on every refused
// Acquire.
//
// Every one, not only the first, and that is deliberate: the verdict is sticky and
// there is no recovery path, so an operator who missed the first event must still
// be able to see that a configured Kubernetes handler is refusing everything and
// why. The reason is its own closed value rather than acquire_failed — a cluster
// whose CNI does not enforce NetworkPolicy is a permanent, human-sized problem,
// and it means every sandbox previously run there had unrestricted egress.
func TestRemoteHandlerEmitsFloorUnverifiedOnEveryRefusedAcquire(t *testing.T) {
	sub := &remoteStub{verifyErr: errors.New("canary leg 2 reached the api server")}
	h := testRemoteHandler(t, sub, DefaultConfig())

	var mu sync.Mutex
	var fired []HealthInfo
	h.(*remoteHandler).setHealthHooks(HealthHooks{Unhealthy: func(i HealthInfo) {
		mu.Lock()
		defer mu.Unlock()
		fired = append(fired, i)
	}})

	p := loopauth.Principal{Tenant: "acme", Subject: "s"}
	for range 3 {
		if _, err := h.Acquire(context.Background(), p, Env{}); err == nil {
			t.Fatal("Acquire succeeded on an unverified floor")
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(fired) != 3 {
		t.Fatalf("health events = %d, want one per refused Acquire (3)", len(fired))
	}
	for i, info := range fired {
		if info.Reason != HealthFloorUnverified {
			t.Errorf("event %d reason = %q, want %q", i, info.Reason, HealthFloorUnverified)
		}
		if info.Healthy {
			t.Errorf("event %d is marked healthy; an unproven floor is not a healthy transition", i)
		}
		if info.Principal != p {
			t.Errorf("event %d principal = %+v, want %+v", i, info.Principal, p)
		}
		if info.Handler != sub.Name() {
			t.Errorf("event %d handler = %q, want %q", i, info.Handler, sub.Name())
		}
	}
}

// A CALLER-DEADLINE expiry during the first Verify is NOT a floor_unverified
// event: that is the caller's bound firing, not the substrate failing, and the
// container handler makes the same exclusion on the provision path. Emitting here
// would turn a cancelled tool call into a "this cluster is broken" page.
func TestRemoteHandlerDoesNotEmitFloorUnverifiedForACallerDeadline(t *testing.T) {
	sub := &remoteStub{verifyErr: context.DeadlineExceeded}
	h := testRemoteHandler(t, sub, DefaultConfig())

	var mu sync.Mutex
	var fired []HealthInfo
	h.(*remoteHandler).setHealthHooks(HealthHooks{Unhealthy: func(i HealthInfo) {
		mu.Lock()
		defer mu.Unlock()
		fired = append(fired, i)
	}})

	if _, err := h.Acquire(context.Background(), loopauth.Principal{Tenant: "t"}, Env{}); err == nil {
		t.Fatal("Acquire succeeded")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(fired) != 0 {
		t.Fatalf("health events = %+v, want none: a caller deadline is not a substrate failure", fired)
	}
}

// THE ORPHAN REAPER REPORTS ONE EVENT PER COLLECTED ORPHAN, with cause `orphan`.
//
// It is the only signal there is that fuse instances are dying without releasing
// their sandboxes: an orphan is by definition a sandbox no Pool remembers, so the
// Pool's own reaper cannot see it and idle_ttl never fires for it. ContainerID is
// empty because Reap reports HOW MANY it deleted, not which, and inventing an id
// would be fabricating an observation (ADR-0056).
func TestRemoteHandlerReaperReportsOrphans(t *testing.T) {
	sub := &remoteStub{reapCount: 3}

	var mu sync.Mutex
	var reaped []ReleaseInfo
	h := testRemoteHandler(t, sub, DefaultConfig(),
		withRemoteReapInterval(5*time.Millisecond),
		WithRemoteReapHook(func(i ReleaseInfo) {
			mu.Lock()
			defer mu.Unlock()
			reaped = append(reaped, i)
		}))
	_ = h

	waitFor(t, "the reaper to report the 3 orphans the substrate collected", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(reaped) >= 3
	})

	mu.Lock()
	defer mu.Unlock()
	for i, info := range reaped {
		if info.Cause != CauseOrphan {
			t.Errorf("reap %d cause = %q, want %q — idle_ttl is this process's Pool reclaiming what it remembers; an orphan is one no Pool remembers", i, info.Cause, CauseOrphan)
		}
		if info.Handler != sub.Name() {
			t.Errorf("reap %d handler = %q, want %q", i, info.Handler, sub.Name())
		}
		if info.ContainerID != "" {
			t.Errorf("reap %d ContainerID = %q, want empty: Reap reports a COUNT, and an invented id is a fabricated observation", i, info.ContainerID)
		}
	}
}

// A reaper that collected NOTHING reports nothing. The ordinary steady state is a
// sweep that finds no orphans, and an event per empty sweep would drown the signal
// the non-empty ones carry.
func TestRemoteHandlerReaperIsSilentWhenItCollectsNothing(t *testing.T) {
	sub := &remoteStub{reapCount: 0}

	var mu sync.Mutex
	count := 0
	testRemoteHandler(t, sub, DefaultConfig(),
		withRemoteReapInterval(5*time.Millisecond),
		WithRemoteReapHook(func(ReleaseInfo) {
			mu.Lock()
			defer mu.Unlock()
			count++
		}))

	// Wait for several sweeps to have happened, which the reapCalls counter proves
	// rather than a sleep-and-hope.
	waitFor(t, "the reaper to sweep at least three times", func() bool {
		_, _, reaps := sub.stats()
		return len(reaps) >= 3
	})

	mu.Lock()
	defer mu.Unlock()
	if count != 0 {
		t.Fatalf("reap events = %d after sweeps that collected nothing, want 0", count)
	}
}

// TestServiceSelectionRefusalIsReadableAtStartup closes a gap task 10 found in
// the composition root, not in this package.
//
// A refused SELECTION is stored on the Service (s.refusal) and surfaced only at
// Acquire — so a fuse configured with `handler: kubernetes` that this binary
// cannot build comes up looking healthy, prints nothing, and fails every bash
// call at runtime with a message only the model sees. That is the
// `security-knob-inert-at-composition-root` failure wearing its other face: not
// an unwired knob, but a wired knob whose refusal nobody is told about.
//
// SelectionRefusal makes the refusal readable at STARTUP so cmd/fuse can say it
// out loud beside the UNCONTAINED and EGRESS-BLACKOUT notices. It is read-only
// and reports nothing about anything else: a Service that selected a handler
// reports nil, which is what lets the composition root print unconditionally.
func TestServiceSelectionRefusalIsReadableAtStartup(t *testing.T) {
	boom := errors.New("no cluster")
	cfg := DefaultConfig()
	cfg.Handler = "kubernetes"

	svc, err := NewService(cfg,
		WithHandlerFactory("kubernetes", func(Config) (Handler, error) { return nil, boom }))
	if err != nil {
		t.Fatalf("NewService returned an error rather than a refusing Service: %v", err)
	}
	if svc.Available() {
		t.Fatal("a failed factory produced an available Service")
	}
	refusal := svc.SelectionRefusal()
	if refusal == nil {
		t.Fatal("SelectionRefusal() = nil on a Service whose selection refused; the composition root then has " +
			"NOTHING to print and a fuse that refuses every bash call comes up looking healthy")
	}
	if !errors.Is(refusal, ErrRefusedUncontained) {
		t.Errorf("SelectionRefusal() = %v, want it wrapped in ErrRefusedUncontained", refusal)
	}
	if !errors.Is(refusal, boom) {
		t.Errorf("SelectionRefusal() = %v, want it to carry the factory's own cause %v", refusal, boom)
	}

	// A Service that DID select reports nil, so the caller can print
	// unconditionally without inventing a refusal.
	ok, err := NewService(DefaultConfig(),
		withContainerLookPath(func(string) (string, error) { return "/usr/bin/docker", nil }),
		withContainerExec(func(context.Context, string, ...string) ([]byte, int, error) { return []byte("ok"), 0, nil }))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if r := ok.SelectionRefusal(); r != nil {
		t.Errorf("SelectionRefusal() = %v on a Service that selected the container handler, want nil", r)
	}
	if r := (*Service)(nil).SelectionRefusal(); r != nil {
		t.Errorf("a nil *Service reports %v; NewBash(nil) is a supported shape and this must be callable beside it", r)
	}
}
