package kubernetes

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/utils/exec"

	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// THE CONFORMANCE ASSERTION. It could not be declared before this task: a stub
// Verify returning nil would have been a substrate claiming to have proved a floor
// it never probed, which is the single failure ADR-0058 rule 3 exists to make
// impossible. Now that Verify actually probes, the compile-time assertion in
// kubernetes.go is live and this test is the readable statement of it.
func TestSubstrateSatisfiesRemoteSubstrate(t *testing.T) {
	var _ sandbox.RemoteSubstrate = (*Substrate)(nil)
}

// canaryProbes installs the reactors that make the canary pair reach whatever
// outcome a case wants.
//
// The fake clientset has no exec subresource, so the PROBE is substituted through
// the same newExecutor seam every other exec assertion in this package uses: what
// a test replaces is the transport, never the argv, the Pod lifecycle, or the
// verdict logic under test.
type canaryProbes struct {
	// openReaches and closedReaches are the two legs' outcomes: whether the
	// `nc -z` inside that Pod succeeded.
	openReaches   bool
	closedReaches bool

	// ncMissing makes every probe report the "nc: not found" shape, which must
	// fail CLOSED with a diagnostic rather than be read as "closed, good".
	ncMissing bool

	// execs counts probes actually issued, so a test can assert that a cached
	// verdict is not re-probed.
	execs atomic.Int64
}

func (c *canaryProbes) install(s *Substrate) {
	s.newExecutor = func(url string) (remotecommand.Executor, error) {
		c.execs.Add(1)
		reaches := c.openReaches
		if strings.Contains(url, canaryClosedPod) {
			reaches = c.closedReaches
		}
		switch {
		case c.ncMissing:
			// `nc: not found` with the shell's own 127. This must fail CLOSED
			// with a diagnostic, never read as "the connection was refused".
			return &stubExecutor{stderr: "sh: nc: not found\n", err: utilexec.CodeExitError{Err: errors.New("exit 127"), Code: 127}}, nil
		case reaches:
			return &stubExecutor{}, nil
		default:
			// busybox `nc -z` exits 1 when the connect fails. A RESULT, not a
			// substrate failure, which is exactly what the verdict reads.
			return &stubExecutor{err: utilexec.CodeExitError{Err: errors.New("exit 1"), Code: 1}}, nil
		}
	}
}

// verifySubstrate builds an enforcing substrate whose canary Pods become Ready as
// soon as they are created, with probes wired per the case.
func verifySubstrate(t *testing.T, probes *canaryProbes) (*Substrate, *fake.Clientset) {
	t.Helper()
	cs := fake.NewClientset(defaultAPIService())
	s := newTestSubstrate(t, cs, enforcing("10.1.2.3"))
	readyOnCreate(t, cs, s)
	probes.install(s)
	return s, cs
}

// defaultAPIService is the `kubernetes` Service every real cluster has and the
// generated fake does not. Verify READS the ClusterIP from it rather than
// hardcoding one — the address is allocated per cluster from the service CIDR —
// so the fixture must supply it.
func defaultAPIService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: canaryAPIService, Namespace: "default"},
		Spec:       corev1.ServiceSpec{ClusterIP: "10.96.0.1"},
	}
}

// A Service fuse cannot read is a REFUSAL, not a probe against a guessed address:
// a leg-1 failure against an address nobody allocated says nothing about policy.
func TestVerifyRefusesWithoutTheAPIService(t *testing.T) {
	cs := fake.NewClientset()
	s := newTestSubstrate(t, cs, enforcing("10.1.2.3"))
	readyOnCreate(t, cs, s)
	(&canaryProbes{openReaches: true, closedReaches: false}).install(s)

	err := s.Verify(context.Background())
	if err == nil {
		t.Fatal("Verify passed with no default/kubernetes Service to probe")
	}
	if !strings.Contains(err.Error(), canaryAPIService) {
		t.Errorf("Verify = %q, must name the %q Service", err, canaryAPIService)
	}
	assertNoCanaryPods(t, cs)
}

// A headless Service (ClusterIP: None) has no address to probe, so it refuses for
// the same reason an absent one does.
func TestVerifyRefusesHeadlessAPIService(t *testing.T) {
	svc := defaultAPIService()
	svc.Spec.ClusterIP = corev1.ClusterIPNone
	cs := fake.NewClientset(svc)
	s := newTestSubstrate(t, cs, enforcing("10.1.2.3"))
	readyOnCreate(t, cs, s)
	(&canaryProbes{openReaches: true, closedReaches: false}).install(s)

	if err := s.Verify(context.Background()); err == nil {
		t.Fatal("Verify passed against a headless API Service")
	}
	assertNoCanaryPods(t, cs)
}

// readyOnCreate admits every Pod Running with each container Ready, so Verify's
// own wait is satisfied and the test is about the VERDICT rather than about
// readiness (which pod_test.go already covers).
func readyOnCreate(t *testing.T, cs *fake.Clientset, s *Substrate) {
	t.Helper()
	cs.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod, ok := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if !ok {
			t.Fatalf("create pods carried %T", action.(k8stesting.CreateAction).GetObject())
		}
		admitted := readyPod(s, pod)
		if err := cs.Tracker().Add(admitted); err != nil {
			return true, nil, err
		}
		return true, admitted, nil
	})
}

// ALL FOUR LEG OUTCOMES. Only the PAIR proves enforcement, which is why this is
// one table and not two tests: an "open reaches" alone could be a cluster with no
// policy at all, and a "closed fails" alone could be an absent destination. The
// pair distinguishes "policy is enforced" from both.
//
// "Both fail" is the case that matters most and the one a lone-leg check gets
// wrong: it looks like enforcement working (the closed leg failed!) and is in fact
// a cluster where nothing can reach the API server, so the closed leg's failure
// proves nothing. It must REFUSE.
func TestVerifyCanaryPairAllFourOutcomes(t *testing.T) {
	for name, tc := range map[string]struct {
		open, closed bool
		wantPass     bool
		// wantLeg is the Pod name the refusal must NAME, so an operator knows
		// which half of the pair went wrong and therefore what to look at.
		wantLeg string
	}{
		"open reaches, closed does not — the floor is proven": {
			open: true, closed: false, wantPass: true,
		},
		"open FAILS — leg 1": {
			open: false, closed: false, wantPass: false, wantLeg: canaryOpenPod,
		},
		"closed REACHES — leg 2": {
			open: true, closed: true, wantPass: false, wantLeg: canaryClosedPod,
		},
		"both fail": {
			open: false, closed: true, wantPass: false, wantLeg: canaryOpenPod,
		},
	} {
		t.Run(name, func(t *testing.T) {
			probes := &canaryProbes{openReaches: tc.open, closedReaches: tc.closed}
			s, cs := verifySubstrate(t, probes)

			err := s.Verify(context.Background())
			if tc.wantPass && err != nil {
				t.Fatalf("Verify = %v, want nil", err)
			}
			if !tc.wantPass {
				if err == nil {
					t.Fatal("Verify passed; the canary pair did not prove the floor")
				}
				if !strings.Contains(err.Error(), tc.wantLeg) {
					t.Errorf("Verify = %q, must name the failing leg %q", err, tc.wantLeg)
				}
			}

			// THE CANARY PODS ARE CLEANED UP IN EVERY CASE — pass, refuse, and
			// each individual leg outcome. A leaked canary in the canary namespace
			// counts against nothing (it has no tenant quota) and would sit there
			// until its activeDeadlineSeconds, which is hours.
			assertNoCanaryPods(t, cs)
		})
	}
}

// An image WITHOUT `nc` makes Verify fail CLOSED with a diagnostic naming the
// image.
//
// This is the case a naive probe gets backwards: "the command failed" and "the
// connection was refused" look identical through an exit status, so an operator
// image lacking busybox `nc` would make the closed leg "pass" and the open leg
// fail — a refusal, yes, but with a reason that points at the network instead of
// at the image. The diagnostic is the whole value here.
func TestVerifyFailsClosedWhenTheImageLacksNC(t *testing.T) {
	probes := &canaryProbes{openReaches: true, closedReaches: false, ncMissing: true}
	s, cs := verifySubstrate(t, probes)

	err := s.Verify(context.Background())
	if err == nil {
		t.Fatal("Verify passed on an image with no nc; it must fail closed")
	}
	if !strings.Contains(err.Error(), s.image) {
		t.Errorf("Verify = %q, must name the image %q so the operator knows what to change", err, s.image)
	}
	assertNoCanaryPods(t, cs)
}

// THE VERDICT IS CACHED PER HANDLER, AND THE REFUSAL IS STICKY.
//
// A cluster whose CNI does not enforce NetworkPolicy is DISQUALIFIED, exactly as a
// kvm-absent host is under ADR-0044's microVM rule — it is never re-probed in the
// hope of a different answer. A retry-until-it-passes Verify is a Verify that
// eventually passes on a flake, and the thing it was gating is the network floor.
//
// The stickiness lives in the ADAPTER (remoteHandler.verifyOnce), and this test
// pins the substrate's own half: the probe runs once and the cached verdict is
// returned thereafter.
func TestVerifyCachesItsVerdict(t *testing.T) {
	t.Run("a refusal is cached", func(t *testing.T) {
		probes := &canaryProbes{openReaches: false}
		s, _ := verifySubstrate(t, probes)

		first := s.Verify(context.Background())
		if first == nil {
			t.Fatal("Verify passed; want a refusal")
		}
		after := probes.execs.Load()

		for range 3 {
			if err := s.Verify(context.Background()); err == nil {
				t.Fatal("a later Verify passed; the refusal must be STICKY")
			} else if err.Error() != first.Error() {
				t.Fatalf("later Verify = %q, want the cached %q", err, first)
			}
		}
		if got := probes.execs.Load(); got != after {
			t.Fatalf("probes issued = %d after caching, want %d: a cached verdict must not re-probe", got, after)
		}
	})

	t.Run("a pass is cached", func(t *testing.T) {
		probes := &canaryProbes{openReaches: true, closedReaches: false}
		s, _ := verifySubstrate(t, probes)

		if err := s.Verify(context.Background()); err != nil {
			t.Fatalf("Verify = %v, want nil", err)
		}
		after := probes.execs.Load()
		if err := s.Verify(context.Background()); err != nil {
			t.Fatalf("second Verify = %v, want the cached nil", err)
		}
		if got := probes.execs.Load(); got != after {
			t.Fatalf("probes issued = %d, want %d", got, after)
		}
	})
}

// THE CANARY NAMESPACE CARRIES THE SAME DEFAULT-DENY as a tenant namespace, and
// leg 2's Pod runs under it ALONE.
//
// If the canary namespace had no floor, leg 2 would reach the API server and
// Verify would refuse on every cluster — the test of the test. And if leg 1 had no
// explicit allow-all policy targeting it, leg 1 would fail on every cluster. Both
// halves are asserted here because each is a way to make Verify useless while
// still looking like it runs.
func TestVerifyCanaryNamespaceHasTheFloorAndLegOneHasItsAllow(t *testing.T) {
	probes := &canaryProbes{openReaches: true, closedReaches: false}
	s, cs := verifySubstrate(t, probes)

	if err := s.Verify(context.Background()); err != nil {
		t.Fatalf("Verify = %v", err)
	}
	ns := s.canaryNamespace()

	deny, err := cs.NetworkingV1().NetworkPolicies(ns).Get(context.Background(), policyDefaultDeny, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the canary namespace has no %q policy: %v", policyDefaultDeny, err)
	}
	if len(deny.Spec.PodSelector.MatchLabels) != 0 || len(deny.Spec.Egress) != 0 {
		t.Errorf("%q is not a default-deny: selector=%v egress=%v", policyDefaultDeny, deny.Spec.PodSelector, deny.Spec.Egress)
	}
	if len(deny.Spec.PolicyTypes) != 2 {
		t.Errorf("%q policyTypes = %v, want both directions", policyDefaultDeny, deny.Spec.PolicyTypes)
	}

	allow, err := cs.NetworkingV1().NetworkPolicies(ns).Get(context.Background(), canaryAllowPolicy, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("leg 1 has no allow policy: %v", err)
	}
	if got := allow.Spec.PodSelector.MatchLabels[labelPod]; got != canaryOpenPod {
		t.Errorf("the allow policy selects %q, want ONLY %q — a broader selector would cover leg 2 and the pair would prove nothing", got, canaryOpenPod)
	}
	if len(allow.Spec.Egress) != 1 || len(allow.Spec.Egress[0].To) != 0 {
		t.Errorf("leg 1's allow is %+v, want one unrestricted egress rule", allow.Spec.Egress)
	}
}

// Verify does not touch tenant namespaces: the canary is its own namespace, so a
// probe cannot consume a tenant's quota or be mistaken for a tenant's sandbox.
func TestVerifyUsesOnlyItsOwnNamespace(t *testing.T) {
	probes := &canaryProbes{openReaches: true, closedReaches: false}
	s, cs := verifySubstrate(t, probes)

	if err := s.Verify(context.Background()); err != nil {
		t.Fatalf("Verify = %v", err)
	}
	list, err := cs.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("List namespaces: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].Name != s.canaryNamespace() {
		names := make([]string, 0, len(list.Items))
		for _, ns := range list.Items {
			names = append(names, ns.Name)
		}
		t.Fatalf("namespaces = %v, want only the canary namespace %q", names, s.canaryNamespace())
	}
}

// A canary Pod that cannot be CREATED at all is a refusal, not a pass — and the
// leg that could not be created is named.
func TestVerifyRefusesWhenACanaryPodCannotBeCreated(t *testing.T) {
	probes := &canaryProbes{openReaches: true, closedReaches: false}
	s, cs := verifySubstrate(t, probes)
	cs.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errForbidden
	})
	_ = probes

	if err := s.Verify(context.Background()); err == nil {
		t.Fatal("Verify passed though no canary Pod could be created")
	}
	assertNoCanaryPods(t, cs)
}

// assertNoCanaryPods is the cleanup assertion, made against the WHOLE canary
// namespace rather than against the two names the substrate happened to pick: a
// delete of the wrong name would otherwise pass.
func assertNoCanaryPods(t *testing.T, cs *fake.Clientset) {
	t.Helper()
	list, err := cs.CoreV1().Pods("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return
		}
		t.Fatalf("List pods: %v", err)
	}
	for _, pod := range list.Items {
		if strings.HasPrefix(pod.Name, canaryPodPrefix) {
			t.Errorf("canary Pod %s/%s was left behind", pod.Namespace, pod.Name)
		}
	}
}

// A NetworkPolicy that cannot be asserted in the canary namespace is a refusal:
// without the floor, leg 2 is not a policed Pod and the pair proves nothing.
func TestVerifyRefusesWhenTheCanaryFloorCannotBeAsserted(t *testing.T) {
	cs := fake.NewClientset(defaultAPIService())
	s := newTestSubstrate(t, cs, enforcing("10.1.2.3"))
	readyOnCreate(t, cs, s)
	(&canaryProbes{openReaches: true, closedReaches: false}).install(s)

	cs.PrependReactor("create", "networkpolicies", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pol, ok := action.(k8stesting.CreateAction).GetObject().(*networkingv1.NetworkPolicy)
		if !ok || pol.Name != policyDefaultDeny {
			return false, nil, nil
		}
		return true, nil, errForbidden
	})

	if err := s.Verify(context.Background()); err == nil {
		t.Fatal("Verify passed though the canary namespace's floor could not be asserted")
	}
	assertNoCanaryPods(t, cs)
}
