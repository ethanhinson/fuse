package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/loopauth"
)

// timeInPast is an already-elapsed deadline, so ctx.Err() reports
// DeadlineExceeded without the test spending any wall clock on it.
func timeInPast() time.Time { return time.Now().Add(-time.Second) }

// fakeLookPath builds a lookPath double reporting exactly the named binaries as
// present, at a predictable absolute path.
func fakeLookPath(present ...string) func(string) (string, error) {
	set := make(map[string]bool, len(present))
	for _, p := range present {
		set[p] = true
	}
	return func(name string) (string, error) {
		if set[name] {
			return "/usr/local/bin/" + name, nil
		}
		return "", exec.ErrNotFound
	}
}

func TestContainerDetectionOrder(t *testing.T) {
	tests := []struct {
		name    string
		present []string
		want    string
		wantErr bool
	}{
		{name: "all three present prefers docker", present: []string{"docker", "nerdctl", "podman"}, want: "docker"},
		{name: "nerdctl beats podman", present: []string{"nerdctl", "podman"}, want: "nerdctl"},
		{name: "only podman", present: []string{"podman"}, want: "podman"},
		{name: "only docker", present: []string{"docker"}, want: "docker"},
		{name: "none found is a construction error", present: nil, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, err := newContainerHandler(DefaultConfig(), withLookPath(fakeLookPath(tc.present...)))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want construction error when no container CLI is present, got handler %#v", h)
				}
				if h != nil {
					// A non-nil handler on the error path invites a caller to
					// use it anyway — which would be a silent host fallback.
					t.Fatalf("want nil handler on the error path, got %#v", h)
				}
				if !errors.Is(err, ErrNoContainerRuntime) {
					t.Fatalf("want ErrNoContainerRuntime, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("newContainerHandler: %v", err)
			}
			if got := h.Runtime(); got != tc.want {
				t.Fatalf("Runtime() = %q, want %q", got, tc.want)
			}
			if got := h.Name(); got != HandlerContainer {
				t.Fatalf("Name() = %q, want %q", got, HandlerContainer)
			}
		})
	}
}

// recordingRun captures the argv of the last RUN invocation. It deliberately
// ignores the pre-pull `pull <image>` invocation (change 0077): the "nothing
// ran" assertions on a working_dir refusal mean "no container run happened", and
// the pre-pull at Acquire is a separate, expected fact recorded in pullArgs.
type recordingRun struct {
	name string
	args []string
	out  []byte
	code int
	err  error

	// pullArgs records the last `pull` invocation's args, and pulls counts how
	// many pulls happened, so a test can assert the single-flight pre-pull ran
	// exactly once without polluting the run assertions.
	pullArgs []string
	pulls    int
}

func (r *recordingRun) run(_ context.Context, name string, args ...string) ([]byte, int, error) {
	if len(args) > 0 && args[0] == "pull" {
		r.pullArgs = append([]string(nil), args...)
		r.pulls++
		// The pre-pull SUCCEEDS by default: it is a precondition of reaching Exec,
		// and the run-level out/code/err a test configures describe the container
		// RUN, not the pull. A test exercising a failing pull uses pullErrRun.
		return nil, 0, nil
	}
	r.name = name
	r.args = append([]string(nil), args...)
	return r.out, r.code, r.err
}

// trustedTestRoot returns a real, existing directory with symlinks already
// resolved, standing in for the repo root a composition root resolves at
// startup. Resolving here (rather than trusting t.TempDir's raw value) matters
// on macOS, where TempDir hands back a /var → /private/var symlink: the
// handler canonicalises its root, so a golden built from the raw path would
// compare two spellings of the same directory.
func trustedTestRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(t.TempDir()): %v", err)
	}
	return root
}

func newTestHandler(t *testing.T, cfg Config, rec *recordingRun, present ...string) *containerHandler {
	t.Helper()
	if len(present) == 0 {
		present = []string{"docker"}
	}
	h, err := newContainerHandler(cfg,
		withLookPath(fakeLookPath(present...)),
		withExecRunner(rec.run),
		// Every handler under test has a trusted root, because in production
		// every handler does: it is what the composition root mounts.
		withTrustedRoot(trustedTestRoot(t)),
	)
	if err != nil {
		t.Fatalf("newContainerHandler: %v", err)
	}
	return h
}

// argValue returns the argument following the first occurrence of flag.
func argValue(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func wantFlag(t *testing.T, args []string, flag, want string) {
	t.Helper()
	got, ok := argValue(args, flag)
	if !ok {
		t.Fatalf("argv carries no %s flag: %#v", flag, args)
	}
	if got != want {
		t.Fatalf("%s = %q, want %q (argv: %#v)", flag, got, want, args)
	}
}

// FINDING A. With working_dir unset — the default, and what a model that never
// sets the optional argument produces — the working tree must STILL be
// mounted. ADR-0044: "The working tree must be mounted in for the model to see
// the repo it edits." A container with no -v at all is an empty alpine box in
// which nothing the agent was asked to do is possible.
func TestContainerArgvMountsTrustedRootWhenWorkingDirUnset(t *testing.T) {
	rec := &recordingRun{}
	h := newTestHandler(t, DefaultConfig(), rec)

	r, _ := h.Acquire(context.Background(), loopauth.Principal{}, Env{})
	if _, err := r.Exec(context.Background(), "true", ""); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	wantFlag(t, rec.args, "-v", h.root+":"+containerWorkspace)
	wantFlag(t, rec.args, "-w", containerWorkspace)
}

// FINDING B, the security one. A model-authored working_dir must never be the
// bind-mount SOURCE. If it were, {"command":"cat /workspace/.aws/credentials",
// "working_dir":"/Users/<op>"} would mount that subtree read-write, as root,
// into a container the model drives — recovering by filesystem exactly the
// credential access the env-scrub closed. ADR-0044 puts the root of trust in
// the loop-start context, "never from model output (not the `command`, not
// `working_dir`)".
func TestContainerRefusesWorkingDirOutsideTrustedRoot(t *testing.T) {
	cases := map[string]func(root string) string{
		"the whole host":            func(string) string { return "/" },
		"an operator home":          func(string) string { return "/Users/someone" },
		"one level above the mount": filepath.Dir,
		"a real dir that is not ours": func(string) string {
			return os.TempDir()
		},
		"absolute traversal out": func(root string) string { return root + "/../.." },
		"relative traversal out": func(string) string { return "../.." },
		"the parent, relatively": func(string) string { return ".." },
	}

	for name, pick := range cases {
		t.Run(name, func(t *testing.T) {
			rec := &recordingRun{}
			h := newTestHandler(t, DefaultConfig(), rec)
			target := pick(h.root)

			r, _ := h.Acquire(context.Background(), loopauth.Principal{}, Env{})
			out, err := r.Exec(context.Background(), "cat /workspace/.aws/credentials", target)

			if err == nil {
				t.Fatalf("working_dir %q was accepted; argv: %#v", target, rec.args)
			}
			if !errors.Is(err, ErrWorkingDirRefused) {
				t.Fatalf("error = %v, want it to wrap ErrWorkingDirRefused", err)
			}
			if out.ExitCode != -1 {
				t.Fatalf("ExitCode = %d, want -1 so an ExitCode-only caller fails closed", out.ExitCode)
			}
			// The load-bearing assertion: nothing ran. A refusal that still
			// invoked the CLI would have already created the mount.
			if rec.name != "" || rec.args != nil {
				t.Fatalf("the refusal still invoked %q with %#v", rec.name, rec.args)
			}
		})
	}
}

// A legitimate subdirectory of the trusted root is honoured — but as a
// SUBPATH: the mount source is unchanged, and only the in-container -w moves.
func TestContainerWorkingDirSubpathMovesWorkdirNotMount(t *testing.T) {
	rec := &recordingRun{}
	h := newTestHandler(t, DefaultConfig(), rec)

	sub := filepath.Join(h.root, "internal", "tools")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	for _, given := range []string{sub, filepath.Join("internal", "tools")} {
		t.Run(given, func(t *testing.T) {
			rec.name, rec.args = "", nil
			r, _ := h.Acquire(context.Background(), loopauth.Principal{}, Env{})
			if _, err := r.Exec(context.Background(), "true", given); err != nil {
				t.Fatalf("Exec: %v", err)
			}
			wantFlag(t, rec.args, "-v", h.root+":"+containerWorkspace)
			wantFlag(t, rec.args, "-w", containerWorkspace+"/internal/tools")
		})
	}
}

// An in-tree path that is not a directory is refused here rather than at the
// daemon, where it would surface as an opaque failure of a container that had
// already been created.
func TestContainerRefusesWorkingDirThatIsNotADirectory(t *testing.T) {
	rec := &recordingRun{}
	h := newTestHandler(t, DefaultConfig(), rec)

	file := filepath.Join(h.root, "README.md")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	r, _ := h.Acquire(context.Background(), loopauth.Principal{}, Env{})
	out, err := r.Exec(context.Background(), "true", file)

	if !errors.Is(err, ErrWorkingDirRefused) {
		t.Fatalf("error = %v, want it to wrap ErrWorkingDirRefused", err)
	}
	if out.ExitCode != -1 {
		t.Fatalf("ExitCode = %d, want -1", out.ExitCode)
	}
	if rec.name != "" || rec.args != nil {
		t.Fatalf("the refusal still invoked %q with %#v", rec.name, rec.args)
	}
}

// A symlink inside the tree pointing OUT of it is the traversal attempt that
// string-prefix checks miss, so the containment check canonicalises before it
// compares.
func TestContainerRefusesSymlinkedWorkingDirEscape(t *testing.T) {
	rec := &recordingRun{}
	h := newTestHandler(t, DefaultConfig(), rec)

	link := filepath.Join(h.root, "escape")
	if err := os.Symlink("/", link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	r, _ := h.Acquire(context.Background(), loopauth.Principal{}, Env{})
	out, err := r.Exec(context.Background(), "true", link)

	if err == nil {
		t.Fatalf("symlinked escape was accepted; argv: %#v", rec.args)
	}
	if !errors.Is(err, ErrWorkingDirRefused) {
		t.Fatalf("error = %v, want it to wrap ErrWorkingDirRefused", err)
	}
	if out.ExitCode != -1 {
		t.Fatalf("ExitCode = %d, want -1", out.ExitCode)
	}
	if rec.name != "" || rec.args != nil {
		t.Fatalf("the refusal still invoked %q with %#v", rec.name, rec.args)
	}
}

// With no trusted root at all (the composition root could not resolve one) the
// safe answer is an unmounted workspace — never promoting the model's path to
// a mount source because no trusted one was available.
func TestContainerWithoutTrustedRootMountsNothingAndRefusesWorkingDir(t *testing.T) {
	rec := &recordingRun{}
	h, err := newContainerHandler(DefaultConfig(),
		withLookPath(fakeLookPath("docker")),
		withExecRunner(rec.run),
		withTrustedRoot(""),
	)
	if err != nil {
		t.Fatalf("newContainerHandler: %v", err)
	}

	r, _ := h.Acquire(context.Background(), loopauth.Principal{}, Env{})
	if _, err := r.Exec(context.Background(), "true", ""); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got, ok := argValue(rec.args, "-v"); ok {
		t.Fatalf("argv mounts %q with no trusted root: %#v", got, rec.args)
	}
	wantFlag(t, rec.args, "-w", containerWorkspace)

	rec.name, rec.args = "", nil
	out, err := r.Exec(context.Background(), "true", "/Users/someone")
	if err == nil {
		t.Fatalf("working_dir was accepted with no trusted root; argv: %#v", rec.args)
	}
	if !errors.Is(err, ErrNoTrustedRoot) {
		t.Fatalf("error = %v, want it to wrap ErrNoTrustedRoot", err)
	}
	if out.ExitCode != -1 {
		t.Fatalf("ExitCode = %d, want -1", out.ExitCode)
	}
	if rec.name != "" || rec.args != nil {
		t.Fatalf("the refusal still invoked %q with %#v", rec.name, rec.args)
	}
}

// LOCAL PARITY (change 0065). This golden is also the parity assertion for the
// per-tenant mount root: newTestHandler configures NO tenantRootSource, which is
// the local single-tenant deployment and the default everywhere else, and the
// argv below is byte-for-byte the argv this test asserted before 0065 existed.
//
// The parity is spelled out rather than left implicit because its failure mode
// is silent and severe in one direction: if a future change ever makes the
// resolver path the default — by defaulting h.tenantRoots to a non-nil value, or
// by letting the nil-interface trap wrap a nil *TenantRoots — every resolver-less
// deployment would resolve to no root, mount nothing, and hand the model an empty
// box that merely LOOKS like its workspace. The explicit check on h.tenantRoots
// below names that regression at the point it would occur; the -v element makes
// it visible in the argv too.
func TestContainerExecArgvGolden(t *testing.T) {
	rec := &recordingRun{}
	cfg := DefaultConfig()
	cfg.Image = "example.invalid/img:1.2.3"
	h := newTestHandler(t, cfg, rec)

	// The precondition the golden below encodes: no per-tenant resolver.
	if h.tenantRoots != nil {
		t.Fatalf("handler holds a tenant-root resolver (%#v) with none configured; "+
			"the resolver-less path must remain the default", h.tenantRoots)
	}

	env := Env{Allow: map[string]string{"PATH": "/usr/bin", "HOME": "/root", "LANG": "C"}}
	r, err := h.Acquire(context.Background(), loopauth.Principal{}, env)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// working_dir unset: the default case, and the one a model produces when it
	// leaves the optional argument alone.
	if _, err := r.Exec(context.Background(), "echo hi", ""); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if rec.name != "docker" {
		t.Fatalf("cli = %q, want docker", rec.name)
	}
	want := []string{
		"run", "--rm", "-i",
		// env keys sorted for determinism; ALWAYS the K=V pair form
		"--env", "HOME=/root",
		"--env", "LANG=C",
		"--env", "PATH=/usr/bin",
		// The mount source is the TRUSTED root the composition root declared,
		// never anything the model said — and, with no resolver configured,
		// it is the HANDLER's single root (h.root) unchanged, not a per-tenant
		// child of it. That h.root, and not some resolver-derived path, is
		// what appears here IS the 0065 local-parity guarantee.
		"-v", h.root + ":/workspace",
		"-w", "/workspace",
		// --pull=never (change 0077): the image is acquired by the pre-pull, so
		// run must never trigger one. With DefaultConfig (local posture, no caps)
		// this is the ONLY 0077 addition — the uncontained baseline is otherwise
		// byte-identical to the #0063 argv.
		"--pull=never",
		"example.invalid/img:1.2.3",
		"/bin/sh", "-c", "echo hi",
	}
	if !reflect.DeepEqual(rec.args, want) {
		t.Fatalf("argv mismatch\n got: %#v\nwant: %#v", rec.args, want)
	}
}

func TestContainerExecNeverUsesBareEnvFlag(t *testing.T) {
	// A bare `--env K` tells every one of docker/nerdctl/podman to copy the
	// HOST's value of K into the container: exactly the ambient-inheritance
	// hole this package exists to close.
	rec := &recordingRun{}
	h := newTestHandler(t, DefaultConfig(), rec)

	r, _ := h.Acquire(context.Background(), loopauth.Principal{}, Env{Allow: map[string]string{"PATH": "/usr/bin"}})
	if _, err := r.Exec(context.Background(), "true", ""); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	for i, a := range rec.args {
		if a != "--env" {
			continue
		}
		if i+1 >= len(rec.args) {
			t.Fatalf("--env is the last argv element: %#v", rec.args)
		}
		if !strings.Contains(rec.args[i+1], "=") {
			t.Fatalf("bare --env %q passes the host value through: %#v", rec.args[i+1], rec.args)
		}
	}
}

func TestContainerExecNoHostVarLeaksIntoArgv(t *testing.T) {
	t.Setenv("FUSE_TEST_SECRET", "super-secret-value")

	rec := &recordingRun{}
	h := newTestHandler(t, DefaultConfig(), rec)

	// The env the Runner is given is the resolved allowlist, which excludes the
	// ambient secret. Nothing in the container path may reconsult os.Environ.
	env := ResolveEnv(nil, func(k string) (string, bool) {
		if k == "PATH" {
			return "/usr/bin", true
		}
		return "", false
	})
	r, _ := h.Acquire(context.Background(), loopauth.Principal{}, env)
	if _, err := r.Exec(context.Background(), "true", ""); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	joined := strings.Join(append([]string{rec.name}, rec.args...), "\x00")
	for _, needle := range []string{"FUSE_TEST_SECRET", "super-secret-value"} {
		if strings.Contains(joined, needle) {
			t.Fatalf("host variable %q leaked into argv: %#v", needle, rec.args)
		}
	}
}

func TestContainerImageSelection(t *testing.T) {
	t.Run("default when unset", func(t *testing.T) {
		rec := &recordingRun{}
		h := newTestHandler(t, DefaultConfig(), rec)
		r, _ := h.Acquire(context.Background(), loopauth.Principal{}, Env{})
		if _, err := r.Exec(context.Background(), "true", ""); err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if !containsArg(rec.args, DefaultContainerImage) {
			t.Fatalf("want default image %q in argv, got %#v", DefaultContainerImage, rec.args)
		}
	})

	t.Run("override honoured", func(t *testing.T) {
		rec := &recordingRun{}
		cfg := DefaultConfig()
		cfg.Image = "ghcr.io/example/custom:v9"
		h := newTestHandler(t, cfg, rec)
		r, _ := h.Acquire(context.Background(), loopauth.Principal{}, Env{})
		if _, err := r.Exec(context.Background(), "true", ""); err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if !containsArg(rec.args, "ghcr.io/example/custom:v9") {
			t.Fatalf("want override image in argv, got %#v", rec.args)
		}
		if containsArg(rec.args, DefaultContainerImage) {
			t.Fatalf("default image still present alongside the override: %#v", rec.args)
		}
	})
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// TestContainerExecErrorSemantics pins the cross-substrate contract shared with
// the host handler: a command that RAN is never an error, and every failure
// path leaves a non-zero ExitCode so a caller reading only ExitCode fails
// closed.
func TestContainerExecErrorSemantics(t *testing.T) {
	t.Run("non-zero exit is not an error", func(t *testing.T) {
		rec := &recordingRun{out: []byte("nope\n"), code: 42}
		h := newTestHandler(t, DefaultConfig(), rec)
		r, _ := h.Acquire(context.Background(), loopauth.Principal{}, Env{})

		out, err := r.Exec(context.Background(), "exit 42", "")
		if err != nil {
			t.Fatalf("want nil error for a command that ran, got %v", err)
		}
		if out.ExitCode != 42 {
			t.Fatalf("ExitCode = %d, want 42", out.ExitCode)
		}
		if string(out.Combined) != "nope\n" {
			t.Fatalf("Combined = %q", out.Combined)
		}
		if out.TimedOut {
			t.Fatal("TimedOut should be false")
		}
	})

	t.Run("substrate could not start the command", func(t *testing.T) {
		rec := &recordingRun{code: 0, err: errors.New("cannot connect to the docker daemon")}
		h := newTestHandler(t, DefaultConfig(), rec)
		r, _ := h.Acquire(context.Background(), loopauth.Principal{}, Env{})

		out, err := r.Exec(context.Background(), "true", "")
		if err == nil {
			t.Fatal("want a non-nil error when the substrate cannot start the command")
		}
		if out.ExitCode != -1 {
			t.Fatalf("ExitCode = %d, want -1 so an ExitCode-only caller fails closed", out.ExitCode)
		}
	})

	t.Run("deadline kill is a result, not an error", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), timeInPast())
		defer cancel()

		rec := &recordingRun{code: 137, err: nil}
		h := newTestHandler(t, DefaultConfig(), rec)
		r, _ := h.Acquire(context.Background(), loopauth.Principal{}, Env{})

		out, err := r.Exec(ctx, "sleep 100", "")
		if err != nil {
			t.Fatalf("want nil error on a deadline kill, got %v", err)
		}
		if !out.TimedOut {
			t.Fatal("want TimedOut = true")
		}
		if out.ExitCode == 0 {
			t.Fatal("ExitCode must never be left 0 on a failure path")
		}
	})
}

func TestContainerReleaseIsIdempotentNoOp(t *testing.T) {
	rec := &recordingRun{}
	h := newTestHandler(t, DefaultConfig(), rec)
	r, _ := h.Acquire(context.Background(), loopauth.Principal{}, Env{})

	for i := 0; i < 3; i++ {
		if err := r.Release(context.Background()); err != nil {
			t.Fatalf("Release #%d: %v", i, err)
		}
	}
	// Release owns no daemon interaction of its own: the run --rm invocation is
	// self-cleaning, and reuse is the pool's concern (T6).
	if rec.name != "" {
		t.Fatalf("Release invoked the CLI (%q %#v); it must not", rec.name, rec.args)
	}
}

// runCommand resolves the container CLI CLIENT's own environment (the daemon-
// addressing passthrough, e.g. DOCKER_HOST) through an injected lookup rather
// than always re-reading the real process environment, so the same seam
// WithEnvLookup governs for the sandboxed command's environment also governs
// this half.
func TestRunCommandResolvesClientEnvThroughInjectedLookup(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///real-daemon.sock")

	lookup := func(key string) (string, bool) {
		if key == "DOCKER_HOST" {
			return "unix:///fake-daemon.sock", true
		}
		return "", false
	}

	out, code, err := runCommand(context.Background(), "env", lookup)
	if err != nil {
		t.Fatalf("runCommand: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (output: %s)", code, out)
	}
	got := string(out)
	if !strings.Contains(got, "DOCKER_HOST=unix:///fake-daemon.sock") {
		t.Fatalf("client env = %q, want it to carry the injected lookup's value", got)
	}
	if strings.Contains(got, "real-daemon.sock") {
		t.Fatalf("client env = %q, leaked the real process environment despite an injected lookup", got)
	}
}

// newContainerHandler wires its default execRunner to resolve the client env
// through h.envLookup, so a Service-level WithEnvLookup reaches the CLI
// client's own environment and not only the sandboxed command's.
func TestNewContainerHandlerDefaultRunUsesEnvLookup(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///real-daemon.sock")

	lookup := func(key string) (string, bool) {
		if key == "DOCKER_HOST" {
			return "unix:///fake-daemon.sock", true
		}
		return "", false
	}

	h, err := newContainerHandler(DefaultConfig(),
		withLookPath(fakeLookPath("docker")),
		withContainerEnvLookup(lookup),
	)
	if err != nil {
		t.Fatalf("newContainerHandler: %v", err)
	}

	out, code, err := h.run(context.Background(), "env")
	if err != nil {
		t.Fatalf("h.run: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (output: %s)", code, out)
	}
	got := string(out)
	if !strings.Contains(got, "DOCKER_HOST=unix:///fake-daemon.sock") {
		t.Fatalf("client env = %q, want it to carry the injected lookup's value", got)
	}
	if strings.Contains(got, "real-daemon.sock") {
		t.Fatalf("client env = %q, leaked the real process environment despite an injected lookup", got)
	}
}

// --- change 0064: the `--network none` floor ---------------------------------

// egressHandler builds a container handler carrying a trusted egress posture,
// applied through withEgress exactly as the composition root applies it (see
// NewService), rather than by poking the field.
func egressHandler(t *testing.T, rec *recordingRun, e Egress, limits Limits) *containerHandler {
	t.Helper()
	h, err := newContainerHandler(DefaultConfig(),
		withLookPath(fakeLookPath("docker")),
		withExecRunner(rec.run),
		withTrustedRoot(trustedTestRoot(t)),
		withLimits(limits),
		withEgress(e),
	)
	if err != nil {
		t.Fatalf("newContainerHandler: %v", err)
	}
	return h
}

// egressRunArgv runs one Exec and returns the recorded container-RUN argv. The
// pre-pull invocation is recorded separately on rec (see recordingRun).
func egressRunArgv(t *testing.T, h *containerHandler, rec *recordingRun, env Env) []string {
	t.Helper()
	r, err := h.Acquire(context.Background(), loopauth.Principal{}, env)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := r.Exec(context.Background(), "true", ""); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	return rec.args
}

// Under the default posture (EgressAllowAll — also the zero value, which is what
// every pre-0064 call site produces) argv carries NO network flag at all. The
// floor is opt-in; a handler that emitted it unconditionally would break every
// operator who never configured egress, and this is the test that says so.
func TestContainerArgvNoNetworkFlagUnderAllowAll(t *testing.T) {
	for name, e := range map[string]Egress{
		"zero value":         {},
		"explicit allow-all": {Mode: EgressAllowAll},
		// An allowlist without the enforcing mode is inert: the mode is the only
		// thing that turns the floor on.
		"allow-all with entries": {Mode: EgressAllowAll, Allow: []AllowEntry{{Host: "example.com", Port: 443}}},
	} {
		t.Run(name, func(t *testing.T) {
			rec := &recordingRun{}
			argv := egressRunArgv(t, egressHandler(t, rec, e, Limits{}), rec, Env{})
			if contains(argv, "--network") {
				t.Fatalf("allow-all argv carries --network: %#v", argv)
			}
		})
	}
}

// Under EgressEnforce the floor is present, as the `--network none` PAIR — not
// `--network=none`, because the pair form is what all three detected CLIs share
// and what the rest of this builder emits.
func TestContainerArgvNetworkNoneUnderEnforce(t *testing.T) {
	for name, e := range map[string]Egress{
		// The deny-all state: enforcing with nothing declared. The floor is a
		// property of the MODE, never of the allowlist being non-empty.
		"enforce, empty allowlist": {Mode: EgressEnforce},
		"enforce with entries":     {Mode: EgressEnforce, Allow: []AllowEntry{{Host: "example.com", Port: 443}}},
	} {
		t.Run(name, func(t *testing.T) {
			rec := &recordingRun{}
			argv := egressRunArgv(t, egressHandler(t, rec, e, Limits{}), rec, Env{})
			if !containsPair(argv, "--network", "none") {
				t.Fatalf("enforce argv missing the --network none pair: %#v", argv)
			}
		})
	}
}

// The ordering guarantee, asserted as a WHOLE-ARGV golden: the floor lands after
// the cgroup caps and before the --env pairs, and every neighbour the reconcile
// named — the --env pairs, the mount, -w, and the trailing --pull=never — keeps
// the position it had before 0064.
func TestContainerArgvEnforceOrderingIsUnchangedAroundTheFloor(t *testing.T) {
	rec := &recordingRun{}
	h := egressHandler(t, rec, Egress{Mode: EgressEnforce}, Limits{
		MemoryBytes: i64(2 << 30),
		CPUs:        str("2.0"),
	})
	// 0065 local parity, on the fully-loaded argv as well as the bare one: the
	// egress/limits handler configures no tenant-root resolver either, so the
	// mount element below is still h.root. Asserted here too because this is the
	// golden that pins ORDER — a resolver accidentally made default would change
	// the mount's VALUE while leaving every position intact, and a value-blind
	// ordering check would not notice.
	if h.tenantRoots != nil {
		t.Fatalf("handler holds a tenant-root resolver (%#v) with none configured", h.tenantRoots)
	}

	argv := egressRunArgv(t, h, rec, Env{Allow: map[string]string{"PATH": "/usr/bin", "HOME": "/root"}})

	want := []string{
		"run", "--rm", "-i",
		"--memory", "2147483648", "--memory-swap", "2147483648",
		"--cpus", "2.0",
		"--network", "none",
		"--env", "HOME=/root",
		"--env", "PATH=/usr/bin",
		"-v", h.root + ":" + containerWorkspace,
		"-w", containerWorkspace,
		"--pull=never",
		DefaultContainerImage, "/bin/sh", "-c", "true",
	}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("argv =\n%#v\nwant\n%#v", argv, want)
	}
}

// THE floor must never reach the pre-pull. A `pull` under --network none cannot
// reach a registry, so an image acquisition that inherited the floor would fail
// every cold start with an opaque network error — and, with --pull=never on the
// run argv, would take the whole substrate down with it.
//
// This asserts the structural reason as well as the symptom: prePull/runPull
// build their own argv (`pull <image>`, straight to h.run) and never call
// containerRunner.argv, so there is no builder for the floor to leak through.
func TestContainerPrePullArgvCarriesNoNetworkFloor(t *testing.T) {
	rec := &recordingRun{}
	h := egressHandler(t, rec, Egress{Mode: EgressEnforce, Allow: []AllowEntry{{Host: "example.com", Port: 443}}}, Limits{})

	// Acquire alone performs the pre-pull; the run argv is asserted elsewhere.
	if _, err := h.Acquire(context.Background(), loopauth.Principal{}, Env{}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	if rec.pulls != 1 {
		t.Fatalf("pulls = %d, want exactly 1", rec.pulls)
	}
	if contains(rec.pullArgs, "--network") {
		t.Fatalf("pre-pull argv carries the egress floor: %#v", rec.pullArgs)
	}
	if !reflect.DeepEqual(rec.pullArgs, []string{"pull", DefaultContainerImage}) {
		t.Fatalf("pre-pull argv = %#v, want exactly [pull <image>] — it must not share the run builder", rec.pullArgs)
	}
}

// withRogueEgress is a caller-supplied containerOption applied BEFORE the ones
// NewService appends. It stands in for any option a caller (or a future seam a
// model can reach) could pass to try to lower the floor.
func withRogueEgress(e Egress) ServiceOption {
	return func(o *serviceOptions) { o.containerOpts = append(o.containerOpts, withEgress(e)) }
}

// Trust ordering (learning `trusted-root-never-model-selectable`): the egress
// posture is established by the trusted side and applied LAST, exactly as
// withTrustedRoot is, so a caller-supplied option cannot put an enforcing config
// back on allow-all. If withEgress were appended before the caller's options,
// the rogue allow-all below would win and the floor would silently vanish.
func TestServiceAppliesTrustedEgressLastSoCallersCannotLowerTheFloor(t *testing.T) {
	rec := &recordingRun{}
	cfg := DefaultConfig()
	cfg.Egress = Egress{Mode: EgressEnforce}

	svc, err := NewService(cfg,
		withRogueEgress(Egress{Mode: EgressAllowAll}),
		withContainerLookPath(fakeLookPath("docker")),
		withContainerExec(rec.run),
		WithTrustedRoot(trustedTestRoot(t)),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	h, ok := svc.handler.(*containerHandler)
	if !ok {
		t.Fatalf("handler = %T, want *containerHandler", svc.handler)
	}
	if h.egress.Mode != EgressEnforce {
		t.Fatalf("handler egress mode = %v, want %v — a caller option lowered the trusted floor", h.egress.Mode, EgressEnforce)
	}
	if argv := egressRunArgv(t, h, rec, Env{}); !containsPair(argv, "--network", "none") {
		t.Fatalf("argv missing the trusted --network none floor: %#v", argv)
	}
}
