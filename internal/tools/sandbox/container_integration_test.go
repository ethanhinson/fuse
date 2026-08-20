package sandbox

// TestContainerSmokeAgainstRealCLI is an end-to-end smoke test that drives a
// real container CLI, when one is present on PATH.
//
// It is gated, not build-tagged: a developer or CI box without docker/nerdctl/
// podman installed must still see this package's suite go green, so the test
// skips loudly rather than failing when no CLI is found or the pinned image
// cannot be pulled. This mirrors the "fail-safe, never fail-open" posture of
// the change, applied to test ergonomics rather than security: absence of a
// runtime is never a red suite.
//
// When a CLI IS present, this is the one test in the package that proves the
// env-scrub end to end against a real runtime, rather than against argv
// construction (container_test.go's goldens): a parent-process secret must
// not be observable inside the container.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/loopauth"
)

func TestContainerSmokeAgainstRealCLI(t *testing.T) {
	var found string
	for _, cli := range containerCLIs {
		if _, err := exec.LookPath(cli); err == nil {
			found = cli
			break
		}
	}
	if found == "" {
		t.Skipf("skipping: none of %s found on PATH", strings.Join(containerCLIs[:], ", "))
	}

	h, err := newContainerHandler(Config{Image: DefaultContainerImage})
	if err != nil {
		t.Skipf("skipping: newContainerHandler: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Pull the pinned image up front so a slow/failed pull is reported as a
	// skip (developer offline, registry unreachable) rather than a red suite.
	if out, err := exec.CommandContext(ctx, found, "pull", DefaultContainerImage).CombinedOutput(); err != nil {
		t.Skipf("skipping: could not pull %s: %v\n%s", DefaultContainerImage, err, out)
	}

	const secretKey = "FUSE_TEST_SECRET"
	const secretValue = "hunter2-container-smoke-do-not-leak"
	t.Setenv(secretKey, secretValue)

	principal := loopauth.Principal{Tenant: "t-smoke", Subject: "s-smoke"}
	env := ResolveEnvFromOS(nil)

	r, err := h.Acquire(ctx, principal, env)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() {
		if err := r.Release(context.Background()); err != nil {
			t.Errorf("Release: %v", err)
		}
	}()

	workdir := t.TempDir()
	const marker = "smoke-marker-file"
	if err := os.WriteFile(filepath.Join(workdir, marker), []byte("present"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// 1. Basic exec: `echo hi` succeeds and the output contains "hi".
	out, err := r.Exec(ctx, "echo hi", workdir)
	if err != nil {
		t.Fatalf("Exec(echo hi): %v (output: %s)", err, out.Combined)
	}
	if out.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 (output: %s)", out.ExitCode, out.Combined)
	}
	if !strings.Contains(string(out.Combined), "hi") {
		t.Fatalf("Combined = %q, want it to contain %q", out.Combined, "hi")
	}

	// 2. Working directory is /workspace, and the mounted tree is visible
	// there: `pwd` reports it, and the marker file created on the host before
	// the call is listable from inside.
	out, err = r.Exec(ctx, "pwd && ls", workdir)
	if err != nil {
		t.Fatalf("Exec(pwd && ls): %v (output: %s)", err, out.Combined)
	}
	if out.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 (output: %s)", out.ExitCode, out.Combined)
	}
	combined := string(out.Combined)
	if !strings.Contains(combined, containerWorkspace) {
		t.Fatalf("Combined = %q, want it to report cwd %q", combined, containerWorkspace)
	}
	if !strings.Contains(combined, marker) {
		t.Fatalf("Combined = %q, want it to list the mounted marker file %q", combined, marker)
	}

	// 3. THE point of the change: a parent FUSE_TEST_SECRET is not visible
	// inside the container.
	out, err = r.Exec(ctx, "env", workdir)
	if err != nil {
		t.Fatalf("Exec(env): %v (output: %s)", err, out.Combined)
	}
	envOut := string(out.Combined)
	if strings.Contains(envOut, secretValue) {
		t.Fatalf("ambient secret VALUE leaked into the container:\n%s", envOut)
	}
	for _, line := range strings.Split(envOut, "\n") {
		key, _, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if key == secretKey {
			t.Fatalf("ambient secret key %q leaked into the container:\n%s", secretKey, envOut)
		}
		if shellInjected[key] || key == "HOSTNAME" {
			// HOSTNAME is injected by the container runtime itself (the
			// container's hostname, e.g. its short container ID) — not
			// inherited from the parent process — the same allowance
			// container_test.go's shellInjected set makes for PWD/OLDPWD/
			// SHLVL/_.
			continue
		}
		if _, ok := env.Allow[key]; !ok {
			t.Fatalf("non-allowlisted variable %q leaked into the container:\n%s", key, envOut)
		}
	}
}
