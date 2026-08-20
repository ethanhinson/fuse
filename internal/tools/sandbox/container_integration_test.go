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

	// The trusted root is declared at CONSTRUCTION, the way a composition root
	// declares it at startup — not per Exec, and never from the arguments of
	// the command being run.
	workdir := trustedTestRoot(t)

	h, err := newContainerHandler(Config{Image: DefaultContainerImage}, withTrustedRoot(workdir))
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

	const marker = "smoke-marker-file"
	if err := os.WriteFile(filepath.Join(workdir, marker), []byte("present"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Every Exec below passes an EMPTY working_dir — the default, and what the
	// bash tool sends when a model leaves the optional argument alone. The
	// trusted root must be mounted anyway.
	// 1. Basic exec: `echo hi` succeeds and the output contains "hi".
	out, err := r.Exec(ctx, "echo hi", "")
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
	out, err = r.Exec(ctx, "pwd && ls", "")
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
	out, err = r.Exec(ctx, "env", "")
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

	// 4. A working_dir INSIDE the trusted root moves the container's cwd — and
	// only the cwd. The tree the model can see is still the one the trusted
	// root declared.
	if err := os.MkdirAll(filepath.Join(workdir, "sub", "deeper"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	out, err = r.Exec(ctx, "pwd && ls ..", filepath.Join(workdir, "sub", "deeper"))
	if err != nil {
		t.Fatalf("Exec(pwd, subpath): %v (output: %s)", err, out.Combined)
	}
	if want := containerWorkspace + "/sub/deeper"; !strings.Contains(string(out.Combined), want) {
		t.Fatalf("Combined = %q, want cwd %q", out.Combined, want)
	}

	// 5. The containment property, against the real runtime: a working_dir
	// outside the trusted root is REFUSED, and no container is created for it.
	// Before the fix this mounted the named host subtree read-write at
	// /workspace, as root, for a path the MODEL chose.
	out, err = r.Exec(ctx, "ls -la /workspace", "/")
	if err == nil {
		t.Fatalf("working_dir %q was accepted (output: %s)", "/", out.Combined)
	}
	if out.ExitCode != -1 {
		t.Fatalf("ExitCode = %d, want -1", out.ExitCode)
	}
	if len(out.Combined) != 0 {
		t.Fatalf("a refused command produced output (%q); it must never have run", out.Combined)
	}
}
