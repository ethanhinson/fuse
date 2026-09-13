package kubernetes

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/ethanhinson/fuse/internal/loopauth"
	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// Fixed names inside a sandbox Pod. Each one is referenced from at least two
// places — the renderer and the confirmer, and for the workspace also the Exec
// path — so they are constants rather than literals: a rename that reached only
// one site would produce a Pod that passes its own read-back and fails at Exec.
const (
	// containerWorkload is the container every Exec targets. Exec addresses a
	// container BY NAME, so a cluster that renamed it is refused at Provision
	// rather than discovered at the first command.
	containerWorkload = "workload"
	// containerEgress is the forwarder sidecar (task 8). Named here because the
	// confirmer must know which container names are LEGITIMATE — anything else in
	// the Pod is an injected container sharing the loopback the forwarder listens
	// on.
	containerEgress = "egress"

	// volumeWorkspace is the per-Pod emptyDir the workspace lives on.
	volumeWorkspace = "workspace"
	// volumeEgressTLS carries the sidecar's client credential. It is mounted into
	// the SIDECAR ONLY: a workload that could read it could talk to the proxy as
	// the sidecar and skip the loopback hop entirely.
	volumeEgressTLS = "egress-tls"

	// workspaceMount is the in-Pod workspace root, and therefore the MountRoot
	// every working_dir is contained against.
	workspaceMount = "/workspace"

	// podGraceSeconds bounds teardown. Five seconds, matching the container
	// handler's disposition: a sandbox has nothing to flush.
	podGraceSeconds = int64(5)

	// sandboxUID and sandboxGID are the uid/gid every sandbox container — and
	// every canary container — runs as.
	//
	// They are FIXED rather than configurable for the reason every other object
	// name here is: an operator-settable uid buys nothing except the ability to
	// set it to 0, and the whole property being bought is "not uid 0". 65532 is
	// the `nonroot` uid distroless and several hardened base images already use,
	// so an operator building a custom workload image against a familiar
	// convention lands on the same number.
	//
	// They exist at all because `runAsNonRoot: true` on its own is a REFUSAL
	// ("unless the image declares a non-root USER"), not a selection — see the
	// pod securityContext below.
	sandboxUID = int64(65532)
	sandboxGID = int64(65532)
)

// sandboxPod is a confirmed Pod presented as a sandbox.RemoteSandbox.
//
// Every field is fixed at Provision. In particular the NAME is fixed: a later
// Teardown or Heartbeat addresses the exact object that was confirmed, never a
// name re-derived from a principal that some other Provision may since have used.
type sandboxPod struct {
	s         *Substrate
	namespace string
	name      string
	principal loopauth.Principal

	// credentialed records that this Pod was provisioned with an egress sidecar,
	// so Teardown knows there is an enrollment lease to drop and a per-Pod policy
	// to remove. It is not re-derived from the substrate's posture at teardown
	// time: the posture is fixed at construction, but reading the flag captured at
	// Provision is what makes the release exactly match the enrollment.
	credentialed bool
}

var _ sandbox.RemoteSandbox = (*sandboxPod)(nil)

// Exec lives in exec.go: the argv rendering it does is the whole boundary
// between the environment fuse resolved and the environment the command observes.

// ID is "<namespace>/<pod>", the value that reaches every sandbox event's
// ContainerID and the Pool's container-id certification.
func (p *sandboxPod) ID() string { return p.namespace + "/" + p.name }

// MountRoot is the in-Pod workspace root. It is the substrate's own trusted
// answer, which is what the adapter contains a model-supplied working_dir
// against.
func (p *sandboxPod) MountRoot() string { return workspaceMount }

// Principal is the identity this Pod was provisioned for, fixed for its life.
func (p *sandboxPod) Principal() loopauth.Principal { return p.principal }

// podName is the DETERMINISTIC Pod name for one principal and one Acquire nonce.
//
// Determinism is not cosmetic here. A Create that fails on the wire is ambiguous
// — the API server may have accepted the object before the response was lost —
// and the only way to clean that up is to Get-then-Delete a name fuse can
// compute without having seen the response. A server-generated name (generateName)
// would make a half-created Pod one that fuse can only forget.
//
// The digest covers tenant, subject AND the caller-supplied nonce, so two
// principals cannot collide (one Provision could otherwise adopt or delete
// another's Pod) and two concurrent Acquires by one principal cannot either.
func podName(p loopauth.Principal, nonce string) string {
	sum := sha256.Sum256([]byte(string(p.Tenant) + "\x00" + p.Subject))
	// A hyphen-separated nonce keeps the two halves readable in kubectl output
	// while the 12 hex digits carry the identity scoping.
	name := "sb-" + hex.EncodeToString(sum[:])[:12]
	if slug := dns1123Slug(nonce, 24); slug != "" {
		name += "-" + slug
	}
	return name
}

// principalDigest is the label value that records WHICH principal owns a Pod.
//
// It is a hash and never the identity. A label value is readable by anyone with
// namespace-list, is capped at 63 bytes, and must be DNS-safe — three independent
// reasons a raw subject cannot go there, any one of which is sufficient.
func principalDigest(p loopauth.Principal) string {
	sum := sha256.Sum256([]byte(string(p.Tenant) + "\x00" + p.Subject))
	return hex.EncodeToString(sum[:])[:16]
}

// newNonce is the per-Acquire uniquifier. crypto/rand rather than a counter so
// two instances (which share no counter) cannot mint the same Pod name for one
// principal.
func newNonce() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("kubernetes: nonce: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// Provision creates, CONFIRMS, and returns a Pod, or returns an error having
// left nothing behind.
//
// The three phases are deliberately separate and the order is load-bearing:
//
//  1. Ensure the namespace WITH its floor. A Pod created before the default-deny
//     policy exists is a Pod that is briefly unpoliced, and "briefly" is all a
//     command needs.
//  2. Create the Pod. A wire failure here is ambiguous and is cleaned up by the
//     deterministic name, not forgotten.
//  3. Confirm the ADMITTED object — poll to Running-and-Ready, then assert the
//     posture field by field. Any drift, and the Pod is deleted before the error
//     is returned.
//
// The contract this keeps (sandbox.RemoteSubstrate.Provision) is that a returned
// sandbox is confirmed and an unconfirmable one is already gone.
func (s *Substrate) Provision(ctx context.Context, p loopauth.Principal, _ sandbox.RemoteSpec) (sandbox.RemoteSandbox, error) {
	// The RemoteSpec argument is deliberately unused: the posture demanded is
	// the one resolved at construction from the trusted, load-once Config (see
	// Substrate's doc comment). Honouring a per-call spec would mean two Pods in
	// one process could be running different postures, and "which posture is this
	// Pod" would stop having an answer. The parameter stays because it is the
	// seam's shape and a future substrate may need it.

	ns, err := s.ensureNamespace(ctx, p.Tenant)
	if err != nil {
		return nil, err
	}

	nonce, err := newNonce()
	if err != nil {
		return nil, err
	}
	name := podName(p, nonce)

	// The Pod and the Secret its sidecar mounts are ONE rendering decision: under
	// enforce a Pod with a sidecar and no Secret never starts, and a Secret with
	// no Pod is litter nothing collects. Under allow-all secret is nil.
	want, secret, err := s.renderPodWithEgress(ns, name, p, time.Now().UTC())
	if err != nil {
		// The enrollment failed, so there is nothing to release and nothing to
		// delete: no object has been created yet.
		return nil, err
	}

	// THE ORDER BELOW IS LOAD-BEARING, and it is the safe direction rather than
	// the fast one:
	//
	//  1. the Secret, because the Pod's sidecar mounts it and a Pod created first
	//     would sit in ContainerCreating until it appeared;
	//  2. the per-Pod egress POLICY, because a Pod admitted before its policy is a
	//     Pod governed for that window by the namespace default-deny alone — which
	//     is strictly tighter than intended, and so is the direction to fail in;
	//  3. the Pod.
	//
	// Every failure past step 1 cleans up what came before it. The reverse order
	// would open a window in which a live Pod had no per-Pod policy, and "briefly
	// unpoliced" is the one outcome ADR-0058 rule 4 forbids.
	if secret != nil {
		if _, serr := s.cs.CoreV1().Secrets(ns).Create(ctx, secret, metav1.CreateOptions{}); serr != nil && !apierrors.IsAlreadyExists(serr) {
			s.releaseCredential(p)
			return nil, fmt.Errorf("kubernetes: create egress secret %s/%s: %w", ns, secret.Name, serr)
		}
	}
	if err := s.applyPolicy(ctx, ns, s.renderEgressPolicy(ns, name)); err != nil {
		s.cleanupEgress(context.WithoutCancel(ctx), ns, name, secret != nil)
		s.releaseCredential(p)
		return nil, err
	}

	admitted, err := s.cs.CoreV1().Pods(ns).Create(ctx, want, metav1.CreateOptions{})
	if err != nil {
		// AMBIGUOUS. The object may have landed before the response was lost, so
		// the cleanup is a delete against the name we chose — which is why the
		// name is derived and not server-generated. context.WithoutCancel because
		// a cancelled caller context is the common reason we are here, and a
		// cleanup issued on it would not leave the process.
		s.deletePodBestEffort(context.WithoutCancel(ctx), ns, name)
		s.cleanupEgress(context.WithoutCancel(ctx), ns, name, secret != nil)
		s.releaseCredential(p)
		return nil, fmt.Errorf("kubernetes: create pod %s/%s: %w", ns, name, err)
	}

	// The Secret is adopted by the ADMITTED Pod, which is the first moment its UID
	// exists. From here Kubernetes owns the Secret's life: the Pod can end in ways
	// fuse never observes (activeDeadlineSeconds, a node failure, a kubectl
	// delete), and a Secret whose deletion depended on fuse would survive all of
	// them carrying a client certificate the proxy may already have revoked.
	if secret != nil {
		s.adoptSecretBestEffort(ctx, ns, secret.Name, admitted)
	}

	confirmed, err := s.confirm(ctx, ns, name, want, admitted)
	if err != nil {
		// EVERY failure past Create deletes. A refusal that forgets the delete is
		// worse than no check: the operator gets an error while a wrongly-postured
		// Pod keeps running and keeps consuming the tenant's quota.
		s.deletePodBestEffort(context.WithoutCancel(ctx), ns, name)
		// The Secret is owned by the Pod now, so deleting the Pod collects it in a
		// real cluster; cleanupEgress removes it explicitly anyway, because the
		// garbage collector is asynchronous and the policy has no owner at all.
		s.cleanupEgress(context.WithoutCancel(ctx), ns, name, secret != nil)
		s.releaseCredential(p)
		return nil, err
	}
	_ = confirmed

	return &sandboxPod{s: s, namespace: ns, name: name, principal: p, credentialed: secret != nil}, nil
}

// releaseCredential drops the enrollment lease a failed Provision took.
//
// Without it every failed Provision leaks one lease on the principal's proxy
// listener, and a principal whose Acquires keep failing would hold a listener —
// and a live policy — forever, which is exactly the leak Release's lease counting
// exists to prevent.
func (s *Substrate) releaseCredential(p loopauth.Principal) {
	if s.credentials != nil && s.enforcing() {
		s.credentials.ReleaseSandbox(p)
	}
}

// cleanupEgress removes the per-Pod policy and Secret. Best effort: it runs on
// failure paths where the error that brought us here is the one worth reporting,
// and a NotFound is the ordinary outcome for an object that was never created.
func (s *Substrate) cleanupEgress(ctx context.Context, ns, pod string, hadSecret bool) {
	_ = s.cs.NetworkingV1().NetworkPolicies(ns).Delete(ctx, egressPolicyName(pod), metav1.DeleteOptions{})
	if hadSecret {
		_ = s.cs.CoreV1().Secrets(ns).Delete(ctx, egressSecretName(pod), metav1.DeleteOptions{})
	}
}

// adoptSecretBestEffort points the Secret at the admitted Pod.
//
// Best effort because a failure here costs a leaked Secret in the worst case and
// must not fail a Provision that otherwise succeeded — the Pod is running and the
// sandbox works. The namespace quota and the reaper bound the leak.
func (s *Substrate) adoptSecretBestEffort(ctx context.Context, ns, name string, pod *corev1.Pod) {
	api := s.cs.CoreV1().Secrets(ns)
	cur, err := api.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return
	}
	adoptSecret(cur, pod)
	_, _ = api.Update(ctx, cur, metav1.UpdateOptions{})
}

// renderPod is the POSTURE FUSE DEMANDS, as one object.
//
// It is a pure function of the substrate's construction-time configuration plus
// the three arguments, with NOTHING derived from model output, a wire field, or a
// tool argument. That is what makes it golden-testable, and the golden test is
// what makes each field below a deliberate decision rather than an accident.
func (s *Substrate) renderPod(ns, name string, p loopauth.Principal, now time.Time) *corev1.Pod {
	fals := false
	tru := true

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				labelManaged:   "true",
				labelPrincipal: principalDigest(p),
				labelInstance:  s.instanceID,
				// The Pod's own name as a LABEL, because a NetworkPolicy's
				// podSelector matches labels and cannot match a name. Task 8's
				// per-Pod egress policy selects on this.
				labelPod: name,
			},
			Annotations: map[string]string{
				// RFC 3339 because Reap parses it. An unparseable value must not
				// read as "fresh" — see reapNamespace.
				annotationHeartbeat: now.Format(time.RFC3339),
			},
		},
		Spec: corev1.PodSpec{
			// gate 5. Explicitly false, never left nil: nil DEFAULTS TO TRUE at
			// the API server, so the absence of this field is the presence of a
			// projected service-account token — an ambient credential the
			// in-sandbox env scrub can neither see nor remove.
			AutomountServiceAccountToken: &fals,
			// gate 6. A ZERO-RBAC identity, never fuse's own: the credential that
			// provisions sandboxes must not be reachable from inside one.
			ServiceAccountName: s.serviceAccount,

			// gate 1/3. hostNetwork is the one that matters most: a Pod on the
			// node's network namespace is a Pod NO NetworkPolicy applies to, so
			// the metadata floor would simply not exist.
			HostNetwork: false,
			HostPID:     false,
			HostIPC:     false,

			// Kubernetes injects every Service's host and port as environment
			// variables unless this is false. The workload's env is meant to be
			// exactly the resolved allowlist and nothing else.
			EnableServiceLinks: &fals,

			// rule 2's GC backstop: this ends the Pod even with no fuse instance
			// alive to reap it, and even if the reaper itself is broken.
			ActiveDeadlineSeconds: ptrInt64(int64(s.podMaxLifetime.Seconds())),

			// A sandbox Pod is disposable. A restarting one would outlive the
			// teardown a timed-out command depends on.
			RestartPolicy:                 corev1.RestartPolicyNever,
			TerminationGracePeriodSeconds: ptrInt64(podGraceSeconds),

			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: &tru,
				// AN EXPLICIT UID, and it is not decoration.
				//
				// `runAsNonRoot: true` ALONE does not mean "run as some non-root
				// user". It means "REFUSE unless the IMAGE declares a non-root
				// USER", enforced by the kubelet at container-create time —
				// "container has runAsNonRoot and image will run as root". Neither
				// this substrate's pinned default (alpine:3.20) nor busybox declares
				// one, so without these three fields the demanded posture is
				// unsatisfiable by the very images the substrate ships against: the
				// Pod reaches Scheduled and then sits in CreateContainerConfigError
				// until startup_timeout, while the read-back reports nothing wrong
				// because nothing about the SPEC is wrong.
				//
				// fsGroup is the second half. The workspace is an emptyDir, created
				// root-owned; without fsGroup the Pod starts and every command fails
				// writing to /workspace, which is the same defect one layer down.
				RunAsUser:  ptrInt64(sandboxUID),
				RunAsGroup: ptrInt64(sandboxGID),
				FSGroup:    ptrInt64(sandboxGID),
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},

			Volumes: []corev1.Volume{{
				Name: volumeWorkspace,
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{
						// A bounded emptyDir. Unbounded, it fills the node's disk,
						// which is a denial of service against every other tenant
						// scheduled there.
						SizeLimit: resource.NewQuantity(s.workspaceSize, resource.BinarySI),
					},
				},
			}},

			Containers: []corev1.Container{s.renderWorkload()},

			// A REQUIRED arch pin, not a preferred one. The egress sidecar's
			// entrypoint path is arch-specific and the sidecar image is fuse's own,
			// so a Pod scheduled onto the wrong arch is — under enforce — a sandbox
			// with no egress datapath. A "preferred" affinity is a suggestion the
			// scheduler may ignore under pressure, which is exactly when it would
			// break.
			Affinity: s.archAffinity(),
		},
	}

	if s.runtimeClass != "" {
		rc := s.runtimeClass
		pod.Spec.RuntimeClassName = &rc
	}
	return pod
}

// archAffinity is the REQUIRED arch node pin, shared by the sandbox Pod and the
// canary so both are scheduled where the arch-specific sidecar entrypoint exists.
func (s *Substrate) archAffinity() *corev1.Affinity {
	return &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key:      corev1.LabelArchStable,
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{s.arch},
					}},
				}},
			},
		},
	}
}

// renderWorkload is the container the shell runs in.
func (s *Substrate) renderWorkload() corev1.Container {
	fals := false
	return corev1.Container{
		Name:  containerWorkload,
		Image: s.image,
		// The Pod is WARM: it parks, and each Exec arrives through the exec
		// subresource. `sleep infinity` rather than a shell so the Pod's PID 1 is
		// not something a command can talk to.
		Command: []string{"sleep", "infinity"},
		// EMPTY, by spec §3. The complete environment is rendered per Exec from
		// the Runner's CURRENT allowlist (task 6), which is the only thing that
		// makes the Pool's reset-on-checkout mean anything on a Pod whose
		// container environment cannot be changed after creation. One variable
		// baked here is a value that survives every ResetEnv for the Pod's whole
		// life — a rotated credential lingering in every later command.
		Env:        nil,
		EnvFrom:    nil,
		WorkingDir: workspaceMount,
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: &fals,
			Privileged:               &fals,
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
		},
		VolumeMounts: []corev1.VolumeMount{{
			Name:      volumeWorkspace,
			MountPath: workspaceMount,
		}},
		Resources: s.renderResources(),
	}
}

// renderResources maps change 0077's limits onto Kubernetes (spec §4).
//
// limits == requests on every expressed resource, which is what puts the Pod in
// the GUARANTEED QoS class. That matters concretely: Guaranteed is the only class
// the kubelet does not evict under node pressure, and a sandbox whose command
// died to an eviction reports a failure the caller cannot tell apart from its own
// bug.
//
// A cap the operator did not set is NOT expressed. Expressing an invented default
// would be fuse making a resource decision the operator did not, and under a
// namespace quota a wrong default is a Pod that cannot be admitted at all.
//
// pids and nofile have NO per-Pod expression in core Kubernetes. The loader has
// already warned (WarnLimitNotEnforceable); this renderer must not invent a
// resource name for them, because an unrecognised resource name is silently
// dropped by the API server and fuse would then believe a cap it does not have.
func (s *Substrate) renderResources() corev1.ResourceRequirements {
	list := corev1.ResourceList{}

	if s.limits.MemoryBytes != nil && *s.limits.MemoryBytes > 0 {
		list[corev1.ResourceMemory] = *resource.NewQuantity(*s.limits.MemoryBytes, resource.BinarySI)
	}
	if s.limits.CPUs != nil {
		if q, err := resource.ParseQuantity(*s.limits.CPUs); err == nil {
			list[corev1.ResourceCPU] = *resource.NewMilliQuantity(q.MilliValue(), resource.DecimalSI)
		}
	}
	// fsize bounds ONE file on the local substrate; the closest Kubernetes
	// expression is a bound on the workspace, so the emptyDir sizeLimit and this
	// ephemeral-storage limit are the same number (see resolveWorkspaceSize).
	// It is an APPROXIMATION and spec §4 records it as one.
	list[corev1.ResourceEphemeralStorage] = *resource.NewQuantity(s.workspaceSize, resource.BinarySI)

	// One ResourceList value, copied — never the same map aliased into both
	// fields, which would make a later edit to one silently change the other.
	requests := corev1.ResourceList{}
	for k, v := range list {
		requests[k] = v
	}
	return corev1.ResourceRequirements{Limits: list, Requests: requests}
}

// confirm is GATE 2: the read-back.
//
// It polls until the Pod is Running with every container Ready (bounded by
// startup_timeout), then asserts the ADMITTED spec against the demanded one field
// by field. Both halves are necessary and neither substitutes for the other: a
// Pod that is Ready may have been mutated on admission, and a Pod with a correct
// spec may never start.
//
// The assertion is against the object the API SERVER reports, not against the
// object fuse sent. That distinction is the entire point of rule 2: what fuse
// asked for is a request, and what the cluster admitted — after defaulting, after
// every mutating webhook, after every injector — is the posture the command will
// actually run under.
func (s *Substrate) confirm(ctx context.Context, ns, name string, want, admitted *corev1.Pod) (*corev1.Pod, error) {
	// The admitted object comes back from Create, so the posture assertion can
	// run IMMEDIATELY — before waiting up to startup_timeout for a Pod whose spec
	// is already wrong. Mutation happens at admission, so there is nothing to
	// gain by waiting and a whole timeout to lose.
	if err := assertPosture(want, admitted); err != nil {
		return nil, err
	}

	running, err := s.waitReady(ctx, ns, name, admitted)
	if err != nil {
		return nil, err
	}
	// Re-assert on the object that is actually RUNNING, not only on the one
	// Create returned: a mutating webhook is not the only way a spec changes, and
	// a Pod mutated after admission must not be confirmed.
	if err := assertPosture(want, running); err != nil {
		return nil, err
	}
	return running, nil
}

// waitReady polls until the Pod is Running with every container Ready, bounded by
// startup_timeout, and returns the object it observed in that state.
//
// It is separate from confirm's posture assertion because the CANARY needs exactly
// this half and none of the other: a canary Pod holds no workspace, runs no
// model-supplied command, and has no sandbox posture to assert — asserting one
// against it would be asserting the wrong thing, and a webhook that mutated it
// would not change the CNI's enforcement, which is the only question Verify asks.
func (s *Substrate) waitReady(ctx context.Context, ns, name string, admitted *corev1.Pod) (*corev1.Pod, error) {
	deadline := time.Now().Add(s.startupTimeout)
	// A short poll interval: a warm Pod's cold start is the latency every first
	// Exec pays, so the confirmation must not add meaningfully to it.
	const pollEvery = 50 * time.Millisecond

	cur := admitted
	for {
		switch {
		case cur.Status.Phase == corev1.PodFailed, cur.Status.Phase == corev1.PodSucceeded:
			// A TERMINAL phase is decided, not pending. Polling on would burn the
			// whole startup timeout to reach the same conclusion.
			return nil, fmt.Errorf("kubernetes: pod %s/%s reached terminal phase %s before becoming ready: %s",
				ns, name, cur.Status.Phase, podDiagnosis(cur))
		case cur.Status.Phase == corev1.PodRunning && allContainersReady(cur):
			return cur, nil
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("kubernetes: pod %s/%s did not become ready within %s (phase %s): %s",
				ns, name, s.startupTimeout, cur.Status.Phase, podDiagnosis(cur))
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("kubernetes: pod %s/%s: %w", ns, name, ctx.Err())
		case <-time.After(pollEvery):
		}

		next, err := s.cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("kubernetes: read back pod %s/%s: %w", ns, name, err)
		}
		cur = next
	}
}

// allContainersReady requires every container to report Ready, not merely the
// phase to be Running.
//
// Phase alone is not the readiness signal: a Pod is Running the moment ONE
// container starts, and an exec into a container that has not started yet fails
// with an opaque error at the worst possible time — the caller's first command.
func allContainersReady(pod *corev1.Pod) bool {
	if len(pod.Status.ContainerStatuses) != len(pod.Spec.Containers) {
		return false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if !cs.Ready {
			return false
		}
	}
	return true
}

// podDiagnosis renders why a Pod is not ready, for the error a caller sees.
//
// It reports the cluster's OWN waiting reasons (ImagePullBackOff, ErrImagePull,
// CreateContainerConfigError) rather than a guess, because those are the ones an
// operator can act on, and because ADR-0056 forbids fabricating a reason.
func podDiagnosis(pod *corev1.Pod) string {
	var parts []string
	if pod.Status.Reason != "" {
		parts = append(parts, "pod reason "+pod.Status.Reason)
	}
	for _, cs := range pod.Status.ContainerStatuses {
		switch {
		case cs.State.Waiting != nil && cs.State.Waiting.Reason != "":
			parts = append(parts, fmt.Sprintf("container %s waiting: %s", cs.Name, cs.State.Waiting.Reason))
		case cs.State.Terminated != nil:
			parts = append(parts, fmt.Sprintf("container %s terminated: %s (exit %d)",
				cs.Name, cs.State.Terminated.Reason, cs.State.Terminated.ExitCode))
		}
	}
	if len(parts) == 0 {
		return "no container status reported"
	}
	return strings.Join(parts, "; ")
}

// errDrift marks a read-back refusal, so a caller (and a test) can tell a posture
// drift apart from a control-plane failure.
var errDrift = errors.New("kubernetes: the admitted pod does not match the demanded posture")

// assertPosture is the field-by-field comparison, and it is the most
// security-load-bearing function in this package.
//
// # Why this is an explicit list and not reflect.DeepEqual
//
// A DeepEqual against the demanded object would fail on every Pod, because the
// API server legitimately defaults dozens of fields fuse never set
// (dnsPolicy, schedulerName, tolerations, the container's terminationMessagePath,
// …). So the comparison has to be selective — and a selective comparison is only
// as good as its list. That is why every entry below carries WHY it is there: a
// future editor deleting one must first disagree with the stated reason.
//
// # The two directions
//
// Some assertions are "this must be X" (automountServiceAccountToken false) and
// some are "there must be NOTHING of this kind" (no hostPath, no projected
// volume, no extra container). The second kind is what catches an injector, which
// does not change fuse's fields at all — it ADDS. A check that only compared the
// fields fuse set would pass a Pod with a mounted docker socket beside them.
func assertPosture(want, got *corev1.Pod) error {
	var faults []string
	fault := func(format string, args ...any) {
		faults = append(faults, fmt.Sprintf(format, args...))
	}

	// --- pod-level, the flags ---

	// nil is NOT false here: an absent automountServiceAccountToken defaults to
	// TRUE at the API server, so treating nil as acceptable would invert this
	// check completely.
	if got.Spec.AutomountServiceAccountToken == nil || *got.Spec.AutomountServiceAccountToken {
		fault("automountServiceAccountToken is %s, want explicit false (a projected service-account token is an ambient credential the env scrub cannot see)", boolPtr(got.Spec.AutomountServiceAccountToken))
	}
	if got.Spec.ServiceAccountName != want.Spec.ServiceAccountName {
		fault("serviceAccountName is %q, want %q (the sandbox must never hold RBAC, least of all fuse's own provisioning identity)", got.Spec.ServiceAccountName, want.Spec.ServiceAccountName)
	}
	if got.Spec.HostNetwork {
		fault("hostNetwork is true: a Pod on the node's network namespace is a Pod NO NetworkPolicy applies to, so the metadata floor does not exist")
	}
	if got.Spec.HostPID {
		fault("hostPID is true: every process on the node is visible, fuse's own included")
	}
	if got.Spec.HostIPC {
		fault("hostIPC is true: the node's IPC namespace is shared across tenants")
	}
	if got.Spec.EnableServiceLinks == nil || *got.Spec.EnableServiceLinks {
		fault("enableServiceLinks is %s, want explicit false (it injects every Service's host and port into a container whose environment must be exactly the allowlist)", boolPtr(got.Spec.EnableServiceLinks))
	}
	if got.Spec.RestartPolicy != want.Spec.RestartPolicy {
		fault("restartPolicy is %q, want %q (a restarting sandbox outlives the teardown a timed-out command depends on)", got.Spec.RestartPolicy, want.Spec.RestartPolicy)
	}
	if got.Spec.ActiveDeadlineSeconds == nil {
		fault("activeDeadlineSeconds is unset: it is the GC backstop that ends this Pod when no fuse instance is alive to reap it")
	} else if want.Spec.ActiveDeadlineSeconds != nil && *got.Spec.ActiveDeadlineSeconds > *want.Spec.ActiveDeadlineSeconds {
		// A cluster may TIGHTEN the deadline; only a loosened one is drift.
		fault("activeDeadlineSeconds is %d, want at most %d", *got.Spec.ActiveDeadlineSeconds, *want.Spec.ActiveDeadlineSeconds)
	}
	if got.Spec.RuntimeClassName == nil && want.Spec.RuntimeClassName != nil {
		fault("runtimeClassName was stripped, want %q (the operator selected a hardened runtime)", *want.Spec.RuntimeClassName)
	} else if got.Spec.RuntimeClassName != nil && want.Spec.RuntimeClassName != nil && *got.Spec.RuntimeClassName != *want.Spec.RuntimeClassName {
		fault("runtimeClassName is %q, want %q", *got.Spec.RuntimeClassName, *want.Spec.RuntimeClassName)
	}

	// --- pod securityContext ---
	if sc := got.Spec.SecurityContext; sc == nil {
		fault("pod securityContext was stripped entirely")
	} else {
		if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
			fault("pod runAsNonRoot is %s, want true (a root workload plus any container-escape primitive is node root)", boolPtr(sc.RunAsNonRoot))
		}
		// The EXPLICIT uid is asserted separately from runAsNonRoot, because the
		// two fail differently and a webhook can strip one without the other.
		// runAsNonRoot alone is a REFUSAL ("unless the image declares a non-root
		// USER"), so a stripped runAsUser turns every Pod into a
		// CreateContainerConfigError — while a runAsUser silently reset to 0 would
		// make Pods start and contradict runAsNonRoot outright. Both are drift.
		if sc.RunAsUser == nil || *sc.RunAsUser != sandboxUID {
			fault("pod runAsUser is %s, want %d; runAsNonRoot without an explicit uid refuses every image that declares no USER, and a uid of 0 contradicts it", int64Ptr(sc.RunAsUser), sandboxUID)
		}
		// A nil profile AND an explicit Unconfined are both drift, and they are
		// separate mistakes: a nil check alone passes a webhook that sets
		// Unconfined on purpose.
		if sc.SeccompProfile == nil {
			fault("pod seccompProfile was dropped, want RuntimeDefault (it is the syscall floor)")
		} else if sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
			fault("pod seccompProfile is %q, want RuntimeDefault", sc.SeccompProfile.Type)
		}
	}

	// --- arch affinity ---
	if !hasRequiredArchAffinity(got, want) {
		fault("the required arch node affinity is missing or changed: an unpinned Pod can be scheduled where the arch-specific egress sidecar entrypoint does not exist, i.e. with no egress datapath at all")
	}

	// --- volumes: the ADDITIVE checks ---
	//
	// This is where an injector is caught. It does not touch the fields fuse set;
	// it appends. hostPath is the node filesystem, and a projected volume is how
	// a service-account token arrives despite automountServiceAccountToken:false
	// — the exact route around the flag above.
	allowedVolumes := map[string]bool{volumeWorkspace: true, volumeEgressTLS: true}
	for _, v := range got.Spec.Volumes {
		switch {
		case v.HostPath != nil:
			fault("volume %q is a hostPath (%q): a sandbox must never mount the node filesystem", v.Name, v.HostPath.Path)
		case v.Projected != nil:
			fault("volume %q is projected: a projected volume is how a service-account token is injected despite automountServiceAccountToken:false", v.Name)
		case !allowedVolumes[v.Name]:
			fault("volume %q was injected; the only volumes a sandbox Pod carries are %q and %q", v.Name, volumeWorkspace, volumeEgressTLS)
		}
	}
	// The workspace must still be the per-Pod, bounded emptyDir it was demanded
	// as. Swapped for a hostPath it becomes shared and persistent across tenants;
	// with the sizeLimit removed it fills the node's disk.
	if ws, ok := volumeNamed(got, volumeWorkspace); !ok {
		fault("the %q volume is gone", volumeWorkspace)
	} else if ws.EmptyDir == nil {
		fault("the %q volume is no longer an emptyDir: a node path makes the workspace shared and persistent across tenants", volumeWorkspace)
	} else if ws.EmptyDir.SizeLimit == nil || ws.EmptyDir.SizeLimit.IsZero() {
		fault("the %q emptyDir has no sizeLimit: an unbounded workspace fills the node's disk, denying service to every other tenant on it", volumeWorkspace)
	}

	// --- containers: names first ---
	//
	// An injected container shares the Pod's network namespace and its loopback,
	// which is precisely where the egress forwarder listens. And the workload is
	// addressed BY NAME by every Exec, so a rename must fail here, loudly, rather
	// than at the caller's first command.
	wantNames := map[string]bool{}
	for _, c := range want.Spec.Containers {
		wantNames[c.Name] = true
	}
	gotNames := map[string]bool{}
	for _, c := range got.Spec.Containers {
		gotNames[c.Name] = true
		if !wantNames[c.Name] {
			fault("container %q was injected (image %q): it shares this Pod's network namespace and its loopback, where the egress forwarder listens", c.Name, c.Image)
		}
	}
	for name := range wantNames {
		if !gotNames[name] {
			fault("container %q is missing from the admitted Pod (every Exec addresses it by name)", name)
		}
	}

	// --- containers: field by field, against the demanded one ---
	for _, wc := range want.Spec.Containers {
		gc, ok := containerNamed(got, wc.Name)
		if !ok {
			continue // already reported above
		}

		if gc.Image != wc.Image {
			fault("container %q image is %q, want %q (the image is the trusted side's choice)", wc.Name, gc.Image, wc.Image)
		}
		if strings.Join(gc.Command, "\x00") != strings.Join(wc.Command, "\x00") {
			fault("container %q command is %v, want %v", wc.Name, gc.Command, wc.Command)
		}
		if gc.WorkingDir != wc.WorkingDir {
			fault("container %q workingDir is %q, want %q (it is the containment root every working_dir resolves against)", wc.Name, gc.WorkingDir, wc.WorkingDir)
		}
		// The workload's env must be EMPTY, and env/envFrom are two separate
		// routes to the same leak: a check on one alone misses the other.
		if len(wc.Env) == 0 && len(gc.Env) != 0 {
			fault("container %q has %d environment variable(s) baked in (%s); the environment is rendered PER EXEC and a baked value survives every ResetEnv", wc.Name, len(gc.Env), envNames(gc.Env))
		}
		if len(wc.EnvFrom) == 0 && len(gc.EnvFrom) != 0 {
			fault("container %q has envFrom sources; that is the same leak as env by another field", wc.Name)
		}

		if sc := gc.SecurityContext; sc == nil {
			fault("container %q securityContext was stripped entirely", wc.Name)
		} else {
			if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
				fault("container %q allowPrivilegeEscalation is %s, want explicit false (setuid binaries in the image become an escalation path)", wc.Name, boolPtr(sc.AllowPrivilegeEscalation))
			}
			if sc.Privileged != nil && *sc.Privileged {
				fault("container %q is privileged: that is root on the node", wc.Name)
			}
			if sc.Capabilities == nil {
				fault("container %q has no capability restrictions, want drop: [ALL]", wc.Name)
			} else {
				if !dropsAll(sc.Capabilities.Drop) {
					fault("container %q capabilities.drop is %v, want [ALL] (the default capability set is exactly what drop:[ALL] removes)", wc.Name, sc.Capabilities.Drop)
				}
				if len(sc.Capabilities.Add) != 0 {
					fault("container %q re-adds capabilities %v: drop:[ALL] with an add list is not drop:[ALL]", wc.Name, sc.Capabilities.Add)
				}
			}
		}

		// A hostPath VOLUME and a hostPath MOUNT are separate things to refuse:
		// an injector that mounts an already-present volume changes no volume
		// list, so a check on volumes alone would miss it.
		for _, m := range gc.VolumeMounts {
			v, ok := volumeNamed(got, m.Name)
			if ok && v.HostPath != nil {
				fault("container %q mounts hostPath volume %q at %q", wc.Name, m.Name, m.MountPath)
			}
			// The egress credential is the SIDECAR's. A workload that can read it
			// can talk to the proxy as the sidecar and skip the loopback hop.
			if m.Name == volumeEgressTLS && wc.Name == containerWorkload {
				fault("the %q Secret is mounted into the workload container at %q; it belongs to the egress sidecar only", volumeEgressTLS, m.MountPath)
			}
		}

		// Limits may be TIGHTENED by a LimitRange but never loosened, and a
		// removed limit is the loosest possible change.
		for name, wantQ := range wc.Resources.Limits {
			gotQ, ok := gc.Resources.Limits[name]
			if !ok {
				fault("container %q lost its %q limit (%s)", wc.Name, name, wantQ.String())
				continue
			}
			if gotQ.Cmp(wantQ) > 0 {
				fault("container %q %q limit is %s, want at most %s", wc.Name, name, gotQ.String(), wantQ.String())
			}
		}
	}

	if len(faults) == 0 {
		return nil
	}
	sort.Strings(faults)
	return fmt.Errorf("%w: %s", errDrift, strings.Join(faults, "; "))
}

// hasRequiredArchAffinity reports whether got still carries the demanded REQUIRED
// arch node affinity. A "preferred" affinity does not count: it is a suggestion
// the scheduler may ignore under pressure, which is exactly when it would matter.
func hasRequiredArchAffinity(got, want *corev1.Pod) bool {
	wantArch := ""
	for _, t := range archTerms(want) {
		wantArch = t
	}
	if wantArch == "" {
		return true // nothing was demanded
	}
	for _, t := range archTerms(got) {
		if t == wantArch {
			return true
		}
	}
	return false
}

func archTerms(pod *corev1.Pod) []string {
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil ||
		pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return nil
	}
	var out []string
	for _, term := range pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Key == corev1.LabelArchStable && expr.Operator == corev1.NodeSelectorOpIn {
				out = append(out, expr.Values...)
			}
		}
	}
	return out
}

func dropsAll(drop []corev1.Capability) bool {
	for _, c := range drop {
		if c == "ALL" {
			return true
		}
	}
	return false
}

func containerNamed(pod *corev1.Pod, name string) (corev1.Container, bool) {
	for _, c := range pod.Spec.Containers {
		if c.Name == name {
			return c, true
		}
	}
	return corev1.Container{}, false
}

func volumeNamed(pod *corev1.Pod, name string) (corev1.Volume, bool) {
	for _, v := range pod.Spec.Volumes {
		if v.Name == name {
			return v, true
		}
	}
	return corev1.Volume{}, false
}

func envNames(env []corev1.EnvVar) string {
	names := make([]string, 0, len(env))
	for _, e := range env {
		names = append(names, e.Name)
	}
	return strings.Join(names, ",")
}

func boolPtr(p *bool) string {
	if p == nil {
		return "nil (which DEFAULTS TO TRUE)"
	}
	return fmt.Sprintf("%v", *p)
}

// int64Ptr renders an optional numeric field for a drift diagnostic. "nil" is
// spelled out rather than shown as 0, because for runAsUser those two are
// different failures — nil refuses every image with no USER, 0 contradicts
// runAsNonRoot.
func int64Ptr(p *int64) string {
	if p == nil {
		return "nil"
	}
	return fmt.Sprintf("%d", *p)
}

// Teardown deletes the Pod. It is idempotent and treats "already gone" as
// success, because Release runs on error paths and from defers where the sandbox
// may well already be down.
//
// NAMESPACES ARE NEVER DELETED. A namespace delete cascades to every object in
// it, so one principal's teardown would take a concurrent Provision's Pod (in the
// same tenant namespace) with it. Namespaces are cheap and the ResourceQuota
// bounds what accumulates inside them.
// teardownEgress releases the enrollment lease and removes the per-Pod policy.
//
// The Secret is NOT deleted here: it carries an ownerReference to the Pod, so
// Kubernetes collects it, and a path that also deleted it explicitly would be a
// second mechanism doing the same job — one of which would rot. The POLICY has no
// owner (a NetworkPolicy cannot be owned by the Pod it selects without the Pod's
// UID at render time, which is before the Pod exists), so it is fuse's to remove.
func (p *sandboxPod) teardownEgress(ctx context.Context) {
	if !p.credentialed {
		return
	}
	_ = p.s.cs.NetworkingV1().NetworkPolicies(p.namespace).Delete(ctx, egressPolicyName(p.name), metav1.DeleteOptions{})
	p.s.releaseCredential(p.principal)
}

func (p *sandboxPod) Teardown(ctx context.Context) error {
	// The egress teardown runs FIRST and unconditionally: the credential must stop
	// working no later than the Pod stops existing, and a Pod whose delete fails
	// (the API server is unreachable, say) is a Pod whose sidecar is still holding
	// a usable client certificate. Revoking first means the worst case is a
	// surviving Pod with no egress rather than a surviving Pod with egress.
	p.teardownEgress(ctx)

	grace := podGraceSeconds
	propagation := metav1.DeletePropagationBackground
	err := p.s.cs.CoreV1().Pods(p.namespace).Delete(ctx, p.name, metav1.DeleteOptions{
		GracePeriodSeconds: &grace,
		PropagationPolicy:  &propagation,
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("kubernetes: delete pod %s: %w", p.ID(), err)
	}
	return nil
}

// Heartbeat records that a live instance still owns this Pod.
//
// It is a merge PATCH of the one annotation rather than a read-modify-write
// Update: the heartbeat fires from a background goroutine every IdleTTL/3, and an
// Update would carry the whole object and lose a conflict race against anything
// else touching the Pod for no benefit.
func (p *sandboxPod) Heartbeat(ctx context.Context) error {
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`,
		annotationHeartbeat, time.Now().UTC().Format(time.RFC3339))
	if _, err := p.s.cs.CoreV1().Pods(p.namespace).Patch(
		ctx, p.name, types.MergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("kubernetes: heartbeat %s: %w", p.ID(), err)
	}
	return nil
}

// Reap deletes orphaned Pods substrate-wide and reports how many.
//
// It selects on fuse.dev/managed plus HEARTBEAT AGE and deliberately IGNORES
// fuse.dev/instance. That is not a simplification: an orphan is precisely a Pod
// whose owning instance is gone, so a reaper that only collected its own label
// could never collect one — the instances that can see the orphan are exactly the
// ones that do not own it.
//
// Safe to run from N instances concurrently: losing the race to delete (NotFound)
// is a success, not an error, or every multi-instance deployment would log reap
// failures forever.
func (s *Substrate) Reap(ctx context.Context, staleAfter time.Duration) (int, error) {
	// Namespaces fuse manages, not every namespace in the cluster: a reaper that
	// enumerated all of them would be one label-selector bug away from sweeping
	// an operator's own workloads.
	nss, err := s.cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: labelManaged + "=true",
	})
	if err != nil {
		return 0, fmt.Errorf("kubernetes: list managed namespaces: %w", err)
	}

	cutoff := time.Now().Add(-staleAfter)
	deleted := 0
	var firstErr error

	for _, ns := range nss.Items {
		n, err := s.reapNamespace(ctx, ns.Name, cutoff)
		deleted += n
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return deleted, firstErr
}

// reapNamespace collects one namespace's orphans.
func (s *Substrate) reapNamespace(ctx context.Context, ns string, cutoff time.Time) (int, error) {
	pods, err := s.cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: labelManaged + "=true",
	})
	if err != nil {
		return 0, fmt.Errorf("kubernetes: list pods in %s: %w", ns, err)
	}

	deleted := 0
	var firstErr error
	for _, pod := range pods.Items {
		if !isOrphan(&pod, cutoff) {
			continue
		}
		grace := podGraceSeconds
		propagation := metav1.DeletePropagationBackground
		err := s.cs.CoreV1().Pods(ns).Delete(ctx, pod.Name, metav1.DeleteOptions{
			GracePeriodSeconds: &grace,
			PropagationPolicy:  &propagation,
		})
		switch {
		case err == nil, apierrors.IsNotFound(err):
			// NotFound means another instance's reaper got there first, which is
			// the outcome this reaper wanted.
			deleted++
		case firstErr == nil:
			firstErr = fmt.Errorf("kubernetes: delete orphan %s/%s: %w", ns, pod.Name, err)
		}
	}
	return deleted, firstErr
}

// isOrphan decides whether a managed Pod's heartbeat is old enough to collect.
//
// A MISSING or UNPARSEABLE heartbeat counts as stale. That is the fail-safe
// direction and it is deliberate: treating it as fresh would make one bad
// annotation write produce a Pod no reaper can ever collect, which is a permanent
// leak against the tenant's quota. The Pod is fuse's own (the managed label got
// it here), and a live one is beating every IdleTTL/3, so an unreadable heartbeat
// is not a state a healthy sandbox is ever in.
func isOrphan(pod *corev1.Pod, cutoff time.Time) bool {
	raw := pod.Annotations[annotationHeartbeat]
	if raw == "" {
		return true
	}
	beat, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return true
	}
	return beat.Before(cutoff)
}

// deletePodBestEffort is the cleanup every refusal path runs.
//
// It is best-effort by NECESSITY, not by preference: it runs while an error is
// already being returned, so there is nothing useful to do with a second error.
// What matters is that it is always ATTEMPTED, and that it targets the
// deterministic name — so a Pod the API server accepted before the response was
// lost is deleted rather than merely forgotten.
func (s *Substrate) deletePodBestEffort(ctx context.Context, ns, name string) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	grace := podGraceSeconds
	propagation := metav1.DeletePropagationBackground
	_ = s.cs.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{
		GracePeriodSeconds: &grace,
		PropagationPolicy:  &propagation,
	})
}

func ptrInt64(v int64) *int64 { return &v }
