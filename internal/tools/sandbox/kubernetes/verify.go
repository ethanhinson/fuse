package kubernetes

// VERIFY — THE CANARY PAIR (change 0075, task 9; ADR-0058 rule 3, spec §3).
//
// This is gate 3: either the network floor is PROVEN on this cluster or the
// handler is disqualified. Nothing softer is available, because the thing being
// checked is whether the cluster's CNI enforces NetworkPolicy AT ALL — and a
// cluster whose CNI does not (Flannel without a policy plugin, a bare kubenet, a
// managed cluster with policy switched off) accepts every NetworkPolicy object
// fuse creates, reports them back unchanged, and enforces none of them. The
// default-deny reads as present and is decoration. There is no field to inspect
// and no API to ask; the only way to know is to try.
//
// # Why a PAIR, and why "both fail" is a refusal
//
// A single probe cannot distinguish the two things that matter:
//
//   - A policed Pod failing to reach a destination could mean the policy is
//     enforced, or it could mean the destination is unreachable from this cluster
//     for reasons having nothing to do with policy.
//   - An unpoliced Pod reaching a destination could mean policy is absent, or it
//     could mean this particular Pod was correctly allowed.
//
// So: leg 1 is a Pod with an EXPLICIT allow-all policy targeting it, which MUST
// reach the API server's ClusterIP. Leg 2 is a Pod under the default-deny ALONE,
// which MUST FAIL to reach the same address. Only both together say "policy
// decides reachability here".
//
// "Both fail" is therefore a REFUSAL, and it is the outcome a lone-leg check gets
// backwards: leg 2 failing looks like enforcement working, while in fact the
// cluster cannot reach the API server at all and leg 2's failure proves nothing.
// Any outcome but (open reaches, closed does not) refuses, naming the leg.
//
// # kubernetes.default, and why `nc`
//
// The destination is the `kubernetes.default` Service's ClusterIP on 443: it is
// present in every cluster, reachable from every Pod that is allowed to reach
// anything, and needs no operator setup. The probe is busybox `nc -z -w 3` —
// present in the pinned default image — because it answers exactly the question
// being asked (did a TCP connect succeed) with an exit status and nothing else. An
// image LACKING it makes Verify fail CLOSED with a diagnostic naming the image,
// because "the command was not found" and "the connection was refused" are
// indistinguishable through an exit status, and reading the former as the latter
// would make leg 2 "pass" on a cluster that enforces nothing.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/utils/exec"
)

const (
	// canaryPodPrefix is the shared prefix of both legs, so a cleanup or an
	// operator's `kubectl get pods` can identify them as a group.
	canaryPodPrefix = "canary-"
	// canaryOpenPod is leg 1: explicitly allowed, must REACH.
	canaryOpenPod = canaryPodPrefix + "open"
	// canaryClosedPod is leg 2: default-deny only, must FAIL.
	canaryClosedPod = canaryPodPrefix + "closed"

	// canaryAllowPolicy is leg 1's explicit allow. It selects canaryOpenPod BY
	// NAME LABEL and nothing else: a broader selector would cover leg 2 as well,
	// both legs would reach, and the pair would prove nothing while still looking
	// like it ran.
	canaryAllowPolicy = "fuse-canary-allow"

	// canaryNamespaceSuffix is appended to the namespace prefix. The canary lives
	// in its own namespace so a probe never consumes a tenant's quota and is never
	// mistaken for a tenant's sandbox.
	canaryNamespaceSuffix = "-canary"

	// canaryProbeTimeout bounds `nc -z -w 3` plus its exec round trip. The -w 3
	// inside the container is the real bound; this is the transport's.
	canaryProbeTimeout = 20 * time.Second

	// canaryConnectWait is nc's own connect timeout in seconds. Short: a policed
	// Pod's connect is DROPPED rather than refused, so leg 2's failure arrives as
	// a timeout and this is how long Verify waits for it.
	canaryConnectWait = "3"

	// canaryAPIPort is the API server's Service port.
	canaryAPIPort = "443"
	// canaryAPIService is the Service every cluster has.
	canaryAPIService = "kubernetes"
)

// verifyOnce and verifyErr are the SUBSTRATE's half of the sticky verdict.
//
// The adapter (sandbox.remoteHandler) caches it too, and both halves are wanted:
// the adapter's is what makes every Acquire refuse, and this one is what makes a
// Verify called directly — by a second adapter over the same substrate, or by the
// composition root's own startup check — return the same answer without probing
// again. A Verify that re-probed would eventually pass on a flake, and what it
// gates is the network floor.
type canaryVerdict struct {
	once sync.Once
	err  error
}

// canaryNamespace is where both legs run.
func (s *Substrate) canaryNamespace() string {
	return s.namespacePrefix + canaryNamespaceSuffix
}

// Verify proves the cluster's network floor with the canary pair, once.
//
// A non-nil error DISQUALIFIES the substrate for the process's lifetime (the
// adapter refuses every Acquire with it), which is why every return below names
// what went wrong in terms an operator can act on: which leg, and — for the image
// case — which image.
func (s *Substrate) Verify(ctx context.Context) error {
	s.verdict.once.Do(func() { s.verdict.err = s.runCanaryPair(ctx) })
	return s.verdict.err
}

// runCanaryPair is one execution of the probe, with cleanup on EVERY path.
func (s *Substrate) runCanaryPair(ctx context.Context) error {
	ns := s.canaryNamespace()

	if err := s.ensureCanaryNamespace(ctx, ns); err != nil {
		return err
	}

	// Both legs are cleaned up unconditionally, on a context of our own making:
	// the ctx that brought us here may be the caller's and may already be
	// cancelled on a refusal path, and a cleanup issued on it would not leave the
	// process — leaking two Pods that then sit until activeDeadlineSeconds.
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), canaryProbeTimeout)
		defer cancel()
		for _, name := range []string{canaryOpenPod, canaryClosedPod} {
			s.deletePodBestEffort(cctx, ns, name)
		}
	}()

	// Leg 1's allow is asserted BEFORE leg 1's Pod. A Pod that started under the
	// default-deny and was allowed a moment later could have run its probe in the
	// window and reported a failure that says nothing about the cluster — which
	// would refuse a perfectly good cluster, the one direction of error that is
	// merely annoying rather than unsafe, but is still a wrong answer.
	if err := s.applyPolicy(ctx, ns, s.renderCanaryAllow(ns)); err != nil {
		return err
	}

	target, err := s.canaryTarget(ctx)
	if err != nil {
		return err
	}

	openReached, openErr := s.runCanaryLeg(ctx, ns, canaryOpenPod, target)
	if openErr != nil {
		return openErr
	}
	closedReached, closedErr := s.runCanaryLeg(ctx, ns, canaryClosedPod, target)
	if closedErr != nil {
		return closedErr
	}

	// THE VERDICT. Exactly one combination passes.
	switch {
	case openReached && !closedReached:
		return nil
	case !openReached && !closedReached:
		// BOTH FAILED. Named as leg 1, deliberately: leg 1 is the one that should
		// have succeeded, and the honest reading is "nothing in this cluster can
		// reach the API server", which makes leg 2's failure uninformative.
		return fmt.Errorf("%w: leg 1 (%s) could not reach %s even with an explicit allow-all policy, and leg 2 (%s) could not either — nothing in this cluster reaches the API server, so leg 2's failure proves nothing about NetworkPolicy enforcement",
			errFloorUnproven, canaryOpenPod, target, canaryClosedPod)
	case !openReached:
		return fmt.Errorf("%w: leg 1 (%s) could not reach %s despite an explicit allow-all policy targeting it; the canary cannot establish a baseline, so enforcement cannot be proved",
			errFloorUnproven, canaryOpenPod, target)
	default:
		// closedReached: THE DANGEROUS OUTCOME. The default-deny is present as an
		// object and is not enforced, so every sandbox Pod fuse has ever created
		// on this cluster has had unrestricted egress — including to the metadata
		// endpoint.
		return fmt.Errorf("%w: leg 2 (%s) REACHED %s under the %s policy alone; this cluster's CNI does not enforce NetworkPolicy, so the metadata-deny floor does not exist and no sandbox on it is contained",
			errFloorUnproven, canaryClosedPod, target, policyDefaultDeny)
	}
}

// errFloorUnproven is the sentinel every canary refusal wraps. The adapter turns
// it into a sandbox.health event with reason floor_unverified.
var errFloorUnproven = errors.New("kubernetes: the network floor could not be proved by the canary pair")

// ensureCanaryNamespace creates the canary namespace and asserts THE SAME
// default-deny a tenant namespace carries.
//
// The same floor, not a weaker one: leg 2's entire meaning is "a Pod under the
// floor fuse actually ships". A canary namespace with a different policy would
// prove something about a policy no sandbox runs under.
func (s *Substrate) ensureCanaryNamespace(ctx context.Context, ns string) error {
	if _, err := s.cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{}); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("kubernetes: canary namespace %s: %w", ns, err)
		}
		obj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:   ns,
			Labels: map[string]string{labelManaged: "true"},
		}}
		if _, err := s.cs.CoreV1().Namespaces().Create(ctx, obj, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("kubernetes: create canary namespace %s: %w", ns, err)
		}
	}
	if err := s.assertDefaultDeny(ctx, ns); err != nil {
		// Without the floor, leg 2 is not a policed Pod and the pair proves
		// nothing. This must refuse rather than probe anyway.
		return fmt.Errorf("%w: the canary namespace's %s policy could not be asserted, so leg 2 would not be policed: %w", errFloorUnproven, policyDefaultDeny, err)
	}
	return nil
}

// renderCanaryAllow is leg 1's explicit allow-all egress, targeting leg 1's Pod
// and no other.
func (s *Substrate) renderCanaryAllow(ns string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      canaryAllowPolicy,
			Namespace: ns,
			Labels:    map[string]string{labelManaged: "true"},
		},
		Spec: networkingv1.NetworkPolicySpec{
			// ONLY leg 1. See canaryAllowPolicy's comment: a wider selector makes
			// the pair meaningless while still looking correct.
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{labelPod: canaryOpenPod}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			// ONE rule with an EMPTY `to`: that is NetworkPolicy's spelling of
			// "allow all egress". An empty rule LIST would mean the opposite
			// (deny everything), which is the exact inversion the default-deny
			// relies on — hence the explicit single empty rule here.
			Egress: []networkingv1.NetworkPolicyEgressRule{{}},
		},
	}
}

// canaryTarget is the API server's ClusterIP:port, read from the Service every
// cluster has.
//
// Read rather than assumed: the ClusterIP is allocated per cluster from the
// service CIDR, so there is no address to hardcode. A Service fuse cannot read is
// a refusal — probing a guessed address would produce a leg-1 failure that says
// nothing about policy.
func (s *Substrate) canaryTarget(ctx context.Context) (string, error) {
	svc, err := s.cs.CoreV1().Services("default").Get(ctx, canaryAPIService, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("%w: the default/%s Service could not be read, so the canary has no destination it can be sure is reachable: %w", errFloorUnproven, canaryAPIService, err)
	}
	ip := svc.Spec.ClusterIP
	if ip == "" || ip == corev1.ClusterIPNone {
		return "", fmt.Errorf("%w: the default/%s Service has no ClusterIP (%q), so the canary has no destination", errFloorUnproven, canaryAPIService, ip)
	}
	return ip + ":" + canaryAPIPort, nil
}

// runCanaryLeg creates one canary Pod, waits for it to be Ready, probes, and
// reports whether the connect SUCCEEDED.
//
// The two returns are deliberately distinct: (reached, nil) is a PROBE RESULT the
// verdict reads, and a non-nil error is "the leg could not be run at all" — a Pod
// that would not start, an image with no `nc`. Collapsing them would make an
// unrunnable leg indistinguishable from a leg that ran and failed, and leg 2 is a
// leg whose failure is the passing outcome.
func (s *Substrate) runCanaryLeg(ctx context.Context, ns, name, target string) (bool, error) {
	pod := s.renderCanaryPod(ns, name)

	admitted, err := s.cs.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return false, fmt.Errorf("%w: canary leg %s could not be created: %w", errFloorUnproven, name, err)
		}
		// A previous run's leftover, or a concurrent Verify in another instance.
		// Delete and refuse rather than probe a Pod this call did not create: its
		// posture and its policy are unknown to us.
		s.deletePodBestEffort(context.WithoutCancel(ctx), ns, name)
		return false, fmt.Errorf("%w: canary leg %s already existed (a previous run's leftover, now deleted); re-run to verify", errFloorUnproven, name)
	}

	// Readiness only — no assertPosture. The canary is not a sandbox: it runs no
	// model-supplied command, holds no workspace, and exists for a few seconds.
	// Asserting the SANDBOX posture against it would be asserting the wrong thing,
	// and a webhook that mutates it does not make the CNI's enforcement any
	// different, which is the only question being asked.
	if _, err := s.waitReady(ctx, ns, name, admitted); err != nil {
		return false, fmt.Errorf("%w: canary leg %s never became Ready: %w", errFloorUnproven, name, err)
	}

	reached, err := s.probe(ctx, ns, name, target)
	if err != nil {
		return false, fmt.Errorf("%w: canary leg %s could not be probed: %w", errFloorUnproven, name, err)
	}
	return reached, nil
}

// renderCanaryPod is a minimal, hardened Pod carrying the labelPod label the
// policies select on.
//
// It shares the sandbox Pod's security posture but NOT its shape: no workspace, no
// sidecar, no per-Exec environment, and a short activeDeadlineSeconds, because it
// is a probe rather than an execution context.
func (s *Substrate) renderCanaryPod(ns, name string) *corev1.Pod {
	fals := false
	tru := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				labelManaged: "true",
				// The policies select on this. Without it leg 1's allow matches
				// nothing and leg 1 fails on every cluster.
				labelPod:      name,
				labelInstance: s.instanceID,
			},
			Annotations: map[string]string{
				// A canary is not a sandbox, but it carries a heartbeat so the
				// reaper collects one that a crashed Verify left behind.
				annotationHeartbeat: time.Now().UTC().Format(time.RFC3339),
			},
		},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: &fals,
			ServiceAccountName:           s.serviceAccount,
			EnableServiceLinks:           &fals,
			RestartPolicy:                corev1.RestartPolicyNever,
			// Minutes, not hours: a canary that outlived its Verify by four hours
			// would be a confusing object in an operator's namespace listing.
			ActiveDeadlineSeconds:         ptrInt64(int64((5 * time.Minute).Seconds())),
			TerminationGracePeriodSeconds: ptrInt64(podGraceSeconds),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   &tru,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:    containerWorkload,
				Image:   s.image,
				Command: []string{"sleep", "infinity"},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &fals,
					Privileged:               &fals,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
			Affinity: s.archAffinity(),
		},
	}
}

// probe runs `nc -z -w 3 <ip> 443` in the canary and reports whether it connected.
//
// The THREE outcomes are kept apart, and that separation is the whole correctness
// of this function:
//
//   - exit 0 → CONNECTED.
//   - exit non-zero with no sign of a missing `nc` → did not connect. A RESULT.
//   - any sign that `nc` was not found → an ERROR, never "did not connect". This
//     is the case the design note at the top of the file is about: reading a
//     missing binary as a refused connection makes leg 2 pass on a cluster that
//     enforces nothing, which is the precise failure this gate exists to catch.
func (s *Substrate) probe(ctx context.Context, ns, pod, target string) (bool, error) {
	host, port, ok := strings.Cut(target, ":")
	if !ok {
		return false, fmt.Errorf("kubernetes: canary target %q is not host:port", target)
	}
	argv := []string{"nc", "-z", "-w", canaryConnectWait, host, port}

	pctx, cancel := context.WithTimeout(ctx, canaryProbeTimeout)
	defer cancel()

	exec, err := s.executorFor(s.execURL(ns, pod, argv))
	if err != nil {
		return false, err
	}

	var buf syncBuffer
	streamErr := exec.StreamWithContext(pctx, remotecommand.StreamOptions{Stdout: &buf, Stderr: &buf})
	text := buf.String()

	if streamErr == nil {
		if missingNC(0, text) {
			return false, s.missingNCError(text)
		}
		return true, nil
	}

	var coded utilexec.CodeExitError
	if errors.As(streamErr, &coded) {
		if missingNC(coded.Code, text) {
			return false, s.missingNCError(text)
		}
		// The command ran and did not connect. That is the RESULT leg 2 needs.
		return false, nil
	}

	// A transport failure: the exec could not be established at all. Not a probe
	// result — there is no evidence either way — so it refuses.
	if missingNC(-1, streamErr.Error()) {
		return false, s.missingNCError(streamErr.Error())
	}
	return false, fmt.Errorf("the exec into %s/%s failed: %w", ns, pod, streamErr)
}

// missingNC recognises the shapes a shell and a kubelet use to say `nc` is not
// there.
//
// The text match and the 126/127 exit statuses are both needed: a shell that ran
// and could not find the binary reports 127 WITH a diagnostic, while a kubelet
// that could not execute what fuse named reports through the status alone. 126 is
// "found but not executable", which an image with a stub `nc` would produce.
func missingNC(code int, text string) bool {
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "nc: not found"),
		strings.Contains(lower, "nc: command not found"),
		strings.Contains(lower, "executable file not found"):
		return true
	case code == 126 || code == 127:
		// The only binary this probe names is `nc`.
		return true
	default:
		return false
	}
}

// missingNCError names the IMAGE, because the fix is to change the image and an
// operator who pinned a distroless one has no other way to learn that.
func (s *Substrate) missingNCError(text string) error {
	return fmt.Errorf("%w: the canary probe needs busybox `nc` and the workload image %q does not provide it (%s); Verify fails CLOSED here rather than reading a missing binary as a refused connection, which would make the policed leg appear to pass on a cluster that enforces nothing",
		errFloorUnproven, s.image, strings.TrimSpace(text))
}
