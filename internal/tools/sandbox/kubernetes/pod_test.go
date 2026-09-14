package kubernetes

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// readyPod is what a compliant cluster admits: the demanded spec, unmodified,
// with Running status and every container Ready.
//
// It is derived from the substrate's OWN rendering on purpose. A hand-written
// "good" Pod would drift from the renderer and turn every drift test below into
// a test of the fixture. Each drift test then applies exactly ONE mutation to
// this, so what the test proves is that the read-back catches that one field —
// not that it catches some unspecified difference.
func readyPod(s *Substrate, pod *corev1.Pod) *corev1.Pod {
	admitted := pod.DeepCopy()
	admitted.Status.Phase = corev1.PodRunning
	for _, c := range admitted.Spec.Containers {
		admitted.Status.ContainerStatuses = append(admitted.Status.ContainerStatuses, corev1.ContainerStatus{
			Name:  c.Name,
			Ready: true,
		})
	}
	return admitted
}

// admitWith installs a create reactor that admits the Pod after passing it
// through mutate — the webhook the cluster is pretending to have.
//
// The mutated object is what lands in the tracker, so a later Get (the
// read-back) and a later Delete both see the same object a real cluster would
// have. That is what lets the "no Pod left behind" assertions be honest.
func admitWith(t *testing.T, cs *fake.Clientset, s *Substrate, mutate func(*corev1.Pod)) {
	t.Helper()
	cs.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod, ok := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if !ok {
			t.Fatalf("create pods action carried %T", action.(k8stesting.CreateAction).GetObject())
		}
		admitted := readyPod(s, pod)
		if mutate != nil {
			mutate(admitted)
		}
		// Let the tracker store the mutated object, so Get and Delete agree.
		if err := cs.Tracker().Add(admitted); err != nil {
			return true, nil, err
		}
		return true, admitted, nil
	})
}

func TestPodNameDeterministicAndScoped(t *testing.T) {
	s := newTestSubstrate(t, fake.NewClientset())

	p := loopPrincipal("acme")
	a := podName(p, "nonce1")
	b := podName(p, "nonce1")
	if a != b {
		t.Fatalf("podName is not deterministic for one principal+nonce: %q then %q", a, b)
	}
	if c := podName(p, "nonce2"); c == a {
		t.Fatalf("podName ignores the nonce: both yield %q", a)
	}
	// The principal digest is what scopes the name. Two principals sharing a
	// nonce must not share a Pod name, or one principal's Provision could adopt
	// (or delete) another's Pod.
	if c := podName(loopPrincipal("other"), "nonce1"); c == a {
		t.Fatalf("podName ignores the principal: %q for both tenants", a)
	}
	if d := podName(loopauthWithSubject("acme", "someone-else"), "nonce1"); d == a {
		t.Fatalf("podName ignores the subject: %q for both subjects", a)
	}

	for _, name := range []string{a, podName(loopPrincipal(""), ""), podName(loopPrincipal("日本語"), "n")} {
		if errs := validateDNS1123Label(name); len(errs) > 0 {
			t.Errorf("podName produced %q, not a DNS-1123 label: %v", name, errs)
		}
		if !strings.HasPrefix(name, "sb-") {
			t.Errorf("podName produced %q, want the sb- prefix", name)
		}
	}
	_ = s
}

// TestRenderPodGolden is the field-by-field golden of the demanded PodSpec, in
// the container_test.go argv-golden spirit. Every line here is a containment
// property, and a change to any of them must be a deliberate edit to this test.
func TestRenderPodGolden(t *testing.T) {
	s := newTestSubstrate(t, fake.NewClientset())
	pod := s.renderPod("fuse-sb-acme-abcd1234", "sb-deadbeef-nonce", loopPrincipal("acme"), time.Unix(1700000000, 0).UTC())

	// --- metadata ---
	if pod.Namespace != "fuse-sb-acme-abcd1234" || pod.Name != "sb-deadbeef-nonce" {
		t.Errorf("meta = %s/%s, want fuse-sb-acme-abcd1234/sb-deadbeef-nonce", pod.Namespace, pod.Name)
	}
	if pod.Labels[labelManaged] != "true" {
		t.Errorf("%s = %q, want true — this is the reaper's selector", labelManaged, pod.Labels[labelManaged])
	}
	if pod.Labels[labelInstance] != "instance-a" {
		t.Errorf("%s = %q, want instance-a", labelInstance, pod.Labels[labelInstance])
	}
	// The principal label is a HASH. A label value is readable by anyone with
	// namespace list, is capped at 63 bytes, and must be DNS-safe — three
	// separate reasons a raw subject cannot go there.
	if got := pod.Labels[labelPrincipal]; got == "" || strings.Contains(got, "acme") {
		t.Errorf("%s = %q, want a hash that does not carry the raw identity", labelPrincipal, got)
	}
	if pod.Labels[labelPod] != pod.Name {
		t.Errorf("%s = %q, want the Pod's own name (a per-Pod NetworkPolicy selects on labels, not names)", labelPod, pod.Labels[labelPod])
	}
	if got, want := pod.Annotations[annotationHeartbeat], "2023-11-14T22:13:20Z"; got != want {
		t.Errorf("%s = %q, want RFC 3339 %q", annotationHeartbeat, got, want)
	}

	// --- pod-level containment ---
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Error("automountServiceAccountToken must be explicitly false (gate 5): a projected token is an ambient credential the env scrub cannot see")
	}
	if pod.Spec.ServiceAccountName != "fuse-sandbox" {
		t.Errorf("serviceAccountName = %q, want fuse-sandbox (zero RBAC; never fuse's own)", pod.Spec.ServiceAccountName)
	}
	if pod.Spec.HostNetwork || pod.Spec.HostPID || pod.Spec.HostIPC {
		t.Errorf("hostNetwork/hostPID/hostIPC = %v/%v/%v, want all false", pod.Spec.HostNetwork, pod.Spec.HostPID, pod.Spec.HostIPC)
	}
	if pod.Spec.EnableServiceLinks == nil || *pod.Spec.EnableServiceLinks {
		t.Error("enableServiceLinks must be explicitly false: it injects every Service's host/port as env into a container whose env is meant to be exactly the allowlist")
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q, want Never (a sandbox Pod is disposable)", pod.Spec.RestartPolicy)
	}
	if pod.Spec.ActiveDeadlineSeconds == nil || *pod.Spec.ActiveDeadlineSeconds != int64((4*time.Hour).Seconds()) {
		t.Errorf("activeDeadlineSeconds = %v, want 14400 — the GC backstop that ends a Pod with no fuse instance alive to reap it", pod.Spec.ActiveDeadlineSeconds)
	}
	if pod.Spec.TerminationGracePeriodSeconds == nil || *pod.Spec.TerminationGracePeriodSeconds != 5 {
		t.Errorf("terminationGracePeriodSeconds = %v, want 5 (teardown is bounded)", pod.Spec.TerminationGracePeriodSeconds)
	}
	if pod.Spec.SecurityContext == nil {
		t.Fatal("pod securityContext is nil")
	}
	if pod.Spec.SecurityContext.RunAsNonRoot == nil || !*pod.Spec.SecurityContext.RunAsNonRoot {
		t.Error("pod runAsNonRoot must be true")
	}
	if sp := pod.Spec.SecurityContext.SeccompProfile; sp == nil || sp.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod seccompProfile = %+v, want RuntimeDefault", sp)
	}
	if pod.Spec.RuntimeClassName != nil {
		t.Errorf("runtimeClassName = %v, want nil when runtime_class is unset (the cluster's default runtime)", *pod.Spec.RuntimeClassName)
	}

	// --- arch affinity ---
	// The sidecar entrypoint path is arch-specific, so an unpinned Pod can be
	// scheduled onto a node where the forwarder binary does not exist — which
	// under enforce is a sandbox with no egress datapath at all.
	arches := affinityArches(t, pod)
	if len(arches) != 1 || arches[0] != "arm64" {
		t.Errorf("arch affinity = %v, want exactly [arm64]", arches)
	}

	// --- volumes ---
	ws := volumeByName(t, pod, volumeWorkspace)
	if ws.EmptyDir == nil {
		t.Fatalf("%s volume is %+v, want an emptyDir", volumeWorkspace, ws)
	}
	if got := ws.EmptyDir.SizeLimit; got == nil || got.Value() != 2*1024*1024*1024 {
		t.Errorf("%s sizeLimit = %v, want 2Gi", volumeWorkspace, got)
	}
	// NO hostPath, ever, under any name. A hostPath volume is a mount of the
	// node's filesystem into a sandbox, which defeats the entire boundary.
	for _, v := range pod.Spec.Volumes {
		if v.HostPath != nil {
			t.Errorf("volume %q is a hostPath (%q) — a sandbox Pod must never mount the node filesystem", v.Name, v.HostPath.Path)
		}
		if v.Projected != nil {
			t.Errorf("volume %q is projected — a projected volume is how a service-account token gets in despite automountServiceAccountToken:false", v.Name)
		}
		if v.Secret != nil && v.Name != volumeEgressTLS {
			t.Errorf("volume %q is an unexpected Secret mount", v.Name)
		}
	}

	// --- the workload container ---
	c := containerByName(t, pod, containerWorkload)
	if c.Image != "alpine:3.20" {
		t.Errorf("workload image = %q, want alpine:3.20", c.Image)
	}
	if got, want := strings.Join(c.Command, " "), "sleep infinity"; got != want {
		t.Errorf("workload command = %q, want %q (the Pod is WARM; the shell arrives per Exec)", got, want)
	}
	// EMPTY env, by spec. The complete environment is rendered per Exec from the
	// Runner's CURRENT allowlist, which is the only thing that makes the Pool's
	// reset-on-checkout mean anything on a Pod whose container env cannot change.
	// A single baked variable here is a value that survives ResetEnv forever.
	if len(c.Env) != 0 {
		t.Errorf("workload env = %+v, want EMPTY — the environment is rendered per Exec, never baked", c.Env)
	}
	if len(c.EnvFrom) != 0 {
		t.Errorf("workload envFrom = %+v, want empty for the same reason", c.EnvFrom)
	}
	if c.WorkingDir != workspaceMount {
		t.Errorf("workload workingDir = %q, want %q", c.WorkingDir, workspaceMount)
	}
	if c.SecurityContext == nil {
		t.Fatal("workload securityContext is nil")
	}
	if c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
		t.Error("workload allowPrivilegeEscalation must be explicitly false")
	}
	if c.SecurityContext.Privileged == nil || *c.SecurityContext.Privileged {
		t.Error("workload privileged must be explicitly false")
	}
	if c.SecurityContext.Capabilities == nil ||
		len(c.SecurityContext.Capabilities.Drop) != 1 ||
		c.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Errorf("workload capabilities = %+v, want drop: [ALL]", c.SecurityContext.Capabilities)
	}
	if len(c.SecurityContext.Capabilities.Add) != 0 {
		t.Errorf("workload capabilities.add = %v, want none", c.SecurityContext.Capabilities.Add)
	}
	mnt := mountByName(t, c, volumeWorkspace)
	if mnt.MountPath != workspaceMount {
		t.Errorf("workspace mountPath = %q, want %q", mnt.MountPath, workspaceMount)
	}
	// The egress TLS Secret must NOT reach the workload: it is the sidecar's
	// client credential, and a workload that can read it can talk to the proxy
	// as the sidecar, bypassing the loopback hop entirely.
	for _, m := range c.VolumeMounts {
		if m.Name == volumeEgressTLS {
			t.Errorf("the %s Secret is mounted into the WORKLOAD container at %q; it belongs to the sidecar only", volumeEgressTLS, m.MountPath)
		}
	}

	// --- limits: Guaranteed QoS ---
	assertGuaranteed(t, c)
}

// TestRenderPodLimitsGuaranteedQoS pins spec §4's mapping. limits == requests is
// what puts the Pod in the Guaranteed QoS class, which is the only class the
// kubelet will not evict under node pressure — a Burstable sandbox is a sandbox
// whose command dies for reasons the caller cannot distinguish from its own bug.
func TestRenderPodLimitsGuaranteedQoS(t *testing.T) {
	tests := []struct {
		name      string
		limits    sandbox.Limits
		wantMem   int64
		wantMilli int64
		wantEph   int64
	}{
		{
			name:      "all set",
			limits:    sandbox.Limits{MemoryBytes: ptr(int64(512 * 1024 * 1024)), CPUs: ptr("1.5"), FsizeBytes: ptr(int64(64 * 1024 * 1024))},
			wantMem:   512 * 1024 * 1024,
			wantMilli: 1500,
			// ephemeral-storage tracks the WORKSPACE bound, which the explicit
			// workspace.size_limit wins over fsize (spec §4's approximation note).
			wantEph: 2 * 1024 * 1024 * 1024,
		},
		{
			name:      "cpu only",
			limits:    sandbox.Limits{CPUs: ptr("0.25")},
			wantMilli: 250,
			wantEph:   2 * 1024 * 1024 * 1024,
		},
		{
			name:    "memory only",
			limits:  sandbox.Limits{MemoryBytes: ptr(int64(1 << 30))},
			wantMem: 1 << 30,
			wantEph: 2 * 1024 * 1024 * 1024,
		},
		{
			// pids and nofile have NO per-Pod expression in core Kubernetes. The
			// loader already warned (WarnLimitNotEnforceable); the renderer must
			// not invent a resource name for them, because a resource the API
			// server does not know is silently dropped and fuse would then
			// believe a cap it does not have.
			name:   "pids and nofile are not expressible",
			limits: sandbox.Limits{Pids: ptr(int64(64)), NoFile: ptr(int64(256))},
			// zero mem/cpu: nothing to express
			wantEph: 2 * 1024 * 1024 * 1024,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSubstrate(t, fake.NewClientset(), func(o *Options) { o.Config.Limits = tc.limits })
			pod := s.renderPod("ns", "sb-x", loopPrincipal("acme"), time.Now())
			c := containerByName(t, pod, containerWorkload)

			assertGuaranteed(t, c)

			if tc.wantMem == 0 {
				if _, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
					t.Errorf("memory limit set for an unset limits.memory: %v", c.Resources.Limits[corev1.ResourceMemory])
				}
			} else if got := c.Resources.Limits[corev1.ResourceMemory]; got.Value() != tc.wantMem {
				t.Errorf("memory limit = %s, want %d bytes", got.String(), tc.wantMem)
			}

			if tc.wantMilli == 0 {
				if _, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
					t.Errorf("cpu limit set for an unset limits.cpus: %v", c.Resources.Limits[corev1.ResourceCPU])
				}
			} else if got := c.Resources.Limits[corev1.ResourceCPU]; got.MilliValue() != tc.wantMilli {
				t.Errorf("cpu limit = %s, want %dm", got.String(), tc.wantMilli)
			}

			if got := c.Resources.Limits[corev1.ResourceEphemeralStorage]; got.Value() != tc.wantEph {
				t.Errorf("ephemeral-storage limit = %s, want %d bytes", got.String(), tc.wantEph)
			}

			for _, unexpressible := range []corev1.ResourceName{"pids", "nofile", "limits.pids"} {
				if _, ok := c.Resources.Limits[unexpressible]; ok {
					t.Errorf("resources.limits carries %q, which core Kubernetes does not know: an unknown resource name is silently dropped and fuse would believe a cap it does not have", unexpressible)
				}
			}
		})
	}
}

// assertGuaranteed is the limits == requests assertion, factored out because it
// applies to every limits shape.
func assertGuaranteed(t *testing.T, c corev1.Container) {
	t.Helper()
	if len(c.Resources.Limits) != len(c.Resources.Requests) {
		t.Errorf("limits (%d entries) and requests (%d entries) differ in size; Guaranteed QoS needs them EQUAL", len(c.Resources.Limits), len(c.Resources.Requests))
	}
	for name, lim := range c.Resources.Limits {
		req, ok := c.Resources.Requests[name]
		if !ok {
			t.Errorf("resources.limits has %q with no matching request; the Pod is Burstable, not Guaranteed, and the kubelet may evict it under node pressure", name)
			continue
		}
		if lim.Cmp(req) != 0 {
			t.Errorf("resources %q: limit %s != request %s; Guaranteed QoS requires equality", name, lim.String(), req.String())
		}
	}
}

func TestRenderPodRuntimeClass(t *testing.T) {
	s := newTestSubstrate(t, fake.NewClientset(), func(o *Options) {
		o.Config.Kubernetes.RuntimeClass = ptr("gvisor")
	})
	pod := s.renderPod("ns", "sb-x", loopPrincipal("acme"), time.Now())
	if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != "gvisor" {
		t.Fatalf("runtimeClassName = %v, want gvisor", pod.Spec.RuntimeClassName)
	}
}

// TestProvisionConfirmsCompliantPod is the happy path: a cluster that admits the
// demanded spec unmodified yields a confirmed sandbox.
func TestProvisionConfirmsCompliantPod(t *testing.T) {
	cs := fake.NewClientset()
	s := newTestSubstrate(t, cs)
	admitWith(t, cs, s, nil)

	sb, err := s.Provision(context.Background(), loopPrincipal("acme"), sandbox.RemoteSpec{})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if sb.MountRoot() != workspaceMount {
		t.Errorf("MountRoot = %q, want %q", sb.MountRoot(), workspaceMount)
	}
	ns, name, ok := strings.Cut(sb.ID(), "/")
	if !ok {
		t.Fatalf("ID = %q, want <namespace>/<pod>", sb.ID())
	}
	if sb.Principal().Tenant != "acme" {
		t.Errorf("Principal().Tenant = %q, want acme", sb.Principal().Tenant)
	}
	if _, err := cs.CoreV1().Pods(ns).Get(context.Background(), name, metav1.GetOptions{}); err != nil {
		t.Fatalf("confirmed Pod %s is not there: %v", sb.ID(), err)
	}
}

// TestProvisionRefusesDrift is GATE 2 AND GATE 5, and it is the reason this task
// is premium.
//
// Each case is ONE mutation an admission webhook, a defaulting pass, or a
// sidecar injector could plausibly apply to the admitted Pod. For each one the
// substrate must REFUSE and must leave NO POD BEHIND — a refusal that forgets
// the delete is strictly worse than no check at all, because the operator gets
// an error while a Pod with the wrong posture keeps running and keeps counting
// against the quota.
//
// "No Pod left behind" is asserted against the whole namespace, not against the
// name the substrate happened to pick, because a delete of the wrong name would
// otherwise pass.
func TestProvisionRefusesDrift(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*corev1.Pod)
		// why names the containment property the drift breaks, and is printed on
		// failure so a future reader learns what was being defended.
		why string
	}{
		{
			name: "hostPath volume added",
			why:  "a hostPath volume mounts the NODE's filesystem into the sandbox, defeating the boundary entirely",
			mutate: func(p *corev1.Pod) {
				p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{
					Name: "node-root",
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{Path: "/"},
					},
				})
			},
		},
		{
			name: "hostPath mounted into the workload",
			why:  "the volume AND the mount both have to be refused; a check on volumes alone misses an injector that mounts an existing one",
			mutate: func(p *corev1.Pod) {
				p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{
					Name:         "docker-sock",
					VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/run/docker.sock"}},
				})
				for i := range p.Spec.Containers {
					if p.Spec.Containers[i].Name == containerWorkload {
						p.Spec.Containers[i].VolumeMounts = append(p.Spec.Containers[i].VolumeMounts,
							corev1.VolumeMount{Name: "docker-sock", MountPath: "/var/run/docker.sock"})
					}
				}
			},
		},
		{
			name: "automountServiceAccountToken flipped to true",
			why:  "a projected service-account token is an ambient credential the in-sandbox env scrub cannot see or remove",
			mutate: func(p *corev1.Pod) {
				p.Spec.AutomountServiceAccountToken = ptr(true)
			},
		},
		{
			name: "automountServiceAccountToken cleared to nil",
			why:  "nil DEFAULTS TO TRUE at the API server; treating nil as false is how this check gets silently inverted",
			mutate: func(p *corev1.Pod) {
				p.Spec.AutomountServiceAccountToken = nil
			},
		},
		{
			name: "seccomp profile dropped",
			why:  "RuntimeDefault seccomp is the syscall floor; Unconfined is the cluster quietly widening the kernel surface",
			mutate: func(p *corev1.Pod) {
				p.Spec.SecurityContext.SeccompProfile = nil
			},
		},
		{
			name: "seccomp profile set to Unconfined",
			why:  "an explicit Unconfined is a present-but-wrong profile, which a nil check alone would pass",
			mutate: func(p *corev1.Pod) {
				p.Spec.SecurityContext.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}
			},
		},
		{
			name: "projected service-account token volume added",
			why:  "the injector route AROUND automountServiceAccountToken:false — the flag stays false and the token arrives anyway",
			mutate: func(p *corev1.Pod) {
				p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{
					Name: "kube-api-access",
					VolumeSource: corev1.VolumeSource{
						Projected: &corev1.ProjectedVolumeSource{
							Sources: []corev1.VolumeProjection{{
								ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token"},
							}},
						},
					},
				})
			},
		},
		{
			name:   "hostNetwork enabled",
			why:    "hostNetwork puts the sandbox on the node's network namespace, where NO NetworkPolicy applies — the metadata floor is simply gone",
			mutate: func(p *corev1.Pod) { p.Spec.HostNetwork = true },
		},
		{
			name:   "hostPID enabled",
			why:    "hostPID exposes every process on the node, fuse's own included",
			mutate: func(p *corev1.Pod) { p.Spec.HostPID = true },
		},
		{
			name:   "hostIPC enabled",
			why:    "hostIPC shares the node's IPC namespace across tenants",
			mutate: func(p *corev1.Pod) { p.Spec.HostIPC = true },
		},
		{
			name: "privileged workload container",
			why:  "a privileged container is root on the node",
			mutate: func(p *corev1.Pod) {
				for i := range p.Spec.Containers {
					if p.Spec.Containers[i].Name == containerWorkload {
						p.Spec.Containers[i].SecurityContext.Privileged = ptr(true)
					}
				}
			},
		},
		{
			name: "allowPrivilegeEscalation enabled",
			why:  "setuid binaries in the image become an escalation path",
			mutate: func(p *corev1.Pod) {
				for i := range p.Spec.Containers {
					if p.Spec.Containers[i].Name == containerWorkload {
						p.Spec.Containers[i].SecurityContext.AllowPrivilegeEscalation = ptr(true)
					}
				}
			},
		},
		{
			name: "capabilities re-added",
			why:  "drop:[ALL] with an add: list is not drop:[ALL]",
			mutate: func(p *corev1.Pod) {
				for i := range p.Spec.Containers {
					if p.Spec.Containers[i].Name == containerWorkload {
						p.Spec.Containers[i].SecurityContext.Capabilities.Add = []corev1.Capability{"NET_ADMIN"}
					}
				}
			},
		},
		{
			name: "capability drop list emptied",
			why:  "the default capability set is what drop:[ALL] exists to remove",
			mutate: func(p *corev1.Pod) {
				for i := range p.Spec.Containers {
					if p.Spec.Containers[i].Name == containerWorkload {
						p.Spec.Containers[i].SecurityContext.Capabilities.Drop = nil
					}
				}
			},
		},
		{
			name:   "runAsNonRoot cleared",
			why:    "a root workload plus any container-escape primitive is node root",
			mutate: func(p *corev1.Pod) { p.Spec.SecurityContext.RunAsNonRoot = nil },
		},
		{
			name: "workload image substituted",
			why:  "the image is the trusted side's choice; an injector swapping it runs code fuse never approved with fuse's posture",
			mutate: func(p *corev1.Pod) {
				for i := range p.Spec.Containers {
					if p.Spec.Containers[i].Name == containerWorkload {
						p.Spec.Containers[i].Image = "attacker/evil:latest"
					}
				}
			},
		},
		{
			name: "environment injected into the workload",
			why:  "the container env must stay EMPTY; a baked variable survives every ResetEnv, so a rotated credential would linger in every later command",
			mutate: func(p *corev1.Pod) {
				for i := range p.Spec.Containers {
					if p.Spec.Containers[i].Name == containerWorkload {
						p.Spec.Containers[i].Env = []corev1.EnvVar{{Name: "AWS_SECRET_ACCESS_KEY", Value: "leaked"}}
					}
				}
			},
		},
		{
			name: "envFrom injected into the workload",
			why:  "envFrom is the same leak by another field, and a check on env alone misses it",
			mutate: func(p *corev1.Pod) {
				for i := range p.Spec.Containers {
					if p.Spec.Containers[i].Name == containerWorkload {
						p.Spec.Containers[i].EnvFrom = []corev1.EnvFromSource{{
							SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "cluster-secrets"}},
						}}
					}
				}
			},
		},
		{
			name:   "serviceAccountName substituted",
			why:    "gate 6: the sandbox must never run as an identity with RBAC, least of all fuse's own provisioning identity",
			mutate: func(p *corev1.Pod) { p.Spec.ServiceAccountName = "fuse" },
		},
		{
			name:   "activeDeadlineSeconds removed",
			why:    "the GC backstop is what ends a Pod when no fuse instance is alive to reap it",
			mutate: func(p *corev1.Pod) { p.Spec.ActiveDeadlineSeconds = nil },
		},
		{
			name:   "restartPolicy changed to Always",
			why:    "a restarting sandbox outlives the teardown that a timed-out command depends on",
			mutate: func(p *corev1.Pod) { p.Spec.RestartPolicy = corev1.RestartPolicyAlways },
		},
		{
			name: "workspace emptyDir replaced by a hostPath",
			why:  "the workspace is per-Pod and disposable; a node path makes it shared and persistent across tenants",
			mutate: func(p *corev1.Pod) {
				for i := range p.Spec.Volumes {
					if p.Spec.Volumes[i].Name == volumeWorkspace {
						p.Spec.Volumes[i].VolumeSource = corev1.VolumeSource{
							HostPath: &corev1.HostPathVolumeSource{Path: "/tmp/shared"},
						}
					}
				}
			},
		},
		{
			name: "workspace sizeLimit removed",
			why:  "an unbounded emptyDir fills the node's disk, which is a denial of service against every other tenant on it",
			mutate: func(p *corev1.Pod) {
				for i := range p.Spec.Volumes {
					if p.Spec.Volumes[i].Name == volumeWorkspace && p.Spec.Volumes[i].EmptyDir != nil {
						p.Spec.Volumes[i].EmptyDir.SizeLimit = nil
					}
				}
			},
		},
		{
			name: "extra container injected",
			why:  "an injected sidecar shares the Pod's network namespace and its loopback, which is where the egress forwarder listens",
			mutate: func(p *corev1.Pod) {
				p.Spec.Containers = append(p.Spec.Containers, corev1.Container{
					Name:  "istio-proxy",
					Image: "mesh/sidecar:1",
				})
				p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, corev1.ContainerStatus{Name: "istio-proxy", Ready: true})
			},
		},
		{
			name: "workload container renamed",
			why:  "Exec targets the container by name; a renamed one would make every Exec fail late instead of Provision fail loudly",
			mutate: func(p *corev1.Pod) {
				for i := range p.Spec.Containers {
					if p.Spec.Containers[i].Name == containerWorkload {
						p.Spec.Containers[i].Name = "main"
						p.Status.ContainerStatuses[i].Name = "main"
					}
				}
			},
		},
		{
			name:   "arch affinity stripped",
			why:    "an unpinned Pod can land on a node where the arch-specific sidecar entrypoint does not exist, i.e. with no egress datapath",
			mutate: func(p *corev1.Pod) { p.Spec.Affinity = nil },
		},
		{
			name: "workload workingDir moved out of the workspace",
			why:  "the working directory is the containment root every Exec's working_dir is resolved against",
			mutate: func(p *corev1.Pod) {
				for i := range p.Spec.Containers {
					if p.Spec.Containers[i].Name == containerWorkload {
						p.Spec.Containers[i].WorkingDir = "/"
					}
				}
			},
		},

		// --- the injection surfaces BESIDE Spec.Containers ---
		//
		// Every case below is an injector that adds nothing to Containers at all,
		// which is exactly why a check that iterates only Containers passes it.
		{
			name: "plain init container injected",
			why:  "the CANONICAL mutating-webhook shape (Istio, Linkerd, every NET_ADMIN iptables-setup injector) adds to initContainers, not containers; a plain one runs privileged code in this Pod — with access to the egress-tls client key and write access to the workspace emptyDir — and then EXITS, after which the Pod is Running with every container Ready and the read-back sees a clean containers list",
			mutate: func(p *corev1.Pod) {
				p.Spec.InitContainers = append(p.Spec.InitContainers, corev1.Container{
					Name:    "istio-init",
					Image:   "mesh/proxyv2:1",
					Command: []string{"istio-iptables"},
					SecurityContext: &corev1.SecurityContext{
						Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_ADMIN", "NET_RAW"}},
					},
					VolumeMounts: []corev1.VolumeMount{{Name: volumeWorkspace, MountPath: workspaceMount}},
				})
				p.Status.InitContainerStatuses = append(p.Status.InitContainerStatuses,
					// A completed init container: Ready is FALSE and Running is
					// nil, which is what a passing Pod actually looks like.
					corev1.ContainerStatus{Name: "istio-init", Ready: false,
						State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}})
			},
		},
		{
			name: "native sidecar injected (initContainers entry with restartPolicy Always)",
			why:  "a Kubernetes 1.29+ native sidecar is an initContainers entry that NEVER exits: it stays live for the Pod's whole life sharing the netns where the egress forwarder listens, and its status lands in InitContainerStatuses — a second list the readiness check must not ignore either",
			mutate: func(p *corev1.Pod) {
				always := corev1.ContainerRestartPolicyAlways
				p.Spec.InitContainers = append(p.Spec.InitContainers, corev1.Container{
					Name:          "istio-proxy",
					Image:         "mesh/proxyv2:1",
					RestartPolicy: &always,
				})
				p.Status.InitContainerStatuses = append(p.Status.InitContainerStatuses,
					corev1.ContainerStatus{Name: "istio-proxy", Ready: true,
						State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}})
			},
		},
		{
			name: "ephemeral container injected",
			why:  "an ephemeral container is a live debug container in the running Pod — the workload's namespaces, the workload's volumes — arriving through a subresource the spec comparison never looks at",
			mutate: func(p *corev1.Pod) {
				p.Spec.EphemeralContainers = append(p.Spec.EphemeralContainers, corev1.EphemeralContainer{
					EphemeralContainerCommon: corev1.EphemeralContainerCommon{
						Name:  "debugger",
						Image: "busybox:latest",
					},
					TargetContainerName: containerWorkload,
				})
				p.Status.EphemeralContainerStatuses = append(p.Status.EphemeralContainerStatuses,
					corev1.ContainerStatus{Name: "debugger", Ready: true})
			},
		},

		// --- the CONTAINER-level overrides of a POD-level floor ---
		//
		// Container securityContext TAKES PRECEDENCE over the pod's. So every
		// pod-level assertion above can be neutralised without touching the field
		// it asserts, by setting the same field one level down — where neither
		// container declares it and nothing was comparing.
		{
			name: "container-level runAsUser 0 overriding the pod's non-root floor",
			why:  "container securityContext.runAsUser takes PRECEDENCE over the pod's; root in the workload plus any container-escape primitive is node root, and the pod-level runAsUser the read-back checks is still intact",
			mutate: func(p *corev1.Pod) {
				for i := range p.Spec.Containers {
					if p.Spec.Containers[i].Name == containerWorkload {
						p.Spec.Containers[i].SecurityContext.RunAsUser = ptr(int64(0))
					}
				}
			},
		},
		{
			name: "container-level runAsNonRoot cleared to false",
			why:  "the same override by the adjacent field: runAsNonRoot:false at the container level lets an image's root USER through while the pod-level true is untouched",
			mutate: func(p *corev1.Pod) {
				for i := range p.Spec.Containers {
					if p.Spec.Containers[i].Name == containerWorkload {
						p.Spec.Containers[i].SecurityContext.RunAsNonRoot = ptr(false)
					}
				}
			},
		},
		{
			name: "container-level seccompProfile set to Unconfined",
			why:  "container seccompProfile overrides the pod's RuntimeDefault, so the syscall floor the pod-level check defends can be removed one level down",
			mutate: func(p *corev1.Pod) {
				for i := range p.Spec.Containers {
					if p.Spec.Containers[i].Name == containerWorkload {
						p.Spec.Containers[i].SecurityContext.SeccompProfile = &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeUnconfined,
						}
					}
				}
			},
		},
		{
			name: "container procMount set to Unmasked",
			why:  "Unmasked /proc re-exposes the kernel paths the runtime masks (/proc/sys, /proc/kcore, /proc/sysrq-trigger) inside a container whose capabilities look correctly dropped",
			mutate: func(p *corev1.Pod) {
				unmasked := corev1.UnmaskedProcMount
				for i := range p.Spec.Containers {
					if p.Spec.Containers[i].Name == containerWorkload {
						p.Spec.Containers[i].SecurityContext.ProcMount = &unmasked
					}
				}
			},
		},

		// --- the pod-level flag that defeats the egress credential split ---
		{
			name:   "shareProcessNamespace enabled",
			why:    "a shared PID namespace lets the workload read the egress sidecar's filesystem through /proc/<pid>/root — including the egress-tls client certificate and key the read-back goes out of its way to keep out of the workload — so it defeats that separation without mounting anything",
			mutate: func(p *corev1.Pod) { p.Spec.ShareProcessNamespace = ptr(true) },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewClientset()
			s := newTestSubstrate(t, cs)
			admitWith(t, cs, s, tc.mutate)

			sb, err := s.Provision(context.Background(), loopPrincipal("acme"), sandbox.RemoteSpec{})
			if err == nil {
				t.Fatalf("Provision CONFIRMED a drifted Pod (%s).\nWhy this matters: %s", tc.name, tc.why)
			}
			if sb != nil {
				t.Errorf("Provision returned a sandbox alongside its error; an unconfirmable sandbox must never be handed out")
			}

			assertNoPodsLeftBehind(t, cs, tc.name)
		})
	}
}

// assertNoPodsLeftBehind is the second half of every refusal assertion. It looks
// across ALL namespaces rather than at the name the substrate chose, so a delete
// aimed at the wrong name cannot pass.
func assertNoPodsLeftBehind(t *testing.T, cs *fake.Clientset, what string) {
	t.Helper()
	pods, err := cs.CoreV1().Pods("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods.Items) != 0 {
		names := make([]string, 0, len(pods.Items))
		for _, p := range pods.Items {
			names = append(names, p.Namespace+"/"+p.Name)
		}
		t.Fatalf("after refusing %s, %d Pod(s) remain: %v.\nA refusal that forgets the delete is WORSE than no check: the operator gets an error while a wrongly-postured Pod keeps running and keeps counting against the quota.", what, len(pods.Items), names)
	}
}

// TestProvisionRefusesNeverReady — the startup deadline. A Pod that never reaches
// Running with every container Ready is torn down and refused, not waited on
// forever and not handed out optimistically.
func TestProvisionRefusesNeverReady(t *testing.T) {
	cs := fake.NewClientset()
	s := newTestSubstrate(t, cs, func(o *Options) {
		o.Config.Kubernetes.StartupTimeout = ptr(150 * time.Millisecond)
	})
	// Admitted, correct in every field, but stuck in Pending.
	admitWith(t, cs, s, func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodPending
		p.Status.ContainerStatuses = nil
	})

	if _, err := s.Provision(context.Background(), loopPrincipal("acme"), sandbox.RemoteSpec{}); err == nil {
		t.Fatal("Provision must refuse a Pod that never becomes Ready")
	}
	assertNoPodsLeftBehind(t, cs, "a Pod that never became Ready")
}

// TestProvisionRefusesContainerNotReady — Running with a container that is NOT
// Ready. Phase alone is not the readiness signal: a Pod is Running the moment one
// container starts, and an Exec into a not-yet-started container fails.
func TestProvisionRefusesContainerNotReady(t *testing.T) {
	cs := fake.NewClientset()
	s := newTestSubstrate(t, cs, func(o *Options) {
		o.Config.Kubernetes.StartupTimeout = ptr(150 * time.Millisecond)
	})
	admitWith(t, cs, s, func(p *corev1.Pod) {
		for i := range p.Status.ContainerStatuses {
			p.Status.ContainerStatuses[i].Ready = false
		}
	})

	if _, err := s.Provision(context.Background(), loopPrincipal("acme"), sandbox.RemoteSpec{}); err == nil {
		t.Fatal("Provision must refuse a Running Pod whose containers are not Ready")
	}
	assertNoPodsLeftBehind(t, cs, "a Running Pod with an unready container")
}

// TestAllContainersReadyCoversInitContainerStatuses is the readiness half of the
// native-sidecar gap.
//
// A Kubernetes 1.29+ native sidecar is an initContainers entry with
// restartPolicy:Always. It stays live for the Pod's whole life, and its status is
// reported in Status.InitContainerStatuses — NOT in ContainerStatuses. So a
// readiness check that walks only ContainerStatuses calls such a Pod ready while
// the sidecar is still pulling, still crash-looping, or not yet started.
//
// This is asserted on allContainersReady directly rather than through Provision
// because assertPosture now REFUSES any init container outright: routed through
// Provision, the posture fault would mask whatever the readiness check did, and
// the test would pass no matter how allContainersReady behaved.
func TestAllContainersReadyCoversInitContainerStatuses(t *testing.T) {
	// A minimal two-container Pod matching the substrate's shape, both Ready. The
	// init list is what each case varies.
	base := func() *corev1.Pod {
		return &corev1.Pod{
			Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: containerWorkload}, {Name: containerEgress},
			}},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{
					{Name: containerWorkload, Ready: true}, {Name: containerEgress, Ready: true},
				},
			},
		}
	}
	always := corev1.ContainerRestartPolicyAlways

	tests := []struct {
		name  string
		build func(*corev1.Pod)
		want  bool
		why   string
	}{
		{
			name:  "no init containers at all",
			build: func(*corev1.Pod) {},
			want:  true,
			why:   "the ordinary sandbox Pod: two containers, both Ready, nothing else",
		},
		{
			name: "native sidecar Ready",
			build: func(p *corev1.Pod) {
				p.Spec.InitContainers = []corev1.Container{{Name: "sidecar", RestartPolicy: &always}}
				p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "sidecar", Ready: true}}
			},
			want: true,
			why:  "a live sidecar that IS ready must not be reported unready; that would hang every Provision to the startup deadline",
		},
		{
			name: "native sidecar NOT Ready",
			build: func(p *corev1.Pod) {
				p.Spec.InitContainers = []corev1.Container{{Name: "sidecar", RestartPolicy: &always}}
				p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "sidecar", Ready: false}}
			},
			want: false,
			why:  "a restartPolicy:Always init container is a LIVE container for the Pod's whole life; if it is not Ready the Pod is not ready, and its status is only ever in InitContainerStatuses",
		},
		{
			name: "native sidecar with no status reported yet",
			build: func(p *corev1.Pod) {
				p.Spec.InitContainers = []corev1.Container{{Name: "sidecar", RestartPolicy: &always}}
			},
			want: false,
			why:  "a missing status is not a ready one; the same reason the ContainerStatuses length is compared against the spec",
		},
		{
			name: "completed plain init container",
			build: func(p *corev1.Pod) {
				p.Spec.InitContainers = []corev1.Container{{Name: "setup"}}
				p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "setup", Ready: false,
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}}}
			},
			want: true,
			why:  "a PLAIN init container is expected to exit with Ready:false; demanding readiness of it would never be satisfiable, so only restartPolicy:Always entries are held to it",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pod := base()
			tc.build(pod)
			if got := allContainersReady(pod); got != tc.want {
				t.Fatalf("allContainersReady = %v, want %v.\nWhy this matters: %s", got, tc.want, tc.why)
			}
		})
	}
}

// TestProvisionRefusesFailedPod — a Pod that reaches a terminal phase is refused
// immediately rather than polled until the deadline.
func TestProvisionRefusesFailedPod(t *testing.T) {
	for _, phase := range []corev1.PodPhase{corev1.PodFailed, corev1.PodSucceeded} {
		t.Run(string(phase), func(t *testing.T) {
			cs := fake.NewClientset()
			s := newTestSubstrate(t, cs, func(o *Options) {
				// Long enough that a poll-until-deadline implementation would be
				// visibly slow, and short enough not to hang the suite.
				o.Config.Kubernetes.StartupTimeout = ptr(10 * time.Second)
			})
			admitWith(t, cs, s, func(p *corev1.Pod) {
				p.Status.Phase = phase
				p.Status.ContainerStatuses = nil
			})

			start := time.Now()
			if _, err := s.Provision(context.Background(), loopPrincipal("acme"), sandbox.RemoteSpec{}); err == nil {
				t.Fatalf("Provision must refuse a %s Pod", phase)
			}
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Errorf("Provision took %v on a terminal %s Pod; a terminal phase is decided, not waited on", elapsed, phase)
			}
			assertNoPodsLeftBehind(t, cs, "a "+string(phase)+" Pod")
		})
	}
}

// TestProvisionCreateFailureDeletesByName is the half-created-Pod path.
//
// A Create that fails ON THE WIRE — a timeout, a reset connection — is
// AMBIGUOUS: the API server may have accepted the object before the response was
// lost. So the failure is followed by a Get-then-Delete against the
// DETERMINISTIC name, which is the whole reason the name is derived rather than
// minted server-side: a name fuse could not predict is a Pod fuse can only
// forget.
func TestProvisionCreateFailureDeletesByName(t *testing.T) {
	cs := fake.NewClientset()
	s := newTestSubstrate(t, cs)

	// The Pod IS created, and then the response is lost.
	cs.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if err := cs.Tracker().Add(readyPod(s, pod)); err != nil {
			return true, nil, err
		}
		return true, nil, fmt.Errorf("the connection was reset")
	})

	if _, err := s.Provision(context.Background(), loopPrincipal("acme"), sandbox.RemoteSpec{}); err == nil {
		t.Fatal("Provision must fail when Create fails")
	}
	assertNoPodsLeftBehind(t, cs, "a Create that failed on the wire after the object landed")
}

// TestProvisionCreateFailureCleanNoPod — the other half of the ambiguity: the
// Create genuinely did not land. The cleanup Get reports NotFound, which is a
// success (there is nothing to delete) and must not be reported as a second
// failure that masks the real one.
func TestProvisionCreateFailureCleanNoPod(t *testing.T) {
	cs := fake.NewClientset()
	s := newTestSubstrate(t, cs)
	cs.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewTimeoutError("create", 1)
	})

	if _, err := s.Provision(context.Background(), loopPrincipal("acme"), sandbox.RemoteSpec{}); err == nil {
		t.Fatal("Provision must fail when Create fails")
	}
	assertNoPodsLeftBehind(t, cs, "a Create that never landed")
}

// TestTeardownDeletesPod — Teardown is idempotent and treats "already gone" as
// success, because Release runs on every early-return path and from defers.
func TestTeardownDeletesPod(t *testing.T) {
	cs := fake.NewClientset()
	s := newTestSubstrate(t, cs)
	admitWith(t, cs, s, nil)

	sb, err := s.Provision(context.Background(), loopPrincipal("acme"), sandbox.RemoteSpec{})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if err := sb.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	assertNoPodsLeftBehind(t, cs, "Teardown")

	if err := sb.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown must be idempotent and treat an already-gone sandbox as success, got: %v", err)
	}
}

// TestTeardownNeverDeletesNamespace — fuse creates namespaces and never deletes
// them. A namespace delete cascades to every object in it, so one tenant's
// teardown racing another Provision in the same namespace would take that
// Provision's Pod with it.
func TestTeardownNeverDeletesNamespace(t *testing.T) {
	cs := fake.NewClientset()
	s := newTestSubstrate(t, cs)
	admitWith(t, cs, s, nil)

	sb, err := s.Provision(context.Background(), loopPrincipal("acme"), sandbox.RemoteSpec{})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if err := sb.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown: %v", err)
	}

	ns, _, _ := strings.Cut(sb.ID(), "/")
	if _, err := cs.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{}); err != nil {
		t.Fatalf("Teardown deleted the tenant namespace %s: %v — a namespace delete cascades and would take a concurrent Provision's Pod with it", ns, err)
	}
}

// TestHeartbeatPatchesAnnotation — the heartbeat is what keeps another
// instance's Reap from collecting a live sandbox.
func TestHeartbeatPatchesAnnotation(t *testing.T) {
	cs := fake.NewClientset()
	s := newTestSubstrate(t, cs)
	admitWith(t, cs, s, nil)

	sb, err := s.Provision(context.Background(), loopPrincipal("acme"), sandbox.RemoteSpec{})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	ns, name, _ := strings.Cut(sb.ID(), "/")

	before, err := cs.CoreV1().Pods(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	stale := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	before.Annotations[annotationHeartbeat] = stale
	if _, err := cs.CoreV1().Pods(ns).Update(context.Background(), before, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update pod: %v", err)
	}

	if err := sb.Heartbeat(context.Background()); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}

	after, err := cs.CoreV1().Pods(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	got := after.Annotations[annotationHeartbeat]
	if got == stale {
		t.Fatal("Heartbeat did not advance fuse.dev/heartbeat; another instance's Reap would collect this live sandbox")
	}
	if _, err := time.Parse(time.RFC3339, got); err != nil {
		t.Fatalf("heartbeat annotation %q is not RFC 3339: %v — Reap parses it, and an unparseable value must not silently mean 'fresh'", got, err)
	}
}

// TestReapSelectsOnHeartbeatAgeAcrossInstances is the orphan collector's whole
// contract.
//
// It selects on fuse.dev/managed + heartbeat AGE and deliberately IGNORES
// fuse.dev/instance: an orphan is precisely a Pod whose owning instance is gone,
// so a reaper that only collected its own label could never collect one. The
// fresh Pod of the OTHER instance must survive, and the stale Pod of THIS
// instance must not be spared.
func TestReapSelectsOnHeartbeatAgeAcrossInstances(t *testing.T) {
	now := time.Now().UTC()
	fresh := now.Add(-time.Minute).Format(time.RFC3339)
	stale := now.Add(-2 * time.Hour).Format(time.RFC3339)

	cs := fake.NewClientset()
	s := newTestSubstrate(t, cs)

	type spec struct {
		name      string
		ns        string
		instance  string
		heartbeat string
		managed   bool
		wantGone  bool
	}
	specs := []spec{
		{name: "stale-other-instance", ns: "fuse-sb-a-1", instance: "instance-b", heartbeat: stale, managed: true, wantGone: true},
		{name: "stale-own-instance", ns: "fuse-sb-a-1", instance: "instance-a", heartbeat: stale, managed: true, wantGone: true},
		{name: "fresh-other-instance", ns: "fuse-sb-a-1", instance: "instance-b", heartbeat: fresh, managed: true},
		{name: "fresh-own-instance", ns: "fuse-sb-b-2", instance: "instance-a", heartbeat: fresh, managed: true},
		{name: "stale-in-another-namespace", ns: "fuse-sb-b-2", instance: "instance-c", heartbeat: stale, managed: true, wantGone: true},
		// NOT fuse's. An operator's own Pod in a namespace fuse created must
		// never be collected, however old it looks.
		{name: "not-managed", ns: "fuse-sb-a-1", instance: "instance-b", heartbeat: stale, managed: false},
		// Managed but with NO heartbeat annotation at all. That is fuse's own
		// object in an unknown state; it is collected, because the alternative is
		// an orphan no reaper can ever see.
		{name: "managed-no-heartbeat", ns: "fuse-sb-a-1", heartbeat: "", managed: true, wantGone: true},
		// An unparseable heartbeat must not read as "fresh": that would be a
		// permanent orphan created by one bad annotation write.
		{name: "managed-bad-heartbeat", ns: "fuse-sb-a-1", heartbeat: "not-a-timestamp", managed: true, wantGone: true},
		// Stale and labelled managed, but in a namespace fuse does not manage.
		// The namespace scope is the OUTER guard and must hold on its own.
		{name: "stale-outside-managed-namespaces", ns: "someone-elses-ns", heartbeat: stale, managed: true},
	}

	// The namespaces themselves must carry fuse.dev/managed: Reap enumerates
	// MANAGED namespaces, not every namespace in the cluster, so that one
	// label-selector slip cannot sweep an operator's own workloads. A test that
	// seeded only Pods would pass against a reaper that listed everything.
	for _, ns := range []string{"fuse-sb-a-1", "fuse-sb-b-2"} {
		if _, err := cs.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: map[string]string{labelManaged: "true"}},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed namespace %s: %v", ns, err)
		}
	}
	// An UNMANAGED namespace whose Pod is labelled as if it were fuse's. Reap
	// must not reach into it at all: the namespace scope is the outer guard.
	if _, err := cs.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "someone-elses-ns"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed unmanaged namespace: %v", err)
	}

	for _, sp := range specs {
		labels := map[string]string{labelInstance: sp.instance}
		if sp.managed {
			labels[labelManaged] = "true"
		}
		ann := map[string]string{}
		if sp.heartbeat != "" {
			ann[annotationHeartbeat] = sp.heartbeat
		}
		if _, err := cs.CoreV1().Pods(sp.ns).Create(context.Background(), &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: sp.name, Namespace: sp.ns, Labels: labels, Annotations: ann},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed %s: %v", sp.name, err)
		}
	}

	n, err := s.Reap(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}

	wantGone := 0
	for _, sp := range specs {
		if sp.wantGone {
			wantGone++
		}
		_, err := cs.CoreV1().Pods(sp.ns).Get(context.Background(), sp.name, metav1.GetOptions{})
		gone := apierrors.IsNotFound(err)
		if gone != sp.wantGone {
			t.Errorf("%s/%s: gone=%v, want gone=%v (instance=%q managed=%v heartbeat=%q)", sp.ns, sp.name, gone, sp.wantGone, sp.instance, sp.managed, sp.heartbeat)
		}
	}
	if n != wantGone {
		t.Errorf("Reap reported %d, want %d", n, wantGone)
	}
}

// TestReapToleratesAlreadyDeleted — N instances reap concurrently, so losing the
// race to delete an orphan is a SUCCESS. Reporting it as an error would make
// every multi-instance deployment log reap failures forever.
func TestReapToleratesAlreadyDeleted(t *testing.T) {
	cs := fake.NewClientset()
	s := newTestSubstrate(t, cs)

	if _, err := cs.CoreV1().Pods("fuse-sb-a-1").Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "orphan",
			Namespace:   "fuse-sb-a-1",
			Labels:      map[string]string{labelManaged: "true"},
			Annotations: map[string]string{annotationHeartbeat: time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)},
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cs.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(corev1.Resource("pods").WithVersion("v1").GroupResource(), "orphan")
	})

	if _, err := s.Reap(context.Background(), time.Hour); err != nil {
		t.Fatalf("Reap must treat an already-deleted orphan as success (N instances reap concurrently), got: %v", err)
	}
}

// THE REAPER MUST COLLECT THE PER-POD NETWORKPOLICY, NOT JUST THE POD.
//
// teardownEgress deletes `fuse-egress-<pod>` because a NetworkPolicy cannot be
// owned by the Pod it selects — there is no ownerReference backstop for it, unlike
// the Secret. The reaper is the path that runs precisely when no Teardown ever
// will (the owning instance died), so a reaper that deletes only the Pod leaves one
// policy behind per dead instance, without bound, in every tenant namespace.
//
// Not a containment hole — a policy selecting no Pod grants nothing — but an
// unbounded leak in the one code path whose whole purpose is to bound leaks.
//
// A FRESH Pod's policy must survive: the reaper's scope is orphans.
func TestReapCollectsThePerPodNetworkPolicy(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	cs := fake.NewClientset()
	s := newTestSubstrate(t, cs)

	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "fuse-sb-a-1", Labels: map[string]string{labelManaged: "true"}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed namespace: %v", err)
	}

	for _, pod := range []struct {
		name      string
		heartbeat time.Time
	}{
		{"orphan", now.Add(-2 * time.Hour)},
		{"live", now.Add(-time.Minute)},
	} {
		if _, err := cs.CoreV1().Pods("fuse-sb-a-1").Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:        pod.name,
				Namespace:   "fuse-sb-a-1",
				Labels:      map[string]string{labelManaged: "true", labelPod: pod.name},
				Annotations: map[string]string{annotationHeartbeat: pod.heartbeat.Format(time.RFC3339)},
			},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed pod %s: %v", pod.name, err)
		}
		if _, err := cs.NetworkingV1().NetworkPolicies("fuse-sb-a-1").Create(ctx,
			s.renderEgressPolicy("fuse-sb-a-1", pod.name), metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed policy for %s: %v", pod.name, err)
		}
	}

	if _, err := s.Reap(ctx, time.Hour); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	_, err := cs.NetworkingV1().NetworkPolicies("fuse-sb-a-1").Get(ctx, egressPolicyName("orphan"), metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("%s survived the reap (err=%v) — the policy has no ownerReference, so nothing else will ever collect it",
			egressPolicyName("orphan"), err)
	}
	if _, err := cs.NetworkingV1().NetworkPolicies("fuse-sb-a-1").Get(ctx, egressPolicyName("live"), metav1.GetOptions{}); err != nil {
		t.Errorf("%s must survive: its Pod is alive and still needs its egress allow (%v)", egressPolicyName("live"), err)
	}
}

// A MISSING policy is the ORDINARY case, not a failure: under allow-all the
// per-Pod policy still exists, but a Pod half-created before an instance died may
// have none, and another instance's reaper may have collected it first. Neither
// may make Reap report an error or stop counting the Pod as collected.
func TestReapToleratesMissingNetworkPolicy(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewClientset()
	s := newTestSubstrate(t, cs)

	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "fuse-sb-a-1", Labels: map[string]string{labelManaged: "true"}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed namespace: %v", err)
	}
	if _, err := cs.CoreV1().Pods("fuse-sb-a-1").Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "orphan",
			Namespace:   "fuse-sb-a-1",
			Labels:      map[string]string{labelManaged: "true"},
			Annotations: map[string]string{annotationHeartbeat: time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)},
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed pod: %v", err)
	}

	n, err := s.Reap(ctx, time.Hour)
	if err != nil {
		t.Fatalf("Reap must tolerate a NotFound policy: %v", err)
	}
	if n != 1 {
		t.Errorf("Reap reported %d, want 1 — the Pod was collected regardless of the policy", n)
	}
}

// --- small accessors, kept here so the assertions above read as assertions ---

func containerByName(t *testing.T, pod *corev1.Pod, name string) corev1.Container {
	t.Helper()
	for _, c := range pod.Spec.Containers {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no container %q in %v", name, containerNames(pod))
	return corev1.Container{}
}

func containerNames(pod *corev1.Pod) []string {
	out := make([]string, 0, len(pod.Spec.Containers))
	for _, c := range pod.Spec.Containers {
		out = append(out, c.Name)
	}
	return out
}

func volumeByName(t *testing.T, pod *corev1.Pod, name string) corev1.Volume {
	t.Helper()
	for _, v := range pod.Spec.Volumes {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("no volume %q", name)
	return corev1.Volume{}
}

func mountByName(t *testing.T, c corev1.Container, name string) corev1.VolumeMount {
	t.Helper()
	for _, m := range c.VolumeMounts {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("container %q has no volumeMount %q", c.Name, name)
	return corev1.VolumeMount{}
}

func affinityArches(t *testing.T, pod *corev1.Pod) []string {
	t.Helper()
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil ||
		pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Fatalf("pod has no REQUIRED node affinity; a preferred one is a suggestion, and the arch pin is a requirement")
	}
	var out []string
	for _, term := range pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Key == corev1.LabelArchStable {
				out = append(out, expr.Values...)
			}
		}
	}
	return out
}

// TestPodDeclaresAnExplicitNonRootUID is the second REGRESSION the kind lane
// (task 11) found against a real cluster, and the more serious of the two: it made
// EVERY sandbox Pod unstartable, not only the canary.
//
// `runAsNonRoot: true` with no `runAsUser` does not mean "run as some non-root
// user". It means "REFUSE unless the IMAGE declares a non-root USER", and the
// kubelet enforces it at container-create time:
//
//	Error: container has runAsNonRoot and image will run as root
//	(container: workload)
//
// Neither the substrate's pinned default (alpine:3.20) nor busybox declares a
// USER, so the demanded posture was unsatisfiable by the very images this
// substrate ships against. The Pod reached Scheduled and then sat in
// CreateContainerConfigError until startup_timeout, and the read-back — which
// asserts `runAsNonRoot` is true and was — reported nothing wrong, because nothing
// about the SPEC was wrong.
//
// The fake clientset runs no kubelet, so no unit test in this package could see it.
//
// The fix is an explicit uid/gid, which makes the demand satisfiable with ANY
// image while keeping the property runAsNonRoot was there for: the workload is not
// uid 0, so a container-escape primitive does not land on node root. fsGroup is
// what makes the emptyDir workspace writable by that uid — without it the Pod
// starts and every command fails on a read-only /workspace, which is the same
// defect one layer down.
func TestPodDeclaresAnExplicitNonRootUID(t *testing.T) {
	s := newTestSubstrate(t, fake.NewClientset())
	pod := s.renderPod("ns", "sb-x", loopPrincipal("acme"), time.Now().UTC())

	sc := pod.Spec.SecurityContext
	if sc == nil {
		t.Fatal("pod securityContext is nil")
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Fatal("runAsNonRoot must stay true")
	}
	if sc.RunAsUser == nil {
		t.Fatal("runAsUser is nil — `runAsNonRoot: true` alone means \"refuse unless the IMAGE declares a non-root " +
			"USER\", and neither alpine nor busybox does, so every Pod sits in CreateContainerConfigError until " +
			"startup_timeout while the spec read-back reports nothing wrong")
	}
	if *sc.RunAsUser == 0 {
		t.Fatalf("runAsUser = 0, which contradicts runAsNonRoot")
	}
	if sc.RunAsGroup == nil || *sc.RunAsGroup == 0 {
		t.Errorf("runAsGroup = %v, want an explicit non-zero gid", sc.RunAsGroup)
	}
	// fsGroup is what makes the emptyDir workspace writable by the uid above.
	// Without it the Pod starts and every command fails writing to /workspace,
	// which is the same defect one layer down.
	if sc.FSGroup == nil || *sc.FSGroup == 0 {
		t.Errorf("fsGroup = %v, want an explicit non-zero gid so the emptyDir workspace is writable by runAsUser",
			sc.FSGroup)
	}
}
