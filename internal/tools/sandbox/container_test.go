package sandbox

import (
	"context"
	"errors"
	"os/exec"
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

// recordingRun captures the argv of the last invocation.
type recordingRun struct {
	name string
	args []string
	out  []byte
	code int
	err  error
}

func (r *recordingRun) run(_ context.Context, name string, args ...string) ([]byte, int, error) {
	r.name = name
	r.args = append([]string(nil), args...)
	return r.out, r.code, r.err
}

func newTestHandler(t *testing.T, cfg Config, rec *recordingRun, present ...string) *containerHandler {
	t.Helper()
	if len(present) == 0 {
		present = []string{"docker"}
	}
	h, err := newContainerHandler(cfg, withLookPath(fakeLookPath(present...)), withExecRunner(rec.run))
	if err != nil {
		t.Fatalf("newContainerHandler: %v", err)
	}
	return h
}

func TestContainerExecArgvGolden(t *testing.T) {
	rec := &recordingRun{}
	cfg := DefaultConfig()
	cfg.Image = "example.invalid/img:1.2.3"
	h := newTestHandler(t, cfg, rec)

	env := Env{Allow: map[string]string{"PATH": "/usr/bin", "HOME": "/root", "LANG": "C"}}
	r, err := h.Acquire(context.Background(), loopauth.Principal{}, env)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	if _, err := r.Exec(context.Background(), "echo hi", "/work/tree"); err != nil {
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
		"-v", "/work/tree:/workspace",
		"-w", "/workspace",
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
	if _, err := r.Exec(context.Background(), "true", "/w"); err != nil {
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
	if _, err := r.Exec(context.Background(), "true", "/w"); err != nil {
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
		if _, err := r.Exec(context.Background(), "true", "/w"); err != nil {
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
		if _, err := r.Exec(context.Background(), "true", "/w"); err != nil {
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

		out, err := r.Exec(context.Background(), "exit 42", "/w")
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

		out, err := r.Exec(context.Background(), "true", "/w")
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

		out, err := r.Exec(ctx, "sleep 100", "/w")
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
