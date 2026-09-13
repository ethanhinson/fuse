package kubernetes

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// enforcing turns the test substrate's posture into `enforce` with the advertise
// address and listen port set, which is the only configuration under which the
// sidecar and the TLS Secret exist at all.
func enforcing(advertise string) func(*Options) {
	return func(o *Options) {
		o.Config.Egress = sandbox.Egress{Mode: sandbox.EgressEnforce}
		o.Config.Kubernetes.ProxyListen = ptr("0.0.0.0:3129")
		o.Config.Kubernetes.ProxyAdvertiseAddress = ptr(advertise)
		o.ProxyCredentials = staticCredentials{}
	}
}

// staticCredentials is a minter that hands back fixed PEM bytes. The real one is
// the Proxy's Enroll; this package must not import the proxy's internals, and what
// these tests assert is the POD's shape given a credential, never the credential's
// cryptography (internal/tools/sandbox's own tests cover that).
type staticCredentials struct{}

func (staticCredentials) EnrollSandbox(sandbox.PrincipalKey) (sandbox.SandboxCredential, error) {
	return sandbox.SandboxCredential{
		CertPEM: []byte("client-cert"),
		KeyPEM:  []byte("client-key"),
		CAPEM:   []byte("ca-cert"),
	}, nil
}

func (staticCredentials) ReleaseSandbox(sandbox.PrincipalKey) {}

// THE ADVERTISE ADDRESS IS A CONSTRUCTION-TIME REFUSAL UNDER `enforce`.
//
// It must name the OWNING instance, because both the policy and the #52
// delegated credentials live in that instance's memory. A substrate that built
// anyway would render a sidecar with no upstream to dial — which presents inside
// the sandbox as every network call hanging, not as a configuration error — and a
// per-Pod NetworkPolicy whose allowed ipBlock is nothing at all.
//
// Both places must be unset for the refusal: the explicit knob and $FUSE_POD_IP.
func TestNewSubstrateRefusesUnsetAdvertiseAddressUnderEnforce(t *testing.T) {
	t.Setenv(podIPEnv, "")

	_, err := newSubstrate(fake.NewClientset(), Options{
		Config: sandbox.Config{
			Egress:     sandbox.Egress{Mode: sandbox.EgressEnforce},
			Kubernetes: sandbox.Kubernetes{ProxyListen: ptr("0.0.0.0:3129")},
		},
		ProxyCredentials: staticCredentials{},
	})
	if err == nil {
		t.Fatal("newSubstrate must refuse when the advertise address is unset under enforce")
	}
	// The diagnostic must name BOTH places an operator can set it, or the error
	// is unactionable in a cluster where the chart has not been updated.
	for _, want := range []string{"advertise_address", podIPEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must name %q", err, want)
		}
	}
}

// $FUSE_POD_IP is the DEFAULT, not a fallback the explicit knob overrides only
// sometimes: the downward API is how the hosted chart supplies it.
func TestNewSubstrateTakesAdvertiseAddressFromPodIPEnv(t *testing.T) {
	t.Setenv(podIPEnv, "10.1.2.3")

	s, err := newSubstrate(fake.NewClientset(), Options{
		Config: sandbox.Config{
			Egress:     sandbox.Egress{Mode: sandbox.EgressEnforce},
			Kubernetes: sandbox.Kubernetes{ProxyListen: ptr("0.0.0.0:3129")},
		},
		ProxyCredentials: staticCredentials{},
	})
	if err != nil {
		t.Fatalf("newSubstrate: %v", err)
	}
	if s.advertiseAddress != "10.1.2.3" {
		t.Fatalf("advertiseAddress = %q, want %q from $%s", s.advertiseAddress, "10.1.2.3", podIPEnv)
	}

	// The explicit knob WINS over the environment: an operator who wrote it down
	// meant it.
	s2, err := newSubstrate(fake.NewClientset(), Options{
		Config: sandbox.Config{
			Egress: sandbox.Egress{Mode: sandbox.EgressEnforce},
			Kubernetes: sandbox.Kubernetes{
				ProxyListen:           ptr("0.0.0.0:3129"),
				ProxyAdvertiseAddress: ptr("10.9.9.9"),
			},
		},
		ProxyCredentials: staticCredentials{},
	})
	if err != nil {
		t.Fatalf("newSubstrate: %v", err)
	}
	if s2.advertiseAddress != "10.9.9.9" {
		t.Fatalf("advertiseAddress = %q, want the explicit %q", s2.advertiseAddress, "10.9.9.9")
	}
}

// Under `allow-all` the advertise address is IRRELEVANT — there is no sidecar and
// no TLS listener to reach — so its absence must not refuse. The metadata floor
// in that posture is the NetworkPolicy's `except` list, not the proxy.
func TestNewSubstrateDoesNotRequireAdvertiseAddressUnderAllowAll(t *testing.T) {
	t.Setenv(podIPEnv, "")
	if _, err := newSubstrate(fake.NewClientset(), Options{
		Config: sandbox.Config{Egress: sandbox.Egress{Mode: sandbox.EgressAllowAll}},
	}); err != nil {
		t.Fatalf("newSubstrate under allow-all must not require an advertise address: %v", err)
	}
}

// Under `enforce` a substrate with NO credential minter refuses: the sidecar
// cannot be given a client certificate, so its TLS connection could never be
// served, and the proxy would close it on an unknown serial. That is a
// configuration error and must be reported as one at construction.
func TestNewSubstrateRefusesEnforceWithoutCredentialMinter(t *testing.T) {
	t.Setenv(podIPEnv, "10.1.2.3")
	if _, err := newSubstrate(fake.NewClientset(), Options{
		Config: sandbox.Config{
			Egress:     sandbox.Egress{Mode: sandbox.EgressEnforce},
			Kubernetes: sandbox.Kubernetes{ProxyListen: ptr("0.0.0.0:3129")},
		},
	}); err == nil {
		t.Fatal("newSubstrate must refuse enforce with no credential minter")
	}
}

// THE egress-tls SECRET MOUNTS INTO THE SIDECAR ONLY.
//
// This is the assertion the spec's rendering table and assertPosture both exist
// for. A workload that can read the sidecar's client certificate can open its own
// TLS connection to the proxy AS the sidecar, and the loopback hop — which is
// where the *_PROXY variables point and therefore where every policy decision
// begins — is skipped entirely. The credential is the identity; a readable
// credential is a bypass.
func TestRenderPodMountsEgressSecretIntoTheSidecarOnly(t *testing.T) {
	s := newTestSubstrate(t, fake.NewClientset(), enforcing("10.1.2.3"))

	pod, _, err := s.renderPodWithEgress("ns", "sb-x-1", loopPrincipal("t"), time.Unix(1700000000, 0).UTC())
	if err != nil {
		t.Fatalf("renderPodWithEgress: %v", err)
	}

	workload, ok := containerNamed(pod, containerWorkload)
	if !ok {
		t.Fatal("no workload container")
	}
	for _, m := range workload.VolumeMounts {
		if m.Name == volumeEgressTLS {
			t.Fatalf("the %q Secret is mounted into %q at %q; it belongs to the sidecar alone", volumeEgressTLS, containerWorkload, m.MountPath)
		}
	}

	side, ok := containerNamed(pod, containerEgress)
	if !ok {
		t.Fatalf("no %q sidecar container under enforce", containerEgress)
	}
	var mount *corev1.VolumeMount
	for i := range side.VolumeMounts {
		if side.VolumeMounts[i].Name == volumeEgressTLS {
			mount = &side.VolumeMounts[i]
		}
	}
	if mount == nil {
		t.Fatalf("the %q Secret is not mounted into the %q sidecar", volumeEgressTLS, containerEgress)
	}
	if !mount.ReadOnly {
		t.Errorf("the %q mount is writable; it must be read-only", volumeEgressTLS)
	}
	if mount.MountPath != egressTLSMount {
		t.Errorf("mountPath = %q, want %q", mount.MountPath, egressTLSMount)
	}

	// The sidecar must ALSO not be able to see the workspace: it relays bytes and
	// has no business in the command's filesystem.
	for _, m := range side.VolumeMounts {
		if m.Name == volumeWorkspace {
			t.Errorf("the sidecar mounts the workspace at %q; it relays bytes and needs no filesystem", m.MountPath)
		}
	}
}

// THE SIDECAR'S ARGV, per spec §3. It is golden-asserted because every element is
// a decision: the listen address is the pod loopback the *_PROXY variables name,
// the upstream is the OWNING instance's advertise address and the proxy's listen
// port, and the three PEM paths are inside the read-only mount.
func TestRenderPodSidecarArgv(t *testing.T) {
	s := newTestSubstrate(t, fake.NewClientset(), enforcing("10.1.2.3"))

	pod, _, err := s.renderPodWithEgress("ns", "sb-x-1", loopPrincipal("t"), time.Unix(1700000000, 0).UTC())
	if err != nil {
		t.Fatalf("renderPodWithEgress: %v", err)
	}
	side, ok := containerNamed(pod, containerEgress)
	if !ok {
		t.Fatalf("no %q container", containerEgress)
	}

	want := []string{
		"/fuse-egress-forward-linux-arm64",
		"-listen", podEgressListen,
		"-upstream", "tls://10.1.2.3:3129",
		"-tls-cert", egressTLSMount + "/tls.crt",
		"-tls-key", egressTLSMount + "/tls.key",
		"-tls-ca", egressTLSMount + "/ca.crt",
	}
	if strings.Join(side.Command, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("sidecar command =\n  %v\nwant\n  %v", side.Command, want)
	}

	// The image defaults to fuse's own published image at this binary's version.
	if !strings.HasPrefix(side.Image, "ghcr.io/ethanhinson/fuse:") {
		t.Errorf("sidecar image = %q, want fuse's own published image", side.Image)
	}

	// SAME HARDENED securityContext as the workload. The sidecar shares the Pod's
	// network namespace, so a privileged sidecar is a privileged Pod.
	sc := side.SecurityContext
	switch {
	case sc == nil:
		t.Error("the sidecar has no securityContext")
	case sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation:
		t.Error("the sidecar allows privilege escalation")
	case sc.Privileged != nil && *sc.Privileged:
		t.Error("the sidecar is privileged")
	case sc.Capabilities == nil || !dropsAll(sc.Capabilities.Drop):
		t.Error("the sidecar does not drop ALL capabilities")
	}

	// The sidecar's env is EMPTY for the same reason the workload's is: it takes
	// everything it needs from argv and a mounted file, so there is nothing a
	// baked value could be but a leak.
	if len(side.Env) != 0 || len(side.EnvFrom) != 0 {
		t.Errorf("the sidecar carries env %v / envFrom; it needs none", side.Env)
	}
}

// Under `allow-all` there is NO sidecar and NO Secret volume. The two postures
// must be structurally distinguishable, and this is the structural difference.
func TestRenderPodHasNoSidecarUnderAllowAll(t *testing.T) {
	s := newTestSubstrate(t, fake.NewClientset(), func(o *Options) {
		o.Config.Egress = sandbox.Egress{Mode: sandbox.EgressAllowAll}
	})

	pod, secret, err := s.renderPodWithEgress("ns", "sb-x-1", loopPrincipal("t"), time.Unix(1700000000, 0).UTC())
	if err != nil {
		t.Fatalf("renderPodWithEgress: %v", err)
	}
	if _, ok := containerNamed(pod, containerEgress); ok {
		t.Errorf("allow-all rendered a %q sidecar", containerEgress)
	}
	if _, ok := volumeNamed(pod, volumeEgressTLS); ok {
		t.Errorf("allow-all rendered a %q volume", volumeEgressTLS)
	}
	if secret != nil {
		t.Errorf("allow-all rendered a Secret (%s)", secret.Name)
	}
	if len(pod.Spec.Containers) != 1 {
		t.Errorf("containers = %d, want exactly the workload", len(pod.Spec.Containers))
	}
}

// Under `enforce` with an EMPTY allowlist — ADR-0053's salvaged posture — the
// sidecar and the TLS material are STILL provisioned. Deny-all is a decision the
// proxy makes and the operator can observe in a refusal hook; a missing sidecar
// is an absent datapath that presents as a hang. The two must stay observably
// distinct, so this posture renders exactly as any other enforce posture does.
func TestRenderPodProvisionsTheSidecarUnderEnforceWithAnEmptyAllowlist(t *testing.T) {
	s := newTestSubstrate(t, fake.NewClientset(), func(o *Options) {
		enforcing("10.1.2.3")(o)
		o.Config.Egress.Allow = nil
	})

	pod, secret, err := s.renderPodWithEgress("ns", "sb-x-1", loopPrincipal("t"), time.Unix(1700000000, 0).UTC())
	if err != nil {
		t.Fatalf("renderPodWithEgress: %v", err)
	}
	if _, ok := containerNamed(pod, containerEgress); !ok {
		t.Errorf("an empty allowlist under enforce dropped the %q sidecar; deny-all must be a PROXY decision, not an absent datapath", containerEgress)
	}
	if secret == nil {
		t.Error("an empty allowlist under enforce dropped the TLS Secret")
	}
}

// THE PER-POD EGRESS POLICY, GOLDEN, IN BOTH MODES — and in EACH of them the
// metadata CIDRs must be denied.
//
// This is ADR-0058 rule 4: the floor is never lower than the metadata deny, in
// EITHER posture. Under enforce the floor holds because the ONLY permitted
// destination is the proxy; under allow-all it holds because the one permitted
// ipBlock carves the metadata addresses out with `except`. A single test over
// both modes is deliberate — the property is about the pair, and a per-mode test
// can pass while the pair is broken.
func TestRenderEgressPolicyGoldenBothModes(t *testing.T) {
	const pod = "sb-abc123-01"

	t.Run("enforce", func(t *testing.T) {
		s := newTestSubstrate(t, fake.NewClientset(), enforcing("10.1.2.3"))
		pol := s.renderEgressPolicy("ns", pod)

		if pol.Name != egressPolicyName(pod) {
			t.Errorf("name = %q, want %q", pol.Name, egressPolicyName(pod))
		}
		if got := pol.Spec.PodSelector.MatchLabels[labelPod]; got != pod {
			t.Errorf("podSelector selects %q on %s, want %q", got, labelPod, pod)
		}
		if len(pol.Spec.PolicyTypes) != 1 || pol.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress {
			t.Errorf("policyTypes = %v, want [Egress] only (ingress stays with the namespace floor)", pol.Spec.PolicyTypes)
		}

		if len(pol.Spec.Egress) != 1 {
			t.Fatalf("egress rules = %d, want exactly 1 (the proxy, and nothing else)", len(pol.Spec.Egress))
		}
		rule := pol.Spec.Egress[0]
		if len(rule.To) != 1 || rule.To[0].IPBlock == nil {
			t.Fatalf("egress rule `to` = %+v, want exactly one ipBlock", rule.To)
		}
		if got := rule.To[0].IPBlock.CIDR; got != "10.1.2.3/32" {
			t.Errorf("ipBlock = %q, want the OWNING instance as a /32", got)
		}
		if len(rule.Ports) != 1 || rule.Ports[0].Port == nil || rule.Ports[0].Port.IntValue() != 3129 {
			t.Fatalf("ports = %+v, want exactly the proxy's listen port", rule.Ports)
		}

		// THE FLOOR: no metadata address is reachable, because the only reachable
		// address at all is the proxy's.
		assertMetadataDenied(t, pol)
	})

	t.Run("allow-all", func(t *testing.T) {
		s := newTestSubstrate(t, fake.NewClientset(), func(o *Options) {
			o.Config.Egress = sandbox.Egress{Mode: sandbox.EgressAllowAll}
		})
		pol := s.renderEgressPolicy("ns", pod)

		if len(pol.Spec.Egress) != 1 {
			t.Fatalf("egress rules = %d, want exactly 1", len(pol.Spec.Egress))
		}
		rule := pol.Spec.Egress[0]
		if len(rule.Ports) != 0 {
			t.Errorf("allow-all must not restrict ports, got %+v", rule.Ports)
		}

		blocks := map[string][]string{}
		for _, to := range rule.To {
			if to.IPBlock == nil {
				t.Fatalf("allow-all `to` entry %+v is not an ipBlock", to)
			}
			blocks[to.IPBlock.CIDR] = to.IPBlock.Except
		}
		v4, ok := blocks["0.0.0.0/0"]
		if !ok {
			t.Fatal("allow-all does not permit 0.0.0.0/0")
		}
		v6, ok := blocks["::/0"]
		if !ok {
			t.Fatal("allow-all does not permit ::/0")
		}

		// THE FLOOR: the metadata addresses are carved OUT of the otherwise
		// unrestricted allow. This is the whole reason allow-all is a policy and
		// not the absence of one.
		for _, want := range []string{"169.254.169.254/32", "169.254.170.2/32"} {
			if !contains(v4, want) {
				t.Errorf("0.0.0.0/0 except = %v, missing %q", v4, want)
			}
		}
		if !contains(v6, "fd00:ec2::254/128") {
			t.Errorf("::/0 except = %v, missing %q", v6, "fd00:ec2::254/128")
		}

		assertMetadataDenied(t, pol)
	})
}

// assertMetadataDenied is the floor assertion, applied to whichever policy is
// rendered: for every metadata address, either no rule reaches it at all or every
// rule that could reach it excludes it.
//
// It is written as a REACHABILITY question rather than a spelling check so it
// holds for both postures with one implementation — which is the point, since the
// claim being made is about the pair.
func assertMetadataDenied(t *testing.T, pol *networkingv1.NetworkPolicy) {
	t.Helper()
	for _, addr := range []string{"169.254.169.254", "169.254.170.2", "fd00:ec2::254"} {
		for i, rule := range pol.Spec.Egress {
			for j, to := range rule.To {
				if to.IPBlock == nil {
					continue
				}
				if ipBlockReaches(to.IPBlock, addr) {
					t.Errorf("egress rule %d/%d (%s except %v) reaches the metadata address %s; the metadata floor must hold in EVERY posture",
						i, j, to.IPBlock.CIDR, to.IPBlock.Except, addr)
				}
			}
		}
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// THE PER-POD POLICY AND THE SECRET ARE CREATED BY Provision, and the Secret
// carries an ownerReference to the Pod so Kubernetes garbage-collects it.
func TestProvisionCreatesTheEgressPolicyAndSecret(t *testing.T) {
	cs := fake.NewClientset()
	s := newTestSubstrate(t, cs, enforcing("10.1.2.3"))
	admitWith(t, cs, s, nil)

	sb, err := s.Provision(context.Background(), loopPrincipal("acme"), sandbox.RemoteSpec{})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	ns, name, ok := strings.Cut(sb.ID(), "/")
	if !ok {
		t.Fatalf("ID = %q, want <namespace>/<pod>", sb.ID())
	}

	pol, err := cs.NetworkingV1().NetworkPolicies(ns).Get(context.Background(), egressPolicyName(name), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the per-Pod egress policy was not created: %v", err)
	}
	if pol.Spec.PodSelector.MatchLabels[labelPod] != name {
		t.Errorf("the policy selects %q, want the Pod %q", pol.Spec.PodSelector.MatchLabels[labelPod], name)
	}

	sec, err := cs.CoreV1().Secrets(ns).Get(context.Background(), egressSecretName(name), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the egress-tls Secret was not created: %v", err)
	}
	// The ownerReference is what makes the Secret's life the POD's life. Without
	// it a reaped Pod leaves its client certificate behind in the namespace
	// forever — and a certificate whose serial the proxy has since revoked is
	// litter an operator cannot tell from a live one.
	if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].Name != name {
		t.Fatalf("Secret ownerReferences = %+v, want exactly one pointing at Pod %q", sec.OwnerReferences, name)
	}
	if sec.OwnerReferences[0].Kind != "Pod" {
		t.Errorf("ownerReference Kind = %q, want Pod", sec.OwnerReferences[0].Kind)
	}
	for _, key := range []string{"tls.crt", "tls.key", "ca.crt"} {
		if len(sec.Data[key]) == 0 {
			t.Errorf("Secret is missing %q", key)
		}
	}
}

// THE POLICY IS ASSERTED BEFORE THE POD EXISTS.
//
// A Pod admitted before its egress policy is in place is a Pod that, for however
// long that window lasts, is governed only by the namespace default-deny — which
// is the SAFE direction and so must not be inverted by a later "create the Pod
// first, it is faster" edit. This test pins the ordering by making the policy
// creation fail and asserting that NO Pod exists afterwards.
func TestProvisionRefusesAndLeavesNoPodWhenTheEgressPolicyCannotBeCreated(t *testing.T) {
	cs := fake.NewClientset()
	// Only the PER-POD policy is refused; the namespace default-deny must still
	// succeed, or the test would be proving something about the namespace floor
	// instead.
	cs.PrependReactor("create", "networkpolicies", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pol, ok := action.(k8stesting.CreateAction).GetObject().(*networkingv1.NetworkPolicy)
		if !ok || !strings.HasPrefix(pol.Name, egressPolicyPrefix) {
			return false, nil, nil
		}
		return true, nil, errForbidden
	})
	s := newTestSubstrate(t, cs, enforcing("10.1.2.3"))
	admitWith(t, cs, s, nil)

	if _, err := s.Provision(context.Background(), loopPrincipal("acme"), sandbox.RemoteSpec{}); err == nil {
		t.Fatal("Provision must refuse when the per-Pod egress policy cannot be created")
	}

	pods, err := cs.CoreV1().Pods(namespaceName("fuse-sb", "acme")).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("List pods: %v", err)
	}
	if len(pods.Items) != 0 {
		t.Fatalf("%d Pod(s) survived a policy failure: %v", len(pods.Items), pods.Items)
	}
}

// assertPosture must REFUSE a Pod whose admitted spec mounts the egress Secret
// into the workload. The renderer never produces one; a mutating webhook can.
// This is the mutation-side half of the sidecar-only assertion above.
func TestAssertPostureRefusesTheEgressSecretMountedIntoTheWorkload(t *testing.T) {
	s := newTestSubstrate(t, fake.NewClientset(), enforcing("10.1.2.3"))
	want, _, err := s.renderPodWithEgress("ns", "sb-x-1", loopPrincipal("t"), time.Unix(1700000000, 0).UTC())
	if err != nil {
		t.Fatalf("renderPodWithEgress: %v", err)
	}

	got := want.DeepCopy()
	for i := range got.Spec.Containers {
		if got.Spec.Containers[i].Name != containerWorkload {
			continue
		}
		got.Spec.Containers[i].VolumeMounts = append(got.Spec.Containers[i].VolumeMounts, corev1.VolumeMount{
			Name:      volumeEgressTLS,
			MountPath: "/stolen",
			ReadOnly:  true,
		})
	}

	err = assertPosture(want, got)
	if err == nil {
		t.Fatal("assertPosture accepted the egress Secret mounted into the workload")
	}
	if !strings.Contains(err.Error(), volumeEgressTLS) {
		t.Errorf("error %q must name the %q volume", err, volumeEgressTLS)
	}
}
