package sandbox

// TestTenantFilesystemIsolationAgainstRealCLI is the POSITIVE half of change
// 0065's containment story: tenant A writes a file into its own workspace and
// tenant B's container cannot see it.
//
// Every other 0065 test is a refusal — an escape shape that returns
// ErrWorkingDirRefused, an argv that lacks a -v. Refusals prove the door is
// locked; only this test proves the two rooms are actually different rooms.
// Isolation asserted purely through argv construction would still hold if the
// runtime resolved the two mount sources to the same tree, so the property an
// operator actually cares about has to be observed against a real runtime once.
//
// Like container_integration_test.go, it is GATED at runtime on exec.LookPath
// rather than behind a build tag: a developer or CI box with no container CLI
// must still see this package go green, so the absence of a runtime is a loud
// skip and never a red suite. That means this test can be silently absent — the
// negative tests, which always run, are what keep the property covered in the
// meantime.

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

func TestTenantFilesystemIsolationAgainstRealCLI(t *testing.T) {
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

	// The host layout a composition root declares at startup: one parent
	// directory, one child per tenant. The parent is never itself a mount
	// source — mounting it would hand every tenant every sibling, which is the
	// pre-0065 behaviour this change ends.
	parent := trustedTestRoot(t)

	// The service is built the way a hosted deployment builds it: a trusted
	// root for the resolver-less default, plus the per-tenant resolver on top.
	// No fake exec runner — this drives the REAL CLI, which is the point.
	cfg := DefaultConfig() // already the contained container handler
	cfg.Image = DefaultContainerImage
	svc, err := NewService(cfg,
		WithTrustedRoot(parent),
		WithTenantRoots(NewTenantRoots(parent, true)),
	)
	if err != nil {
		t.Skipf("skipping: NewService: %v", err)
	}
	h, ok := svc.handler.(*containerHandler)
	if !ok {
		t.Fatalf("selected handler is %T, want *containerHandler", svc.handler)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Pull up front so a slow or unreachable registry is a skip, not a red
	// suite (same posture as the smoke test).
	if out, err := exec.CommandContext(ctx, found, "pull", DefaultContainerImage).CombinedOutput(); err != nil {
		t.Skipf("skipping: could not pull %s: %v\n%s", DefaultContainerImage, err, out)
	}

	env := ResolveEnvFromOS(nil)
	alice := loopauth.Principal{Tenant: "tenant-alice", Subject: "s-alice"}
	bob := loopauth.Principal{Tenant: "tenant-bob", Subject: "s-bob"}

	const sentinel = "alice-secret.txt"
	const secretBody = "alice-only-payload-must-not-cross-tenants"

	ra, err := h.Acquire(ctx, alice, env)
	if err != nil {
		t.Fatalf("Acquire(alice): %v", err)
	}
	defer func() { _ = ra.Release(context.Background()) }()

	rb, err := h.Acquire(ctx, bob, env)
	if err != nil {
		t.Fatalf("Acquire(bob): %v", err)
	}
	defer func() { _ = rb.Release(context.Background()) }()

	// The two Runners must have resolved DIFFERENT host roots before anything
	// is written. If they had not, everything below would still pass or fail
	// for reasons that have nothing to do with isolation.
	rootA, rootB := runnerMountRoot(ra), runnerMountRoot(rb)
	if rootA == "" || rootB == "" {
		t.Fatalf("a tenant resolved to no mount root at all (alice=%q bob=%q)", rootA, rootB)
	}
	if rootA == rootB {
		t.Fatalf("both tenants resolved to the SAME host root %q", rootA)
	}

	// STEP 1 — alice writes the sentinel, from INSIDE her container, through
	// the real runtime. Writing it host-side would only prove the host layout;
	// writing it through the container proves the mount is read-write and lands
	// in alice's tree, which is what makes step 3 meaningful.
	out, err := ra.Exec(ctx, "printf '%s' "+secretBody+" > "+containerWorkspace+"/"+sentinel, "")
	if err != nil {
		t.Fatalf("Exec(alice write): %v (output: %s)", err, out.Combined)
	}
	if out.ExitCode != 0 {
		t.Fatalf("alice write ExitCode = %d, want 0 (output: %s)", out.ExitCode, out.Combined)
	}

	// STEP 2 — THE ANTI-VACUITY GATE. Alice must be able to read her own file
	// back. Without this, step 3 is worthless: if the write had silently failed
	// (bad mount, read-only tree, wrong path) then "bob sees nothing" would be
	// true of a file that never existed anywhere, and the test would pass while
	// proving nothing at all. Ordering the assertions this way — prove presence,
	// THEN prove absence — is what makes the isolation claim honest.
	out, err = ra.Exec(ctx, "cat "+containerWorkspace+"/"+sentinel, "")
	if err != nil {
		t.Fatalf("Exec(alice read-back): %v (output: %s)", err, out.Combined)
	}
	if out.ExitCode != 0 || !strings.Contains(string(out.Combined), secretBody) {
		t.Fatalf("alice cannot read back her own sentinel (exit %d, output %q); "+
			"the isolation assertion below would be vacuous", out.ExitCode, out.Combined)
	}
	// And it really landed in ALICE's host tree, not somewhere shared.
	if _, err := os.Stat(filepath.Join(rootA, sentinel)); err != nil {
		t.Fatalf("sentinel is not present in alice's host root %q: %v", rootA, err)
	}

	// STEP 3 — THE PROPERTY. Bob's container cannot see alice's file. Asserted
	// on the REAL command's output and exit code, never on argv: argv proves
	// what was requested, the exit code proves what the runtime actually did.
	out, err = rb.Exec(ctx, "cat "+containerWorkspace+"/"+sentinel, "")
	if err != nil {
		t.Fatalf("Exec(bob read): %v (output: %s)", err, out.Combined)
	}
	if out.ExitCode == 0 {
		t.Fatalf("CROSS-TENANT DISCLOSURE: bob read alice's sentinel (exit 0, output %q)", out.Combined)
	}
	if strings.Contains(string(out.Combined), secretBody) {
		t.Fatalf("CROSS-TENANT DISCLOSURE: alice's payload appeared in bob's container: %q", out.Combined)
	}

	// And it is not merely unreadable by name — it is not in bob's tree at all,
	// so a listing does not disclose even its existence.
	out, err = rb.Exec(ctx, "ls -a "+containerWorkspace, "")
	if err != nil {
		t.Fatalf("Exec(bob ls): %v (output: %s)", err, out.Combined)
	}
	if strings.Contains(string(out.Combined), sentinel) {
		t.Fatalf("CROSS-TENANT DISCLOSURE: alice's filename is listable from bob's workspace: %q", out.Combined)
	}

	// STEP 4 — and bob cannot reach alice's tree by NAMING it either. The host
	// path is a real, existing directory; containment must come from the mount
	// boundary and the working_dir refusal, not from the path being absent.
	out, err = rb.Exec(ctx, "ls -la "+containerWorkspace, rootA)
	if err == nil {
		t.Fatalf("bob's working_dir named alice's host root and was ACCEPTED (output: %s)", out.Combined)
	}
	if out.ExitCode != -1 {
		t.Fatalf("ExitCode = %d, want -1", out.ExitCode)
	}
	if len(out.Combined) != 0 {
		t.Fatalf("a refused command produced output (%q); it must never have run", out.Combined)
	}
}
