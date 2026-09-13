package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/loopauth"
	"github.com/ethanhinson/fuse/internal/tools/sandbox"
	"github.com/ethanhinson/fuse/internal/tools/sandbox/kubernetes"
)

// WHY THIS FILE EXISTS.
//
// `security-knob-inert-at-composition-root`, for the third time in this package
// (#64's Proxy, #65's TenantRoots/health hooks, and now #0075's Kubernetes
// substrate). The shape never changes: a fail-CLOSED feature passes its ENTIRE
// unit suite while being completely inert in the shipped binary, because every
// unit test constructs the enforcing object BY HAND and nothing in cmd/ ever
// calls the constructor.
//
// Change 0075 ships exactly that shape again, and worse than before, because
// the failure is silent in BOTH directions:
//
//   - sandbox.WithHandlerFactory("kubernetes", …) unregistered ⇒ `handler:
//     kubernetes` refuses at selectHandler with "this binary has no factory for
//     it". Every kubernetes unit test still passes; the whole substrate is dead
//     code in the binary.
//   - The factory registered but the ADVERTISE-address refusal not reached ⇒ a
//     fuse that comes up under `enforce` with sidecars that can reach nothing,
//     presenting inside the sandbox as a hang rather than a config error.
//
// So the assertions below are about the COMPOSITION ROOT and nothing else. Each
// is written to turn RED if the corresponding line is dropped from
// cmd/fuse/sandbox.go.
//
// A stub substrate is substituted through newKubernetesSubstrate — the same
// seam-var idiom egressForwarderCandidates uses, and for the same reason: the
// REAL constructor needs a reachable API server, and a test that required one
// would be a test that skipped on every developer machine and therefore proved
// nothing about the wiring. What the stub replaces is the CLUSTER, never the
// registration, the refusal ordering, or selectHandler's decision — those are
// what is under test. TestKubernetesSubstrateConstructorHasANonTestCaller
// closes the remaining gap by proving the real constructor is reached from
// non-test code.

// stubRemoteSubstrate is a sandbox.RemoteSubstrate that touches no network. It
// exists only so the composition root's factory can return a real
// sandbox.Handler without a cluster.
type stubRemoteSubstrate struct {
	name string
}

func (s *stubRemoteSubstrate) Name() string { return s.name }

func (s *stubRemoteSubstrate) Verify(context.Context) error { return nil }

func (s *stubRemoteSubstrate) Provision(context.Context, loopauth.Principal, sandbox.RemoteSpec) (sandbox.RemoteSandbox, error) {
	return nil, context.Canceled
}

func (s *stubRemoteSubstrate) Reap(context.Context, time.Duration) (int, error) { return 0, nil }

// withStubKubernetesSubstrate swaps the substrate constructor for the test and
// counts how many times the composition root actually called it. The count is
// the load-bearing assertion: a factory that is registered but never invoked,
// and a factory that is invoked but whose handler is discarded, are both the
// inert-knob failure.
func withStubKubernetesSubstrate(t *testing.T) *atomic.Int64 {
	t.Helper()
	var calls atomic.Int64
	prev := newKubernetesSubstrate
	newKubernetesSubstrate = func(opts kubernetesSubstrateOptions) (sandbox.RemoteSubstrate, error) {
		calls.Add(1)
		if opts.Config.Kubernetes.Refused {
			// The stub must honour the ONE refusal the real constructor owns
			// that this test cares about, or the malformed-block assertion
			// below would pass for the wrong reason.
			t.Errorf("the composition root called the substrate constructor with a REFUSED kubernetes: block; " +
				"a discarded block must never be built from substrate defaults")
		}
		return &stubRemoteSubstrate{name: sandbox.HandlerKubernetes}, nil
	}
	t.Cleanup(func() { newKubernetesSubstrate = prev })
	return &calls
}

const kubernetesAllowAllConfig = "handler: kubernetes\n" +
	"kubernetes:\n" +
	"  namespace_prefix: fuse-sb\n"

// TestKubernetesHandlerIsRegisteredAndConstructed is THE wiring assertion of
// task 10, and the one the learning demands: `handler: kubernetes` in the
// trusted-local off-switch file must yield a Service that is AVAILABLE, reports
// the kubernetes handler name, and whose handler was actually built by the
// registered factory.
//
// Deleting the sandbox.WithHandlerFactory registration from newSandboxService
// turns this RED three ways at once: the Service comes back nil (selectHandler
// refuses an unregistered name), HandlerName() is not "kubernetes", and the
// constructor call count is zero.
func TestKubernetesHandlerIsRegisteredAndConstructed(t *testing.T) {
	tenantWorkspaceHome(t)
	root := t.TempDir()
	chdirForSandbox(t, root)
	writeEgressConfig(t, root, kubernetesAllowAllConfig)
	calls := withStubKubernetesSubstrate(t)

	var buf bytes.Buffer
	svc, closeFn := newSandboxService(config.Config{}, false, &buf)
	t.Cleanup(closeFn)

	if svc == nil {
		t.Fatalf("newSandboxService returned NIL for `handler: kubernetes` — the factory is not registered at the "+
			"composition root, so the whole Kubernetes substrate is dead code in the shipped binary; diagnostics: %s",
			buf.String())
	}
	if got := svc.HandlerName(); got != sandbox.HandlerKubernetes {
		t.Fatalf("HandlerName() = %q, want %q; diagnostics: %s", got, sandbox.HandlerKubernetes, buf.String())
	}
	if !svc.Available() {
		t.Fatal("Service reports no handler; `handler: kubernetes` selected a Service with nothing behind it")
	}
	if !svc.Contained() {
		t.Fatal("Service.Contained() = false on the kubernetes handler — a remote substrate IS contained, and a " +
			"false here files its events under the one handler label that means uncontained")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("the substrate constructor was called %d times, want exactly 1 — the handler was not actually "+
			"CONSTRUCTED, so the registration is decoration", n)
	}
}

// TestKubernetesEnforceWithoutAdvertiseAddressRefusesLoudly is the second half
// of the inert-knob defect, pointed at the datapath rather than at the
// registration.
//
// Under `egress.mode: enforce` a sandbox Pod reaches fuse's own TLS listener at
// kubernetes.proxy.advertise_address (or $FUSE_POD_IP). With NEITHER set, every
// Pod comes up with a sidecar dialling nothing: inside the sandbox that is a
// HANG, which tells an operator nothing at all. The kubernetes package refuses
// that at construction (task 8) — this asserts the refusal actually reaches the
// operator through the composition root instead of being swallowed into a
// fallback substrate.
func TestKubernetesEnforceWithoutAdvertiseAddressRefusesLoudly(t *testing.T) {
	tenantWorkspaceHome(t)
	root := t.TempDir()
	chdirForSandbox(t, root)
	writeEgressConfig(t, root,
		"handler: kubernetes\n"+
			"egress:\n  mode: enforce\n  allow:\n    - host: api.example.com\n      port: 443\n")
	// $FUSE_POD_IP is the documented fallback, so it must be absent for this to
	// be the case under test. t.Setenv restores it afterwards.
	t.Setenv("FUSE_POD_IP", "")

	// The REAL constructor, deliberately: the refusal under test belongs to the
	// kubernetes package's newSubstrate and the point is that it is reached.
	var buf bytes.Buffer
	svc, closeFn := newSandboxService(config.Config{}, true, &buf)
	t.Cleanup(closeFn)

	if svc.Available() {
		t.Fatalf("newSandboxService returned a USABLE Service (handler %q) with egress ENFORCED and no advertise "+
			"address; every Pod's sidecar would dial nothing and the sandbox would simply hang", svc.HandlerName())
	}
	if svc.SelectionRefusal() == nil {
		t.Fatal("the Service carries no selection refusal, so the composition root has nothing to print")
	}
	out := buf.String()
	for _, want := range []string{"substrate unavailable", "advertise"} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal diagnostic %q does not mention %q — an operator cannot act on a refusal that does "+
				"not name the missing knob", out, want)
		}
	}
}

// TestMalformedKubernetesBlockRefusesAndNeverFallsBack is ADR-0058 rule 2
// pointed sideways: a named handler that cannot be built must not be silently
// replaced by a DIFFERENT one.
//
// A malformed `kubernetes:` block is discarded wholesale at load
// (WarnBadKubernetes) and records Config.Kubernetes.Refused. Because the handler
// was NAMED, the only correct outcome is a refusal. Falling back to the
// container handler would hand the operator a working bash tool whose sandbox is
// not where they think it is; falling back to the host handler would be an
// outright containment breach.
func TestMalformedKubernetesBlockRefusesAndNeverFallsBack(t *testing.T) {
	tenantWorkspaceHome(t)
	root := t.TempDir()
	chdirForSandbox(t, root)
	// An in-BLOCK fault, deliberately, rather than a YAML type error: a type
	// error discards the whole FILE (the ADR-0053 salvage path) and would not
	// exercise Config.Kubernetes.Refused at all.
	writeEgressConfig(t, root,
		"handler: kubernetes\n"+
			"kubernetes:\n"+
			"  namespace_prefix: \"Not_A_Label\"\n"+
			"  startup_timeout: \"not-a-duration\"\n")
	calls := withStubKubernetesSubstrate(t)

	var buf bytes.Buffer
	svc, closeFn := newSandboxService(config.Config{}, false, &buf)
	t.Cleanup(closeFn)

	if svc.Available() {
		t.Fatalf("a malformed kubernetes: block yielded a usable %q Service — the operator NAMED kubernetes and got "+
			"a different substrate, which is the silent-substitution failure ADR-0058 rule 2 forbids", svc.HandlerName())
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("the substrate constructor ran %d times on a REFUSED block; a discarded block must build NOTHING", n)
	}
	out := buf.String()
	if !strings.Contains(out, "bad_kubernetes") {
		t.Errorf("diagnostics %q do not carry the bad_kubernetes warning; the operator's only signal that their "+
			"block was thrown away", out)
	}
	if !strings.Contains(out, "substrate unavailable") {
		t.Errorf("diagnostics %q do not report the substrate as unavailable", out)
	}
	// The refusal must not read as "uncontained", which is the host substrate's
	// notice: a refusal and an authorized escape look nothing alike.
	if strings.Contains(out, "UNCONTAINED") {
		t.Errorf("diagnostics %q announce UNCONTAINED for a refused kubernetes block — the named-handler failure "+
			"path reached the HOST handler", out)
	}
}

// TestKubernetesFactoryRegistrationIsGatedOnTheNamedHandler pins the other
// direction: a config that does NOT name kubernetes must not cause a substrate
// to be constructed. The factory is only ever invoked by selectHandler for the
// name it is registered under, and building a cluster client for every local
// `fuse shell` would be a startup network call nobody asked for.
func TestKubernetesFactoryRegistrationIsGatedOnTheNamedHandler(t *testing.T) {
	tenantWorkspaceHome(t)
	root := t.TempDir()
	chdirForSandbox(t, root)
	writeEgressConfig(t, root, "handler: container\n")
	calls := withStubKubernetesSubstrate(t)

	var buf bytes.Buffer
	_, closeFn := newSandboxService(config.Config{}, false, &buf)
	t.Cleanup(closeFn)

	if n := calls.Load(); n != 0 {
		t.Fatalf("the kubernetes substrate constructor ran %d times for `handler: container`", n)
	}
}

// TestKubernetesOrphanReapHookIsWired closes the observability half. The orphan
// reaper is the ONLY signal that fuse instances are dying without releasing
// their sandboxes — an orphan is by definition a sandbox no Pool remembers, so
// the Pool's own reaper cannot see it and idle_ttl never fires for it. With
// sandbox.WithRemoteReapHook unwired, `sandbox.reap` with cause `orphan` can
// never be emitted by a real fuse, and fuse_sandbox_reap_total{cause="orphan"}
// stays permanently at zero.
//
// The hook the composition root installs is a process-scoped INDIRECTION (the
// handler is built once, at Service construction, while event stores are
// per-loop), so what is asserted here is that the indirection exists and that a
// per-loop store installed through installSandboxLoopHooks becomes its target.
func TestKubernetesOrphanReapHookIsWired(t *testing.T) {
	tenantWorkspaceHome(t)
	root := t.TempDir()
	chdirForSandbox(t, root)
	writeEgressConfig(t, root, kubernetesAllowAllConfig)
	withStubKubernetesSubstrate(t)

	// The sink is process-scoped (one substrate per process), so a test must
	// restore whatever an earlier test left behind.
	prev := remoteReapSink.swap(nil)
	t.Cleanup(func() { remoteReapSink.swap(prev) })

	var buf bytes.Buffer
	svc, closeFn := newSandboxService(config.Config{}, false, &buf)
	t.Cleanup(closeFn)
	if svc == nil {
		t.Fatalf("newSandboxService returned nil; diagnostics: %s", buf.String())
	}

	if remoteReapSink.target() != nil {
		t.Fatal("the reap sink already has a target before any loop installed one; the seam must start inert")
	}

	store := &countingStore{}
	installSandboxLoopHooks(svc, store, "root-node")

	sink := remoteReapSink.target()
	if sink == nil {
		t.Fatal("installSandboxLoopHooks did NOT install the orphan-reap observer — sandbox.reap with cause " +
			"`orphan` can never be emitted by a real fuse, and an always-zero orphan counter reads exactly like " +
			"a fleet that never leaks a sandbox")
	}
	// Drive it: the indirection must reach the loop's store, not merely be
	// non-nil.
	sink(sandbox.ReleaseInfo{Handler: sandbox.HandlerKubernetes, Cause: sandbox.CauseOrphan})
	store.mu.Lock()
	n := store.appended
	store.mu.Unlock()
	if n == 0 {
		t.Fatal("the installed orphan-reap observer appended nothing to the loop's event store")
	}
}

// TestKubernetesSubstrateConstructorHasANonTestCaller is the grep the learning
// asks for, made mechanical.
//
// Every other test in this file substitutes newKubernetesSubstrate, so none of
// them can observe the REAL kubernetes.New ever being reached. This one reads
// the non-test sources of cmd/ and asserts the constructor is called from
// production code — the exact check that would have caught #64's zero-non-test-
// caller Proxy.
func TestKubernetesSubstrateConstructorHasANonTestCaller(t *testing.T) {
	out, err := exec.Command("go", "list", "-f", "{{.Dir}}", "github.com/ethanhinson/fuse/cmd/fuse").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	dir := strings.TrimSpace(string(out))

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	found := ""
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			t.Fatalf("read %s: %v", name, rerr)
		}
		if strings.Contains(string(body), "kubernetes.New(") {
			found = name
			break
		}
	}
	if found == "" {
		t.Fatal("no NON-TEST file in cmd/fuse calls kubernetes.New — internal/tools/sandbox/kubernetes is a fully " +
			"tested package that the shipped binary never constructs, which is `security-knob-inert-at-" +
			"composition-root` exactly")
	}
}

// TestKubernetesProxyListenDefaultAgreesWithTheSubstrate pins the one value that
// exists on both sides of the seam.
//
// The composition root BINDS the TLS listener; the kubernetes package derives the
// per-Pod NetworkPolicy's permitted port and the sidecar's -upstream port. If
// those two disagree by a digit, every sandbox under `enforce` has a policy
// permitting a port nothing is listening on — a total egress blackout that
// presents inside the sandbox as a hang and that no unit test on either side can
// see, because each half is internally consistent.
//
// So the default is ONE exported constant and this asserts the composition root
// reads it rather than re-spelling it.
func TestKubernetesProxyListenDefaultAgreesWithTheSubstrate(t *testing.T) {
	if got := kubernetesProxyListen(sandbox.DefaultConfig()); got != kubernetes.DefaultProxyListen {
		t.Fatalf("kubernetesProxyListen(default) = %q, want %q — the port fuse LISTENS on and the port a Pod's "+
			"policy PERMITS come from this one value and must not be spelled twice", got, kubernetes.DefaultProxyListen)
	}

	// An explicit knob wins, and the substrate parses the SAME string for its
	// port, so the two stay derived from one place.
	explicit := "0.0.0.0:31290"
	cfg := sandbox.DefaultConfig()
	cfg.Kubernetes.ProxyListen = &explicit
	if got := kubernetesProxyListen(cfg); got != explicit {
		t.Fatalf("kubernetesProxyListen(explicit) = %q, want %q", got, explicit)
	}
}
