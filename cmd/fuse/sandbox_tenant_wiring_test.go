package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// WHY THIS FILE EXISTS.
//
// The `security-knob-inert-at-composition-root` learning, recorded from change
// #64: a fail-CLOSED security feature can pass its ENTIRE package test suite
// while doing nothing at all in the shipped binary, because every unit test
// constructs the enforcing object by hand and NOTHING wires it in cmd/. #64
// shipped sandbox.NewProxy with zero non-test callers and a fully green suite,
// and `egress.mode: enforce` was a silent total blackout in a real fuse.
//
// Change 0065 ships two knobs of exactly that shape:
//
//	sandbox.WithTenantRoots  — the per-tenant bind-mount source
//	Service.SetHealthHooks   — the substrate-health emitter
//
// and both fail the same invisible way. An unwired resolver leaves every tenant
// on the ONE process-wide trusted root, which is the pre-0065 behaviour this
// change exists to end; an unwired emitter leaves fuse_sandbox_unhealthy_total
// permanently at zero, which reads exactly like a healthy fleet. Neither is
// observable from inside internal/tools/sandbox, so the assertion lives HERE,
// on the composition root, where the wiring either happens or does not.
//
// Every test below is written to FAIL if the corresponding option is dropped
// from cmd/fuse/sandbox.go. That is the point of the file.

// tenantWorkspaceHome points HOME at a temp directory so the hosted workspace
// parent (see hostedWorkspaceParent) is created under test control rather than
// in the developer's real ~/.fuse. It returns the home it installed.
func tenantWorkspaceHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

// TestHostedPostureWiresPerTenantRoots is THE wiring assertion. A hosted fuse —
// the loop servers, which execute on behalf of REMOTE principals — must come
// out of newSandboxService carrying a per-tenant mount resolver. Without one,
// every authenticated tenant's bash shares one bind-mount, which is precisely
// the cross-tenant disclosure change 0065 closes.
//
// Removing sandbox.WithTenantRoots from newSandboxService turns this RED.
func TestHostedPostureWiresPerTenantRoots(t *testing.T) {
	tenantWorkspaceHome(t)
	root := t.TempDir()
	chdirForSandbox(t, root)

	var buf bytes.Buffer
	svc, closeFn := newSandboxService(config.Config{}, true, &buf)
	t.Cleanup(closeFn)
	if svc == nil {
		t.Fatalf("newSandboxService returned nil; diagnostics: %s", buf.String())
	}
	if !svc.Hosted() {
		t.Fatal("hosted=true did not reach the Service")
	}
	// TenantScoped is answered by the SELECTED SUBSTRATE, not by a remembered
	// option — deliberately, so it cannot drift from what Acquire will really
	// do. On a machine with no container runtime there is no substrate to
	// answer, so the question is unanswerable rather than answered "no": skip
	// instead of asserting, so this never reads as a wiring failure when it is
	// really an absent docker.
	if !svc.Available() {
		t.Skip("no container runtime on this machine; the substrate cannot report its mount posture")
	}
	if !svc.TenantScoped() {
		t.Fatalf("HOSTED Service carries NO per-tenant mount resolver — every tenant "+
			"would share the one trusted root %q, the pre-0065 cross-tenant exposure; "+
			"diagnostics: %s", svc.TrustedRoot(), buf.String())
	}
}

// TestLocalPostureKeepsTheSingleTrustedRoot is the other half of the same
// decision, and it is not a formality: the resolver is the HOSTED-profile
// widening, never a replacement. A local CLI has one operator, one tenant and
// one working tree, and silently moving its bash off the repo the operator is
// standing in — onto some ~/.fuse/workspaces/_default box — would break the
// tool for the overwhelmingly common case while isolating nothing.
func TestLocalPostureKeepsTheSingleTrustedRoot(t *testing.T) {
	tenantWorkspaceHome(t)
	root := t.TempDir()
	chdirForSandbox(t, root)

	var buf bytes.Buffer
	svc, closeFn := newSandboxService(config.Config{}, false, &buf)
	t.Cleanup(closeFn)
	if svc == nil {
		t.Fatalf("newSandboxService returned nil; diagnostics: %s", buf.String())
	}
	// No substrate check needed in this direction: a nil handler reports false,
	// which is the value this test wants, so the assertion is sound either way.
	if svc.TenantScoped() {
		t.Fatal("LOCAL Service carries a per-tenant resolver; the single-root path must remain the default")
	}
	if got := svc.TrustedRoot(); got == "" {
		t.Error("local Service has no trusted root; the operator's own working tree must be mounted")
	}
}

// TestHostedWorkspaceParentIsCreatedOwnerOnly pins the HOST-LAYOUT POLICY this
// composition root chose, because the policy is security-bearing on its own:
// the parent is created 0700. A per-tenant tree another uid on the host can
// read is not an isolation boundary, and the parent is what every tenant tree
// inherits its reachability from.
//
// It also pins that the parent is OUTSIDE any tenant's own tree — it is never
// itself a mount source (see sandbox.TenantRoots' "siblings, never nested").
func TestHostedWorkspaceParentIsCreatedOwnerOnly(t *testing.T) {
	home := tenantWorkspaceHome(t)

	var buf bytes.Buffer
	parent := hostedWorkspaceParent(&buf)
	if parent == "" {
		t.Fatalf("hostedWorkspaceParent = \"\"; diagnostics: %s", buf.String())
	}
	if !strings.HasPrefix(parent, home) {
		t.Errorf("parent %q is not under the fuse home %q", parent, home)
	}
	info, err := os.Stat(parent)
	if err != nil || !info.IsDir() {
		t.Fatalf("stat(%q) = %v, %v; want an existing directory", parent, info, err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("workspace parent %q permissions = %#o, want 0700 — a per-tenant tree "+
			"another uid can read is not an isolation boundary", parent, perm)
	}
}

// TestHostedTenantRootsAreDisjointSiblings proves the wired resolver does the
// thing the wiring is FOR, rather than merely being non-nil: two authenticated
// principals with distinct tenants get distinct, non-overlapping host trees,
// and a principal whose tenant is empty gets no tree at all rather than sharing
// one with whatever real tenant happens to be named "_default".
func TestHostedTenantRootsAreDisjointSiblings(t *testing.T) {
	tenantWorkspaceHome(t)
	var buf bytes.Buffer
	parent := hostedWorkspaceParent(&buf)
	if parent == "" {
		t.Fatalf("hostedWorkspaceParent = \"\"; diagnostics: %s", buf.String())
	}
	roots := sandbox.NewTenantRoots(parent, true)

	a, err := roots.Root(loopauth.Principal{Subject: "s1", Tenant: event.TenantID("acme")})
	if err != nil {
		t.Fatalf("tenant acme: %v", err)
	}
	b, err := roots.Root(loopauth.Principal{Subject: "s2", Tenant: event.TenantID("globex")})
	if err != nil {
		t.Fatalf("tenant globex: %v", err)
	}
	if a == b {
		t.Fatalf("two tenants resolved to the same tree %q", a)
	}
	// Non-overlapping in BOTH directions: neither may contain the other.
	if rel, rerr := filepath.Rel(a, b); rerr == nil && !strings.HasPrefix(rel, "..") {
		t.Errorf("tenant globex tree %q lies inside tenant acme's %q", b, a)
	}
	if rel, rerr := filepath.Rel(b, a); rerr == nil && !strings.HasPrefix(rel, "..") {
		t.Errorf("tenant acme tree %q lies inside tenant globex's %q", a, b)
	}
	// The parent itself is never a mount source.
	if a == parent || b == parent {
		t.Errorf("a tenant resolved to the shared parent %q — that is every tenant seeing every sibling", parent)
	}
	if _, err := roots.Root(loopauth.Principal{Subject: "anon"}); err == nil {
		t.Error("an EMPTY tenant resolved to a tree; it must resolve to none rather than share one")
	}
}

// TestHostedServiceWithNoResolvableParentSaysSo: the degraded-safe posture must
// not be silent, mirroring #64's "EGRESS ENFORCED with NO DATAPATH" notice that
// that change's review demanded. A hosted fuse that cannot provision per-tenant
// workspaces still refuses to share one — but an operator whose every bash call
// fails with "no per-tenant workspace root" deserves to have learned why at
// startup rather than from the first refusal.
func TestHostedServiceWithNoResolvableParentSaysSo(t *testing.T) {
	// A HOME that is a regular FILE: nothing can be created under it, so the
	// parent cannot be provisioned.
	dir := t.TempDir()
	notADir := filepath.Join(dir, "home-is-a-file")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", notADir)

	var buf bytes.Buffer
	if parent := hostedWorkspaceParent(&buf); parent != "" {
		t.Fatalf("hostedWorkspaceParent = %q with an unusable home, want \"\"", parent)
	}
	out := buf.String()
	for _, want := range []string{"HOSTED", "NO PER-TENANT WORKSPACE"} {
		if !strings.Contains(out, want) {
			t.Errorf("degraded-safe diagnostic %q does not contain %q", out, want)
		}
	}
}

// TestHealthHooksAreInstalledWhereTheStoreIs closes the second inert knob.
// SandboxHealthHooks is the ONLY production translator from the substrate's
// health seam to KindSandboxHealth events, so with no non-test caller the whole
// fuse_sandbox_unhealthy_total family can never observe data — and an
// always-zero failure counter is indistinguishable from a healthy fleet.
//
// It mirrors the SetGateHooks assertion's shape: the hooks are installed on the
// path that HAS a per-loop event store, and installing them is what makes the
// Service's health seam live.
func TestHealthHooksAreInstalledWhereTheStoreIs(t *testing.T) {
	tenantWorkspaceHome(t)
	root := t.TempDir()
	chdirForSandbox(t, root)

	var buf bytes.Buffer
	svc, closeFn := newSandboxService(config.Config{}, true, &buf)
	t.Cleanup(closeFn)
	if svc == nil {
		t.Fatalf("newSandboxService returned nil; diagnostics: %s", buf.String())
	}
	if svc.HealthObserved() {
		t.Fatal("a freshly constructed Service already reports health hooks; the seam must start inert")
	}

	store := &countingStore{}
	installSandboxLoopHooks(svc, store, "root-node")
	if !svc.HealthObserved() {
		t.Fatal("SetHealthHooks was NOT installed on the path that has an event store — " +
			"fuse_sandbox_unhealthy_total can never observe data, and an always-zero " +
			"failure counter reads exactly like a healthy fleet")
	}
}

// TestInstallSandboxLoopHooksToleratesNoStore pins the honest shape for the
// bindings that have no per-loop store (one-shot, shell, research-probe,
// mcp-server): they install nothing rather than emitting into a NoopStore that
// would make the wiring LOOK active. A nil *Service must be tolerated too —
// NewBash(nil) is a supported fail-closed shape.
func TestInstallSandboxLoopHooksToleratesNoStore(t *testing.T) {
	tenantWorkspaceHome(t)
	root := t.TempDir()
	chdirForSandbox(t, root)

	var buf bytes.Buffer
	svc, closeFn := newSandboxService(config.Config{}, false, &buf)
	t.Cleanup(closeFn)

	installSandboxLoopHooks(svc, nil, "root-node") // must not panic
	if svc != nil && svc.HealthObserved() {
		t.Error("a nil store installed live health hooks; a binding with no store must stay inert")
	}
	installSandboxLoopHooks(nil, &countingStore{}, "root-node") // must not panic
}

// countingStore is a minimal event.EventStore standing in for a loop's own
// store. It records nothing beyond being non-nil, which is all these wiring
// assertions need — the translation itself is pinned in internal/tools.
type countingStore struct {
	mu       sync.Mutex
	appended int
}

func (c *countingStore) Append(event.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.appended++
	return nil
}

func (c *countingStore) Subscribe() (<-chan event.Event, func()) {
	ch := make(chan event.Event)
	return ch, func() {}
}

func (c *countingStore) Replay(event.Seq) ([]event.Event, error) { return nil, nil }

// chdirForSandbox points the process working directory at dir for the test.
// newSandboxService reads its trusted root from os.Getwd, so a test that wants
// a known root has to move the process — and must move it back, since the
// working directory is process-global.
func chdirForSandbox(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %q: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}
