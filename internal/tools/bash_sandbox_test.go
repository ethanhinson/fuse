package tools

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/loopauth"
	"github.com/ethanhinson/fuse/internal/toolidentity"
	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// --- test doubles ---------------------------------------------------------
//
// The bash tool acquires from the substrate seam (a *sandbox.Pool in
// production). These doubles stand in for it so the tool's OWN behaviour —
// which principal it keys by, how it maps Exec results, and whether it hands
// the Runner back on EVERY path — is assertable without a container daemon and
// without reaching into the sandbox package's unexported selection seams.

type fakeRunner struct {
	out      sandbox.Output
	err      error
	execs    int
	lastCmd  string
	lastDir  string
	released int
}

func (r *fakeRunner) Exec(_ context.Context, cmd string, dir string) (sandbox.Output, error) {
	r.execs++
	r.lastCmd, r.lastDir = cmd, dir
	return r.out, r.err
}

func (r *fakeRunner) Release(context.Context) error { r.released++; return nil }

// fakeSubstrate counts checkouts against returns. A leak is exactly the state
// this double is built to make visible: acquires > releases.
type fakeSubstrate struct {
	mu sync.Mutex

	runner     *fakeRunner
	acquireErr error

	acquires  int
	releases  int
	closes    int
	principal loopauth.Principal
	causes    []sandbox.ReleaseCause
}

func (s *fakeSubstrate) Acquire(_ context.Context, p loopauth.Principal) (sandbox.Runner, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.principal = p
	if s.acquireErr != nil {
		return nil, s.acquireErr
	}
	s.acquires++
	return s.runner, nil
}

func (s *fakeSubstrate) Release(_ context.Context, _ loopauth.Principal, _ sandbox.Runner, cause sandbox.ReleaseCause) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releases++
	s.causes = append(s.causes, cause)
	return nil
}

func (s *fakeSubstrate) Close(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closes++
	return nil
}

func (s *fakeSubstrate) balanced() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acquires == s.releases
}

func newFakeBash(sub *fakeSubstrate) *bashTool {
	return newBashOverSubstrate(sub)
}

// --- the principal is context-borne, never model-borne ---------------------

// The pool key comes from the authenticated loop principal on the context. A
// model that could steer it — by naming a tenant in `command` or `working_dir`
// — could check out ANOTHER tenant's warm execution context, which is the one
// thing the per-principal pool exists to prevent.
func TestBashPrincipalComesFromContextNotArgs(t *testing.T) {
	sub := &fakeSubstrate{runner: &fakeRunner{}}
	b := newFakeBash(sub)

	want := loopauth.Principal{Tenant: "tenantA", Subject: "alice"}
	ctx := toolidentity.WithPrincipal(context.Background(), want)

	res := b.Execute(ctx, `{"command":"echo tenantB","working_dir":"/tmp/tenantB"}`)
	if res.IsError {
		t.Fatalf("unexpected error result: %s", res.Output)
	}
	if sub.principal != want {
		t.Fatalf("acquired for %+v, want the context principal %+v", sub.principal, want)
	}
}

// With no principal on the context (the CLI bindings before they stamp one, and
// every direct tool test) the tool resolves the ZERO principal locally. It must
// not invent one from the arguments, and it must not refuse.
func TestBashMissingPrincipalUsesZeroPrincipal(t *testing.T) {
	sub := &fakeSubstrate{runner: &fakeRunner{}}
	b := newFakeBash(sub)

	res := b.Execute(context.Background(), `{"command":"echo hi"}`)
	if res.IsError {
		t.Fatalf("unexpected error result: %s", res.Output)
	}
	if (sub.principal != loopauth.Principal{}) {
		t.Fatalf("acquired for %+v, want the zero principal", sub.principal)
	}
}

// --- fail closed ----------------------------------------------------------

// A nil Service is NOT "run it on the host with whatever environment this
// process happens to hold" — that is the fail-open this whole change exists to
// remove. It is an unavailable tool.
func TestBashWithNoServiceFailsClosed(t *testing.T) {
	res := NewBash(nil).Execute(context.Background(), `{"command":"echo leaked"}`)
	if !res.IsError {
		t.Fatalf("a bash tool with no sandbox substrate must refuse; got %+v", res)
	}
	if !strings.Contains(res.Output, "unavailable") {
		t.Fatalf("refusal should say the tool is unavailable, got %q", res.Output)
	}
}

// The selection refusal (no container runtime, host not authorized) must reach
// the model as an ERROR result carrying the refusal, never as an empty success
// that reads like a command which ran and printed nothing.
func TestBashSurfacesUncontainedRefusal(t *testing.T) {
	sub := &fakeSubstrate{acquireErr: sandbox.ErrRefusedUncontained}
	b := newFakeBash(sub)

	res := b.Execute(context.Background(), `{"command":"echo hi"}`)
	if !res.IsError {
		t.Fatalf("a refusal must be an error result, got %+v", res)
	}
	if !strings.Contains(res.Output, "refusing to run bash uncontained") {
		t.Fatalf("refusal text lost: %q", res.Output)
	}
	if !sub.balanced() {
		t.Fatal("a failed Acquire must not count as a checkout")
	}
}

// --- Exec result mapping --------------------------------------------------

// Runner.Exec has two distinct failure shapes and they must not be collapsed:
// ran-and-exited-non-zero is a RESULT (nil error, ExitCode set), while
// substrate-could-not-start is an ERROR (non-nil error, ExitCode -1). Both are
// error Results to the model, but neither may ever surface as success.
func TestBashMapsExecResultsFaithfully(t *testing.T) {
	cases := []struct {
		name      string
		out       sandbox.Output
		err       error
		wantErr   bool
		wantIn    string
		wantCause sandbox.ReleaseCause
	}{
		{
			name:      "success",
			out:       sandbox.Output{Combined: []byte("hello\n"), ExitCode: 0},
			wantIn:    "hello",
			wantCause: sandbox.CauseReleased,
		},
		{
			name:      "ran and exited non-zero",
			out:       sandbox.Output{Combined: []byte("boom\n"), ExitCode: 3},
			wantErr:   true,
			wantIn:    "exit status 3",
			wantCause: sandbox.CauseReleased,
		},
		{
			name:      "substrate could not start the command",
			out:       sandbox.Output{ExitCode: -1},
			err:       errors.New("docker: cannot connect to the daemon"),
			wantErr:   true,
			wantIn:    "cannot connect to the daemon",
			wantCause: sandbox.CauseEarlyReturn,
		},
		{
			name:      "timed out",
			out:       sandbox.Output{Combined: []byte("part\n"), ExitCode: -1, TimedOut: true},
			wantErr:   true,
			wantIn:    "timed out",
			wantCause: sandbox.CauseEarlyReturn,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sub := &fakeSubstrate{runner: &fakeRunner{out: tc.out, err: tc.err}}
			b := newFakeBash(sub)

			res := b.Execute(context.Background(), `{"command":"whatever"}`)
			if res.IsError != tc.wantErr {
				t.Fatalf("IsError = %v, want %v (output %q)", res.IsError, tc.wantErr, res.Output)
			}
			if !strings.Contains(res.Output, tc.wantIn) {
				t.Fatalf("output %q does not contain %q", res.Output, tc.wantIn)
			}
			// EVERY path returns the checkout. A leak here is a container that
			// outlives its command.
			if !sub.balanced() {
				t.Fatalf("acquire/release imbalance: %d acquired, %d released", sub.acquires, sub.releases)
			}
			if len(sub.causes) != 1 || sub.causes[0] != tc.wantCause {
				t.Fatalf("release causes = %v, want [%s]", sub.causes, tc.wantCause)
			}
		})
	}
}

// The command and working directory reach the substrate verbatim — the tool
// runs nothing itself.
func TestBashExecutesThroughTheSubstrate(t *testing.T) {
	r := &fakeRunner{out: sandbox.Output{Combined: []byte("ok")}}
	sub := &fakeSubstrate{runner: r}
	b := newFakeBash(sub)

	if res := b.Execute(context.Background(), `{"command":"ls -l","working_dir":"/w"}`); res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if r.execs != 1 || r.lastCmd != "ls -l" || r.lastDir != "/w" {
		t.Fatalf("substrate saw execs=%d cmd=%q dir=%q", r.execs, r.lastCmd, r.lastDir)
	}
}

// Bad input is rejected BEFORE anything is checked out, so a malformed call can
// never cost a container.
func TestBashBadArgsAcquireNothing(t *testing.T) {
	for _, args := range []string{`not json`, `{"command":""}`} {
		sub := &fakeSubstrate{runner: &fakeRunner{}}
		b := newFakeBash(sub)
		if res := b.Execute(context.Background(), args); !res.IsError {
			t.Fatalf("args %q should be an error result", args)
		}
		if sub.acquires != 0 {
			t.Fatalf("args %q acquired a Runner", args)
		}
	}
}

// --- teardown -------------------------------------------------------------

// ReleaseSandboxes is the seam every binding's LoopTeardown calls. It must
// reach the bash tool inside the loop's registry and release the pool, or the
// reaper goroutine and every warm container outlive the loop.
func TestReleaseSandboxesClosesBashSubstrate(t *testing.T) {
	sub := &fakeSubstrate{runner: &fakeRunner{}}
	b := newFakeBash(sub)

	reg := NewRegistry()
	reg.Register(b)
	reg.Register(NewReadFile())

	if err := ReleaseSandboxes(context.Background(), reg); err != nil {
		t.Fatalf("ReleaseSandboxes: %v", err)
	}
	if sub.closes != 1 {
		t.Fatalf("substrate closes = %d, want 1", sub.closes)
	}

	// Idempotent: teardown may run on an error path AND on the completion
	// goroutine (see runtime.launchLoop), so a second call must be a no-op.
	if err := ReleaseSandboxes(context.Background(), reg); err != nil {
		t.Fatalf("second ReleaseSandboxes: %v", err)
	}
	if sub.closes != 1 {
		t.Fatalf("substrate closed twice (%d) — teardown is not idempotent", sub.closes)
	}
}

// A shell session runs MANY loops through ONE shared registry, so a loop's
// teardown must not permanently disable bash for the rest of the session: the
// next call re-opens a pool over the same frozen Service.
func TestBashUsableAfterTeardown(t *testing.T) {
	svc, err := sandbox.NewService(hostAuthorizedConfig())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	b := NewBash(svc)

	if res := b.Execute(context.Background(), `{"command":"echo one"}`); res.IsError {
		t.Fatalf("first call: %s", res.Output)
	}
	if err := ReleaseSandboxes(context.Background(), registryWith(b)); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if res := b.Execute(context.Background(), `{"command":"echo two"}`); res.IsError {
		t.Fatalf("call after teardown: %s", res.Output)
	}
}

func registryWith(ts ...Tool) *Registry {
	r := NewRegistry()
	for _, t := range ts {
		r.Register(t)
	}
	return r
}

// hostAuthorizedConfig is the operator off-switch, built by hand: the only
// configuration under which a unit test may run a real command on this machine.
func hostAuthorizedConfig() sandbox.Config {
	return sandbox.Config{Contained: false, Handler: sandbox.HandlerHost}
}

// The scrub applies on the host substrate too (T2). This is the tool-level
// proof that the bash tool inherits it rather than shelling out itself.
func TestBashScrubsAmbientEnvironment(t *testing.T) {
	t.Setenv("FUSE_TEST_SECRET", "s3cret")

	svc, err := sandbox.NewService(hostAuthorizedConfig())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	res := NewBash(svc).Execute(context.Background(), `{"command":"env"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if strings.Contains(res.Output, "s3cret") {
		t.Fatalf("the ambient environment leaked into a bash command:\n%s", res.Output)
	}
}
