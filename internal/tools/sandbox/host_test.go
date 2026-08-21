package sandbox

// These tests live in the internal test package (not sandbox_test) on purpose:
// the load-bearing guard of this change is a package-internal construction
// detail — that the rendered environment slice handed to os/exec is never nil —
// and asserting it directly is far more durable than inferring it from output.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/loopauth"
)

// shellInjected are variables the shell itself creates in its own child
// environment. They are NOT inherited from the parent process, so seeing them
// in `env` output is not an env-scrub failure. Everything else that appears
// must have come from the allowlist.
var shellInjected = map[string]bool{
	"PWD":    true,
	"OLDPWD": true,
	"SHLVL":  true,
	"_":      true,
}

func testPrincipal() loopauth.Principal {
	return loopauth.Principal{Tenant: "t-1", Subject: "s-1"}
}

// acquireHost is the common setup: a host Runner for the test principal over
// env.
func acquireHost(t *testing.T, env Env) Runner {
	t.Helper()
	h := newHostHandler()
	if got := h.Name(); got != "host" {
		t.Fatalf("Name() = %q, want %q", got, "host")
	}
	r, err := h.Acquire(context.Background(), testPrincipal(), env)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() {
		if err := r.Release(context.Background()); err != nil {
			t.Errorf("Release: %v", err)
		}
	})
	return r
}

// TestHostExecDoesNotInheritAmbientEnvironment is THE test of this change.
//
// An ambient secret is set in the test process. A command run through the host
// handler must not be able to see it: the host path applies the same explicit
// allowlist as every other substrate, and never inherits.
func TestHostExecDoesNotInheritAmbientEnvironment(t *testing.T) {
	const secretKey = "FUSE_TEST_SECRET"
	// A value distinctive enough that a substring scan of the raw output is a
	// meaningful check even if the leak arrives with an unparseable shape (a
	// multi-line value, say).
	const secretValue = "hunter2-a4f1c9de-do-not-leak"

	t.Setenv(secretKey, secretValue)
	t.Setenv("AWS_SECRET_ACCESS_KEY", secretValue)
	if v, ok := os.LookupEnv(secretKey); !ok || v != secretValue {
		t.Fatal("precondition: the ambient secret must be set in the test process")
	}

	r := acquireHost(t, ResolveEnvFromOS(nil))

	out, err := r.Exec(context.Background(), "env", "")
	if err != nil {
		t.Fatalf("Exec: %v (output: %s)", err, out.Combined)
	}
	if out.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 (output: %s)", out.ExitCode, out.Combined)
	}

	combined := string(out.Combined)
	if strings.Contains(combined, secretValue) {
		t.Fatalf("ambient secret VALUE leaked into the sandboxed environment:\n%s", combined)
	}

	allowed := ResolveEnvFromOS(nil).Allow
	for _, line := range strings.Split(combined, "\n") {
		key, _, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if key == secretKey || key == "AWS_SECRET_ACCESS_KEY" {
			t.Fatalf("ambient secret %q leaked into the sandboxed environment:\n%s", key, combined)
		}
		if shellInjected[key] {
			continue
		}
		if _, ok := allowed[key]; !ok {
			t.Fatalf("non-allowlisted host variable %q leaked into the sandboxed environment:\n%s", key, combined)
		}
	}

	// The scrub must be a scrub, not a wipe: the allowlist keys that resolved
	// are genuinely present.
	for key, want := range allowed {
		if !strings.Contains(combined, key+"="+want) {
			t.Fatalf("allowlisted %s=%s missing from the sandboxed environment:\n%s", key, want, combined)
		}
	}
}

// TestHostExecEmptyAllowlistStillScrubs covers the degenerate case: an empty
// allowlist must mean "no variables", never "inherit everything".
func TestHostExecEmptyAllowlistStillScrubs(t *testing.T) {
	const secretKey = "FUSE_TEST_SECRET"
	const secretValue = "hunter2-empty-allowlist-do-not-leak"
	t.Setenv(secretKey, secretValue)

	r := acquireHost(t, Env{})

	out, err := r.Exec(context.Background(), "env", "")
	if err != nil {
		t.Fatalf("Exec: %v (output: %s)", err, out.Combined)
	}
	combined := string(out.Combined)
	if strings.Contains(combined, secretValue) || strings.Contains(combined, secretKey) {
		t.Fatalf("ambient secret leaked under an empty allowlist:\n%s", combined)
	}
	for _, line := range strings.Split(combined, "\n") {
		key, _, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if !shellInjected[key] {
			t.Fatalf("variable %q present under an EMPTY allowlist (nil Env means inherit):\n%s", key, combined)
		}
	}
}

// TestRenderEnvIsNeverNil is the structural guard. A nil os/exec Env means
// "inherit the parent process environment"; the renderer must therefore return
// a non-nil slice for every input, including nil and empty ones.
func TestRenderEnvIsNeverNil(t *testing.T) {
	cases := []struct {
		name string
		env  Env
		want []string
	}{
		{name: "zero Env", env: Env{}, want: []string{}},
		{name: "explicitly nil Allow", env: Env{Allow: nil}, want: []string{}},
		{name: "empty Allow", env: Env{Allow: map[string]string{}}, want: []string{}},
		{
			name: "populated",
			env:  Env{Allow: map[string]string{"PATH": "/bin", "HOME": "/h"}},
			want: []string{"HOME=/h", "PATH=/bin"},
		},
		{
			name: "empty value renders as K=",
			env:  Env{Allow: map[string]string{"LANG": ""}},
			want: []string{"LANG="},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderEnv(tc.env)
			if got == nil {
				t.Fatal("renderEnv returned nil; a nil exec.Cmd.Env means INHERIT, which is the leak this package closes")
			}
			if len(got) != len(tc.want) {
				t.Fatalf("renderEnv = %v, want %v", got, tc.want)
			}
			// Deterministic order keeps argv/env goldens stable downstream.
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("renderEnv = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestHostRunnerBindsResolvedEnv proves the Runner carries the env it was
// acquired with all the way to exec.Cmd.Env, non-nil, without reaching back to
// the process environment.
func TestHostRunnerBindsResolvedEnv(t *testing.T) {
	t.Setenv("FUSE_TEST_SECRET", "hunter2-binding-do-not-leak")

	r := acquireHost(t, Env{Allow: map[string]string{"PATH": "/usr/bin:/bin", "FUSE_TEST_MARKER": "present"}})

	hr, ok := r.(*hostRunner)
	if !ok {
		t.Fatalf("Runner is %T, want *hostRunner", r)
	}
	cmd := hr.command(context.Background(), "true", "")
	if cmd.Env == nil {
		t.Fatal("exec.Cmd.Env is nil; nil means INHERIT the parent process environment")
	}
	joined := strings.Join(cmd.Env, "\n")
	if strings.Contains(joined, "FUSE_TEST_SECRET") {
		t.Fatalf("ambient secret reached exec.Cmd.Env: %v", cmd.Env)
	}
	if !strings.Contains(joined, "FUSE_TEST_MARKER=present") {
		t.Fatalf("resolved env not bound to the Runner: %v", cmd.Env)
	}
}

func TestHostExecExitCode(t *testing.T) {
	r := acquireHost(t, ResolveEnvFromOS(nil))

	out, err := r.Exec(context.Background(), "echo to-stdout; echo to-stderr 1>&2; exit 7", "")
	// A non-zero exit is a RESULT, not a substrate failure: it is reported via
	// ExitCode, and err stays nil.
	if err != nil {
		t.Fatalf("Exec returned err %v for a command that ran and exited non-zero", err)
	}
	if out.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want 7", out.ExitCode)
	}
	if out.TimedOut {
		t.Fatal("TimedOut = true for a command that exited on its own")
	}
	combined := string(out.Combined)
	if !strings.Contains(combined, "to-stdout") || !strings.Contains(combined, "to-stderr") {
		t.Fatalf("Combined = %q, want both streams interleaved", combined)
	}
}

func TestHostExecSuccessExitCodeZero(t *testing.T) {
	r := acquireHost(t, ResolveEnvFromOS(nil))

	out, err := r.Exec(context.Background(), "echo ok", "")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out.ExitCode != 0 || out.TimedOut {
		t.Fatalf("Output = %+v, want ExitCode 0 and TimedOut false", out)
	}
	if strings.TrimSpace(string(out.Combined)) != "ok" {
		t.Fatalf("Combined = %q, want %q", out.Combined, "ok")
	}
}

func TestHostExecTimedOut(t *testing.T) {
	r := acquireHost(t, ResolveEnvFromOS(nil))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	out, err := r.Exec(ctx, "sleep 5", "")
	if err != nil {
		t.Fatalf("Exec returned err %v for a timeout; the timeout is reported via TimedOut", err)
	}
	if !out.TimedOut {
		t.Fatalf("TimedOut = false for a command killed by the context deadline (Output = %+v)", out)
	}
	if out.ExitCode == 0 {
		t.Fatal("ExitCode = 0 for a command killed by the context deadline")
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("precondition: ctx.Err() = %v, want DeadlineExceeded", ctx.Err())
	}
}

func TestHostExecWorkingDir(t *testing.T) {
	dir := t.TempDir()
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	r := acquireHost(t, ResolveEnvFromOS(nil))

	out, execErr := r.Exec(context.Background(), "pwd", dir)
	if execErr != nil {
		t.Fatalf("Exec: %v", execErr)
	}
	got, err := filepath.EvalSymlinks(strings.TrimSpace(string(out.Combined)))
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", out.Combined, err)
	}
	if got != want {
		t.Fatalf("pwd = %q, want %q", got, want)
	}
}

// TestHostExecEmptyWorkingDirLeavesDirUnset: an empty workingDir means "the
// process default", not an empty path (which os/exec would reject).
func TestHostExecEmptyWorkingDirLeavesDirUnset(t *testing.T) {
	r := acquireHost(t, ResolveEnvFromOS(nil))

	hr := r.(*hostRunner)
	if dir := hr.command(context.Background(), "true", "").Dir; dir != "" {
		t.Fatalf("Cmd.Dir = %q, want empty", dir)
	}

	out, err := r.Exec(context.Background(), "pwd", "")
	if err != nil || out.ExitCode != 0 {
		t.Fatalf("Exec = %+v, err %v; want a clean run in the process default directory", out, err)
	}
}

// TestHostExecSubstrateFailureReportsError: a command that cannot be started at
// all (a nonexistent working directory) is a substrate failure and DOES return
// an error, distinguishing it from a command that ran and failed.
func TestHostExecSubstrateFailureReportsError(t *testing.T) {
	r := acquireHost(t, ResolveEnvFromOS(nil))

	out, err := r.Exec(context.Background(), "true", filepath.Join(t.TempDir(), "no-such-dir"))
	if err == nil {
		t.Fatalf("Exec returned nil err for an unstartable command (Output = %+v)", out)
	}
	if out.ExitCode == 0 {
		t.Fatalf("ExitCode = 0 for an unstartable command (Output = %+v)", out)
	}
}

// TestHostReleaseIsNoOp: the host substrate owns no resource, so Release is a
// no-op — and repeated Release never errors.
func TestHostReleaseIsNoOp(t *testing.T) {
	h := newHostHandler()
	r, err := h.Acquire(context.Background(), testPrincipal(), ResolveEnvFromOS(nil))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := r.Release(context.Background()); err != nil {
			t.Fatalf("Release #%d: %v", i, err)
		}
	}
	// A released host Runner is still usable; nothing was torn down.
	if _, err := r.Exec(context.Background(), "true", ""); err != nil {
		t.Fatalf("Exec after Release: %v", err)
	}
}

// TestHostAcquirePerPrincipalRunners: Acquire is principal-scoped by
// construction — two principals never share a Runner instance.
func TestHostAcquirePerPrincipalRunners(t *testing.T) {
	h := newHostHandler()
	a, err := h.Acquire(context.Background(), loopauth.Principal{Tenant: "t-a", Subject: "s-a"}, Env{})
	if err != nil {
		t.Fatalf("Acquire a: %v", err)
	}
	b, err := h.Acquire(context.Background(), loopauth.Principal{Tenant: "t-b", Subject: "s-b"}, Env{})
	if err != nil {
		t.Fatalf("Acquire b: %v", err)
	}
	if a == b {
		t.Fatal("two principals were handed the same Runner")
	}
	ra, rb := a.(*hostRunner), b.(*hostRunner)
	if ra.principal == rb.principal {
		t.Fatalf("Runner principals collide: %+v", ra.principal)
	}
}

// hostHandler must satisfy the seam.
var _ Handler = (*hostHandler)(nil)
var _ Runner = (*hostRunner)(nil)
