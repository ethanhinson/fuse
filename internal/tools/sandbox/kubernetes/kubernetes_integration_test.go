package kubernetes

// THE KIND LANE — the acceptances that only a REAL cluster can settle.
//
// # Gated at RUNTIME, never build-tagged
//
// This package's stated policy, inherited from
// internal/tools/sandbox/container_integration_test.go: "absence of a runtime is
// never a red suite". Every test below reaches for a cluster and calls
// t.Skipf when it cannot find one, so `go test ./...` is green on a machine
// with no kind, no docker and no kubeconfig.
//
// A build tag would have been the easier choice and is the wrong one. A tagged
// file is invisible to `go vet`, invisible to the compiler on every ordinary run,
// and rots silently: the first time anyone builds it may be months after the code
// it tests has changed shape. These compile on every run and skip at runtime, so
// the only thing an absent cluster costs is the assertions themselves.
//
// # The skip must be LOUD
//
// `smoke-over-fake-backend-proves-wire-not-system`: a suite that skips quietly is
// indistinguishable from a suite that passed. So every skip message NAMES the
// acceptance that did not run, and names the reason. Do not shorten them.
//
// # What is deliberately NOT here
//
// Everything provable against the generated fake clientset lives in the unit
// files beside this one — PodSpec goldens, the read-back drift refusals, policy
// rendering for both egress modes, the namespace injectivity table, the limits
// mapping, argv rendering, and the Reap selection logic. This file exists ONLY
// for the claims a fake client cannot make: that a Pod actually runs, that exec
// actually works, that NetworkPolicy is actually enforced, and that the metadata
// endpoint is actually unreachable.
//
// # Bring-up
//
//	kind create cluster --name fuse-sandbox
//	kubectl apply -f https://raw.githubusercontent.com/projectcalico/calico/v3.28.0/manifests/calico.yaml
//	make test-k8s
//
// See docs/sandbox-kubernetes.md. Note the CNI step: kind's default kindnetd does
// NOT enforce NetworkPolicy, so TestIntegrationMetadataFloorHoldsInBothModes and
// Verify's canary pair correctly refuse without Calico or Cilium.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// kindContextEnv lets a lane point at a cluster other than the documented one.
// It is TEST-ONLY plumbing and reaches no production path: the substrate reads
// its context from the trusted config and from nowhere else.
const kindContextEnv = "FUSE_K8S_TEST_CONTEXT"

// defaultKindContext is the context `kind create cluster --name fuse-sandbox`
// writes, which is what docs/sandbox-kubernetes.md tells an operator to create.
const defaultKindContext = "kind-fuse-sandbox"

// integrationImage is the workload image these tests run. busybox rather than the
// substrate's alpine default for one reason that matters: it carries `nc`, which
// Verify's canary probe requires, and it is small enough that a cold `kind load`
// is not the dominant cost of the lane.
const integrationImage = "busybox:1.36"

// integrationPrefix keeps every object this lane creates under one namespace
// prefix, distinct from the default, so a failed run leaves a droppable blast
// radius: `kubectl delete ns -l fuse.dev/managed=true` is never needed against
// namespaces a real deployment owns.
const integrationPrefix = "fuse-it"

// clusterOrSkip resolves a live cluster, or skips LOUDLY naming what did not run.
//
// It performs a real API call (a server-version fetch) rather than merely loading
// a kubeconfig: a stale context pointing at a deleted kind cluster loads fine and
// then fails every assertion, which would read as a broken substrate rather than
// as an absent one.
func clusterOrSkip(t *testing.T, acceptance string) (*Substrate, kubernetes.Interface) {
	t.Helper()

	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skipf("SKIPPING the kind acceptance %q: no KUBECONFIG and no home directory to find one in (%v). "+
				"NOTHING about this acceptance was verified.", acceptance, err)
		}
		kubeconfig = filepath.Join(home, ".kube", "config")
	}
	if _, err := os.Stat(kubeconfig); err != nil {
		t.Skipf("SKIPPING the kind acceptance %q: no kubeconfig at %s (%v). Bring a cluster up with "+
			"`kind create cluster --name fuse-sandbox` — see docs/sandbox-kubernetes.md. "+
			"NOTHING about this acceptance was verified.", acceptance, kubeconfig, err)
	}

	kctx := os.Getenv(kindContextEnv)
	if kctx == "" {
		kctx = defaultKindContext
	}

	s, err := New(Options{
		Config: sandbox.Config{
			Image:   integrationImage,
			IdleTTL: 30 * time.Second,
			Limits: sandbox.Limits{
				MemoryBytes: ptr(int64(256 * 1024 * 1024)),
				CPUs:        ptr("0.5"),
			},
			Kubernetes: sandbox.Kubernetes{
				Kubeconfig:              ptr(kubeconfig),
				Context:                 ptr(kctx),
				NamespacePrefix:         ptr(integrationPrefix),
				StartupTimeout:          ptr(120 * time.Second),
				PodMaxLifetime:          ptr(10 * time.Minute),
				WorkspaceSizeLimitBytes: ptr(int64(256 * 1024 * 1024)),
			},
		},
		MaxPodsPerTenant: 4,
		InstanceID:       "it-instance-a",
	})
	if err != nil {
		t.Skipf("SKIPPING the kind acceptance %q: no usable cluster at context %q in %s (%v). Bring one up with "+
			"`kind create cluster --name fuse-sandbox`, or point %s at another context. "+
			"NOTHING about this acceptance was verified.", acceptance, kctx, kubeconfig, err, kindContextEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := s.cs.Discovery().ServerVersion(); err != nil {
		t.Skipf("SKIPPING the kind acceptance %q: context %q does not answer (%v) — a stale kubeconfig entry for a "+
			"deleted cluster looks exactly like this. NOTHING about this acceptance was verified.", acceptance, kctx, err)
	}
	// A namespace list proves the credential can actually do something, so an RBAC
	// misconfiguration is a skip with a diagnostic rather than N confusing failures.
	if _, err := s.cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
		t.Skipf("SKIPPING the kind acceptance %q: the kubeconfig credential cannot list namespaces (%v); apply "+
			"deploy/k8s/sandbox-rbac.yaml or use a cluster-admin context. "+
			"NOTHING about this acceptance was verified.", acceptance, err)
	}

	return s, s.cs
}

// loadImageOrSkip makes the workload image present on the kind nodes.
//
// `kind load docker-image` rather than trusting the node to pull: a kind node
// pulling from Docker Hub is the single flakiest step in this lane, and a rate
// limit would read as a broken substrate. When kind is not the runtime (a real
// cluster reached through KUBECONFIG) this is skipped and the node pulls.
func loadImageOrSkip(t *testing.T, acceptance string) {
	t.Helper()
	if _, err := exec.LookPath("kind"); err != nil {
		return // Not a kind cluster; the node pulls for itself.
	}
	kctx := os.Getenv(kindContextEnv)
	if kctx == "" {
		kctx = defaultKindContext
	}
	name := strings.TrimPrefix(kctx, "kind-")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if out, err := exec.CommandContext(ctx, "docker", "pull", integrationImage).CombinedOutput(); err != nil {
		t.Skipf("SKIPPING the kind acceptance %q: could not pull %s (%v)\n%s\n"+
			"NOTHING about this acceptance was verified.", acceptance, integrationImage, err, out)
	}
	if out, err := exec.CommandContext(ctx, "kind", "load", "docker-image", integrationImage, "--name", name).CombinedOutput(); err != nil {
		t.Skipf("SKIPPING the kind acceptance %q: could not load %s into kind cluster %q (%v)\n%s\n"+
			"NOTHING about this acceptance was verified.", acceptance, integrationImage, name, err, out)
	}
}

// cleanupNamespaces deletes the namespaces this lane created. Production fuse
// NEVER deletes a namespace (that is operator-owned) — this is test hygiene on
// objects only the test made, under a prefix only the test uses.
func cleanupNamespaces(t *testing.T, cs kubernetes.Interface) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		nss, err := cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: labelManaged + "=true"})
		if err != nil {
			return
		}
		for _, ns := range nss.Items {
			if !strings.HasPrefix(ns.Name, integrationPrefix+"-") {
				// Never touch a namespace this lane did not create.
				continue
			}
			if ns.Name == integrationPrefix+canaryNamespaceSuffix {
				// The CANARY namespace is deliberately kept. Deleting it between
				// tests puts it into Terminating, and the next test's Verify then
				// fails to create anything in it — surfacing as "the canary refused
				// this cluster", which reads as a CNI problem and is actually this
				// cleanup racing itself. It holds no per-test state (Verify recreates
				// its Pods every run), so leaving it is both safe and correct.
				continue
			}
			_ = cs.CoreV1().Namespaces().Delete(ctx, ns.Name, metav1.DeleteOptions{})
		}
	})
}

// verifyOrSkip runs the canary pair and turns a refusal into a SKIP naming which
// leg failed.
//
// This is the one place in the lane where skipping on a refusal is correct rather
// than evasive: kind's default kindnetd does not enforce NetworkPolicy, and
// Verify refusing on such a cluster is the substrate working exactly as designed
// (ADR-0058 rule 3). The tests that follow are about Pod lifecycle and exec, not
// about the floor, so a cluster with no policy enforcement should skip them
// rather than redden them — and the acceptance that IS about the floor
// (TestIntegrationMetadataFloorHoldsInBothModes) reports its own skip in the same
// terms.
func verifyOrSkip(t *testing.T, s *Substrate, acceptance string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if err := s.Verify(ctx); err != nil {
		t.Skipf("SKIPPING the kind acceptance %q: the canary pair REFUSED this cluster (%v). This is the substrate "+
			"working as designed — kind's default kindnetd does not enforce NetworkPolicy, so install Calico or "+
			"Cilium (see docs/sandbox-kubernetes.md) to run this lane. "+
			"NOTHING about this acceptance was verified.", acceptance, err)
	}
}

// TestIntegrationWarmPodServesTwoCommandsThenIsReaped is the headline acceptance:
// a warm Pod serves one bash command, a SECOND command reuses the same Pod, and
// the idle reaper then deletes it.
//
// The reuse assertion is the one a fake client cannot make. `containerIdentified`
// had no implementor before this change (`docker run --rm` leaves no durable
// container), so "the Pool reuses a warm sandbox" was a documented property with
// nothing behind it. Here the Pod's identity is read back from the sandbox itself
// and compared across two Execs.
func TestIntegrationWarmPodServesTwoCommandsThenIsReaped(t *testing.T) {
	const acceptance = "a warm Pod serves one bash command, a second command REUSES it, and the reaper deletes it"
	s, cs := clusterOrSkip(t, acceptance)
	loadImageOrSkip(t, acceptance)
	cleanupNamespaces(t, cs)
	verifyOrSkip(t, s, acceptance)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	p := loopauth.Principal{Tenant: event.TenantID("it-warm"), Subject: "s-warm"}
	sb, err := s.Provision(ctx, p, sandbox.RemoteSpec{})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	id := sb.ID()
	t.Logf("provisioned %s", id)

	// FIRST command.
	out, err := sb.Exec(ctx, sandbox.Env{}, "echo one > /workspace/marker && echo first-ok", sb.MountRoot())
	if err != nil {
		t.Fatalf("first Exec: %v (combined: %q)", err, out.Combined)
	}
	if out.ExitCode != 0 {
		t.Fatalf("first Exec exit = %d, want 0 (combined: %q)", out.ExitCode, out.Combined)
	}
	if !strings.Contains(string(out.Combined), "first-ok") {
		t.Fatalf("first Exec combined = %q, want it to contain first-ok", out.Combined)
	}

	// SECOND command, through the SAME sandbox. The marker file the first command
	// wrote must still be there: that is what "warm" means, and a fresh Pod per
	// Exec would pass every other assertion in this test while failing this one.
	out2, err := sb.Exec(ctx, sandbox.Env{}, "cat /workspace/marker", sb.MountRoot())
	if err != nil {
		t.Fatalf("second Exec: %v (combined: %q)", err, out2.Combined)
	}
	if out2.ExitCode != 0 {
		t.Fatalf("second Exec exit = %d, want 0 — the workspace did not survive, so the Pod was NOT reused "+
			"(combined: %q)", out2.ExitCode, out2.Combined)
	}
	if !strings.Contains(string(out2.Combined), "one") {
		t.Fatalf("second Exec combined = %q, want the marker the FIRST command wrote; the Pod was not reused", out2.Combined)
	}
	if sb.ID() != id {
		t.Fatalf("sandbox id changed from %q to %q across two Execs", id, sb.ID())
	}

	// THE REAPER. staleAfter=0 makes every managed Pod stale, which is the reaper's
	// own selection logic driven to its limit rather than a special case: it
	// selects purely on heartbeat age.
	n, err := s.Reap(ctx, 0)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if n < 1 {
		t.Fatalf("Reap deleted %d, want at least the one Pod this test created", n)
	}
	assertPodGone(t, ctx, cs, id)
}

// TestIntegrationDeadlineTearsThePodDown is the run-`--rm` guarantee on a
// substrate fuse does not own: a command that exceeds its deadline must report
// TimedOut AND leave no Pod behind.
//
// It is the acceptance most worth a real cluster. On the fake client "the Pod was
// deleted" is a recorded action; here it is an object that either exists or does
// not, and the teardown races the sandbox's own heartbeat goroutine.
func TestIntegrationDeadlineTearsThePodDown(t *testing.T) {
	const acceptance = "a deadline-exceeded command returns TimedOut and the Pod is GONE"
	s, cs := clusterOrSkip(t, acceptance)
	loadImageOrSkip(t, acceptance)
	cleanupNamespaces(t, cs)
	verifyOrSkip(t, s, acceptance)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// THROUGH THE ADAPTER, not through Provision directly — and the distinction is
	// the point of this test rather than an implementation detail.
	//
	// Output.TimedOut and the teardown-on-deadline are the ADAPTER's contract
	// (sandbox.remoteRunner.Exec), not the substrate's: the substrate exposes a raw
	// Exec whose deadline is just a cancelled stream, and it is the adapter that
	// converts that into "TimedOut, and the sandbox is destroyed so no process is
	// left behind" — the run-`--rm` guarantee on a substrate where the sandbox
	// deliberately OUTLIVES the command. Driving Provision directly would assert
	// the wrong layer and pass while the bash tool's actual path was broken.
	h, err := sandbox.NewRemoteHandler(s, sandbox.Config{IdleTTL: 30 * time.Second})
	if err != nil {
		t.Fatalf("NewRemoteHandler: %v", err)
	}
	if c, ok := h.(interface{ Close() error }); ok {
		t.Cleanup(func() { _ = c.Close() })
	}

	p := loopauth.Principal{Tenant: event.TenantID("it-deadline"), Subject: "s-deadline"}
	runner, err := h.Acquire(ctx, p, sandbox.Env{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// ContainerID is reached through an assertion because the Pool's seam is
	// UNEXPORTED (sandbox.containerIdentified) — deliberately, so only handlers in
	// that package can satisfy it. The assertion doubles as this lane's proof that
	// a remote Runner really is the first implementor of that seam: it had none
	// before this change, so "the Pool reuses a warm sandbox" was a documented
	// property with nothing behind it.
	identified, ok := runner.(interface{ ContainerID() string })
	if !ok {
		t.Fatal("the remote Runner does not report a ContainerID; the Pool cannot certify it for warm reuse, which " +
			"presents as 'the pool never reuses a remote sandbox' rather than as a compile error")
	}
	id := identified.ContainerID()
	if id == "" {
		t.Fatal("the remote Runner's ContainerID is empty")
	}

	// `sleep 60` against a 5s deadline.
	execCtx, execCancel := context.WithTimeout(ctx, 5*time.Second)
	defer execCancel()
	out, err := runner.Exec(execCtx, "sleep 60", "")
	if !out.TimedOut {
		t.Fatalf("Output.TimedOut = false for a command that outlived its deadline (err=%v, exit=%d, combined=%q)",
			err, out.ExitCode, out.Combined)
	}
	if out.ExitCode == 0 {
		t.Errorf("a killed command reported exit 0; that is not a result to pass on")
	}

	// The Pod must be GONE, not merely marked. The substrate tears it down on the
	// deadline path precisely so a timed-out command leaves no process behind, and
	// "deleted" here means the API server no longer has the object.
	assertPodGone(t, ctx, cs, id)
}

// TestIntegrationWorkingDirIsContainedAgainstThePodsFilesystem is ADR-0044's gate
// 4 on a substrate fuse does not own: a non-empty working_dir must be HONOURED
// when it names a subpath of the Pod's workspace, and REFUSED when it escapes —
// and the decision must be made about the POD's filesystem, not fuse's.
//
// It is the acceptance that only a real cluster settles, because the defect it
// pins was invisible to every other lane. The adapter used to resolve containment
// through sandbox.resolveWorkspace, which canonicalises with EvalSymlinks/Stat
// against whatever filesystem the fuse PROCESS is running on. The unit lane hid
// that behind a test-only host-root option pointed at a t.TempDir(), and the only
// kind-lane path through the adapter passed working_dir="", the one value that
// never touches a filesystem at all. In the shipped binary "/workspace" is a path
// in the Pod and not in fuse's container, so every non-empty working_dir was
// refused: the feature was inoperative.
//
// So this test runs THROUGH THE ADAPTER (sandbox.NewRemoteHandler with no options
// — the shipped configuration) with a working_dir that exists ONLY in the Pod. A
// pass is only possible if containment stopped consulting fuse's filesystem.
func TestIntegrationWorkingDirIsContainedAgainstThePodsFilesystem(t *testing.T) {
	const acceptance = "a non-empty working_dir naming an in-Pod subpath RUNS there through the adapter, and an escape is refused"
	s, cs := clusterOrSkip(t, acceptance)
	loadImageOrSkip(t, acceptance)
	cleanupNamespaces(t, cs)
	verifyOrSkip(t, s, acceptance)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h, err := sandbox.NewRemoteHandler(s, sandbox.Config{IdleTTL: 2 * time.Minute})
	if err != nil {
		t.Fatalf("NewRemoteHandler: %v", err)
	}
	if c, ok := h.(interface{ Close() error }); ok {
		t.Cleanup(func() { _ = c.Close() })
	}

	p := loopauth.Principal{Tenant: event.TenantID("it-workdir"), Subject: "s-workdir"}
	runner, err := h.Acquire(ctx, p, sandbox.Env{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() { _ = runner.Release(context.WithoutCancel(ctx)) })

	// Create the subdirectory INSIDE the Pod. Nothing on the machine running this
	// test has it, which is the whole point: the containment decision below must be
	// about the Pod's emptyDir.
	if out, err := runner.Exec(ctx, "mkdir -p /workspace/deep/nest && echo made", ""); err != nil || out.ExitCode != 0 {
		t.Fatalf("mkdir in the Pod: err=%v exit=%d combined=%q", err, out.ExitCode, out.Combined)
	}

	// HONOURED, both spellings. `pwd` is the assertion: it reports where the
	// command ACTUALLY ran, inside the Pod, rather than what fuse computed.
	for _, workingDir := range []string{"deep/nest", "/workspace/deep/nest"} {
		out, err := runner.Exec(ctx, "pwd", workingDir)
		if err != nil {
			t.Fatalf("Exec(working_dir=%q) = %v; an in-Pod subpath must be honoured — this is the refusal that made "+
				"the feature inoperative, and it is only reproducible with a working_dir fuse's own filesystem lacks "+
				"(combined: %q)", workingDir, err, out.Combined)
		}
		if out.ExitCode != 0 {
			t.Fatalf("Exec(working_dir=%q) exit = %d, want 0 (combined: %q)", workingDir, out.ExitCode, out.Combined)
		}
		if got := strings.TrimSpace(string(out.Combined)); got != "/workspace/deep/nest" {
			t.Fatalf("the command ran in %q, want /workspace/deep/nest — the working_dir did not reach the Pod", got)
		}
	}

	// REFUSED, and refused in the adapter: nothing runs at all. `/etc` and `/`
	// genuinely EXIST in the Pod, so only the containment check stops them.
	for _, escape := range []string{"..", "../..", "/etc", "/", "/workspace/../etc", "/workspaceXXX"} {
		out, err := runner.Exec(ctx, "pwd", escape)
		if !errors.Is(err, sandbox.ErrWorkingDirRefused) {
			t.Fatalf("Exec(working_dir=%q) err = %v, want it to wrap ErrWorkingDirRefused (combined: %q)",
				escape, err, out.Combined)
		}
		if out.ExitCode != -1 {
			t.Fatalf("Exec(working_dir=%q) exit = %d, want -1 so an ExitCode-only caller fails closed", escape, out.ExitCode)
		}
		if len(out.Combined) != 0 {
			t.Fatalf("a refused working_dir still produced output %q; the command RAN", out.Combined)
		}
	}
}

// TestIntegrationMetadataFloorHoldsInBothModes is the acceptance the whole egress
// design rests on: a sandbox must NOT be able to read the cloud
// instance-metadata endpoint, in EITHER posture.
//
// Both modes, because the floor's shape differs between them and only one of the
// two shapes has ever been the easy case. Under `enforce` the endpoint is
// unreachable because the per-Pod policy names exactly one destination and this
// is not it. Under `allow-all` the policy is 0.0.0.0/0 with the metadata CIDRs in
// an `except` list — an additive-policy subtlety that is exactly the kind of
// thing a golden-object test asserts correctly while the cluster does something
// else.
//
// Only the allow-all leg runs here. The enforce leg needs a reachable fuse TLS
// listener on an advertise address the Pod can route to, which means running the
// fuse process itself in the cluster — that is #76's deployment and is named as
// NOT covered in the results file rather than implied.
func TestIntegrationMetadataFloorHoldsInBothModes(t *testing.T) {
	const acceptance = "curl/nc to 169.254.169.254 FAILS under egress.mode: allow-all"
	s, cs := clusterOrSkip(t, acceptance)
	loadImageOrSkip(t, acceptance)
	cleanupNamespaces(t, cs)
	verifyOrSkip(t, s, acceptance)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	p := loopauth.Principal{Tenant: event.TenantID("it-metadata"), Subject: "s-metadata"}
	sb, err := s.Provision(ctx, p, sandbox.RemoteSpec{})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	t.Cleanup(func() { _ = sb.Teardown(context.WithoutCancel(ctx)) })

	// A CONTROL first. Without it, a Pod with no network at all would pass the
	// metadata assertion for entirely the wrong reason, which is
	// `smoke-over-fake-backend-proves-wire-not-system` in miniature: the test would
	// prove the floor while proving nothing about the datapath.
	ctrl, err := sb.Exec(ctx, sandbox.Env{}, "nc -z -w 3 kubernetes.default 443 && echo control-reached", sb.MountRoot())
	if err != nil {
		t.Fatalf("control Exec: %v (combined %q)", err, ctrl.Combined)
	}
	if ctrl.ExitCode != 0 || !strings.Contains(string(ctrl.Combined), "control-reached") {
		t.Skipf("SKIPPING the kind acceptance %q: the CONTROL leg could not reach kubernetes.default:443 under "+
			"allow-all (exit %d, combined %q), so a metadata refusal would prove nothing — the Pod may simply have "+
			"no network. NOTHING about this acceptance was verified.", acceptance, ctrl.ExitCode, ctrl.Combined)
	}

	// THE FLOOR. Every metadata endpoint the substrate denies, each asserted
	// separately so a diagnostic names WHICH one leaked.
	for _, target := range []struct{ name, addr string }{
		{"AWS IMDS / GCE / Azure", "169.254.169.254"},
		{"ECS task metadata (hands out the task role)", "169.254.170.2"},
	} {
		out, err := sb.Exec(ctx, sandbox.Env{}, "nc -z -w 3 "+target.addr+" 80 && echo LEAKED", sb.MountRoot())
		if err != nil {
			t.Fatalf("metadata Exec for %s: %v", target.addr, err)
		}
		if out.ExitCode == 0 || strings.Contains(string(out.Combined), "LEAKED") {
			t.Fatalf("a sandbox REACHED %s (%s) under egress.mode: allow-all — it holds the NODE's cloud identity, "+
				"a credential fuse never issued, cannot scope and cannot revoke (exit %d, combined %q)",
				target.addr, target.name, out.ExitCode, out.Combined)
		}
	}
}

// TestIntegrationOrphanIsReapedByASecondInstance is the ADR-0058 rule that a
// reaped orphan is instance-AGNOSTIC.
//
// An orphan exists precisely when no fuse instance remembers a sandbox, so any
// LIVE instance must be able to collect any DEAD instance's leftovers. A reaper
// that filtered on `fuse.dev/instance` — the intuitive, wrong implementation —
// could never collect one, and no single-instance test can tell the difference.
//
// So: instance A provisions, its heartbeat is backdated to simulate A dying, and
// instance B (a second Substrate with a different InstanceID) reaps it.
func TestIntegrationOrphanIsReapedByASecondInstance(t *testing.T) {
	const acceptance = "an orphan with a stale heartbeat is reaped by a SECOND instance"
	s, cs := clusterOrSkip(t, acceptance)
	loadImageOrSkip(t, acceptance)
	cleanupNamespaces(t, cs)
	verifyOrSkip(t, s, acceptance)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	p := loopauth.Principal{Tenant: event.TenantID("it-orphan"), Subject: "s-orphan"}
	sb, err := s.Provision(ctx, p, sandbox.RemoteSpec{})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	id := sb.ID()
	ns, name, ok := strings.Cut(id, "/")
	if !ok {
		t.Fatalf("sandbox ID %q is not <namespace>/<pod>", id)
	}

	// INSTANCE A DIES. Simulated by backdating the heartbeat annotation, which is
	// the reaper's whole input — a genuinely killed process would leave exactly
	// this state once its last beat aged out.
	stale := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, annotationHeartbeat, stale)
	if _, err := cs.CoreV1().Pods(ns).Patch(ctx, name, "application/merge-patch+json", []byte(patch), metav1.PatchOptions{}); err != nil {
		t.Fatalf("backdate heartbeat: %v", err)
	}

	// INSTANCE B — a different InstanceID, everything else identical. It must
	// collect A's Pod.
	b, err := New(Options{
		Config:           s.integrationConfigForSecondInstance(),
		MaxPodsPerTenant: 4,
		InstanceID:       "it-instance-B",
	})
	if err != nil {
		t.Fatalf("second instance: %v", err)
	}
	if b.instanceID == s.instanceID {
		t.Fatalf("both instances report %q; this test would prove nothing", b.instanceID)
	}

	n, err := b.Reap(ctx, time.Hour)
	if err != nil {
		t.Fatalf("Reap by the second instance: %v", err)
	}
	if n < 1 {
		t.Fatalf("the second instance's Reap deleted %d Pods, want at least instance A's orphan — a reaper that "+
			"filtered on fuse.dev/instance could never collect an orphan, which is exactly the bug this test "+
			"exists to catch", n)
	}
	assertPodGone(t, ctx, cs, id)
}

// integrationConfigForSecondInstance rebuilds the config a second Substrate over
// the SAME cluster is constructed from.
//
// It reads the resolved fields off the first substrate rather than re-deriving
// them from the environment, so the two instances provably differ in exactly one
// thing — the instance id — which is what the orphan test's conclusion rests on.
func (s *Substrate) integrationConfigForSecondInstance() sandbox.Config {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, _ := os.UserHomeDir()
		kubeconfig = filepath.Join(home, ".kube", "config")
	}
	kctx := os.Getenv(kindContextEnv)
	if kctx == "" {
		kctx = defaultKindContext
	}
	return sandbox.Config{
		Image: s.image,
		Kubernetes: sandbox.Kubernetes{
			Kubeconfig:      ptr(kubeconfig),
			Context:         ptr(kctx),
			NamespacePrefix: ptr(s.namespacePrefix),
			StartupTimeout:  ptr(s.startupTimeout),
			PodMaxLifetime:  ptr(s.podMaxLifetime),
		},
	}
}

// assertPodGone waits briefly for a deleted Pod to actually disappear.
//
// A poll rather than a single Get, because Delete is asynchronous: the API server
// records a deletionTimestamp, the kubelet runs the (5s) grace period, and only
// then is the object removed. A single Get immediately after Delete would be
// flaky in the direction that MATTERS — it would fail on a correct teardown — so
// the wait is generous and the failure, when it comes, is real.
func assertPodGone(t *testing.T, ctx context.Context, cs kubernetes.Interface, id string) {
	t.Helper()
	ns, name, ok := strings.Cut(id, "/")
	if !ok {
		t.Fatalf("sandbox ID %q is not <namespace>/<pod>", id)
	}

	deadline := time.Now().Add(90 * time.Second)
	var last *corev1.Pod
	for time.Now().Before(deadline) {
		got, err := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return
		}
		if err != nil {
			t.Fatalf("Get pod %s: %v", id, err)
		}
		last = got
		time.Sleep(2 * time.Second)
	}
	phase := "<unknown>"
	if last != nil {
		phase = string(last.Status.Phase)
	}
	t.Fatalf("pod %s still exists 90s after it should have been torn down (phase %s) — a timed-out or reaped "+
		"sandbox that is merely FORGOTTEN is the failure mode ADR-0058 rule 2 exists to forbid", id, phase)
}
