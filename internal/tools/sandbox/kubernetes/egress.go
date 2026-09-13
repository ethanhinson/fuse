package kubernetes

// THE EGRESS DATAPATH ON KUBERNETES (change 0075, task 8; spec §5).
//
// A sandbox Pod's traffic reaches the network through exactly one path:
//
//	workload (HTTP_PROXY=http://127.0.0.1:3128)
//	  → egress sidecar, sharing the Pod's loopback
//	    → mutual TLS to fuse's OWN listener at <advertise>:<proxy listen port>
//	      → the allowlist decision, the #52 delegated identity, the destination
//
// and two NetworkPolicies make that the only path: the namespace's
// `fuse-default-deny` (every Pod, both directions, no rules) plus the per-Pod
// `fuse-egress-<pod>`. Policies are ADDITIVE, so default-deny plus one allow is
// exactly the intended reachability set — which is why the per-Pod policy can be
// a single narrow allow rather than a restatement of the floor.
//
// # The floor holds in BOTH postures, differently
//
// Under `enforce` the metadata endpoints are unreachable because the ONLY
// reachable address is the proxy's /32. Under `allow-all` there is no proxy in the
// path at all, so the floor has to be written into the policy itself: the allow is
// `0.0.0.0/0 except [169.254.169.254/32, 169.254.170.2/32]` plus
// `::/0 except [fd00:ec2::254/128]`. That is ADR-0058 rule 4, and the golden test
// asserts it in EACH mode rather than in one and by implication in the other.
//
// # Why the advertise address must name the OWNING instance
//
// The policy and the #52 credentials a principal's traffic is served under live in
// ONE fuse process's memory. A Service ClusterIP would load-balance a sandbox's
// TLS connection to whichever replica the kube-proxy picked, and a replica that
// never enrolled that serial closes the connection — intermittently, depending on
// the balance. So the address is the Pod IP (spec §5 says so explicitly), and
// "unset in both places under enforce" is a construction-time refusal rather than
// a Pod that cannot reach anything.

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/ethanhinson/fuse/internal/loopauth"
	"github.com/ethanhinson/fuse/internal/tools/sandbox"
	"github.com/ethanhinson/fuse/internal/version"
)

// DefaultProxyListen is the address fuse's own TLS listener binds when
// kubernetes.proxy.listen is unset.
//
// It is EXPORTED because the composition root binds the listener (Proxy.ListenTLS)
// while this package derives the per-Pod NetworkPolicy's permitted port and the
// sidecar's -upstream port from the same value. One exported constant is what
// makes "the port fuse listens on" and "the port a Pod may reach" unable to
// drift; a second literal in cmd/fuse would be a silent total egress blackout the
// moment either changed.
//
// PodIPEnvVar is exported for the same reason: the operator doc and the
// advertise-address diagnostic must name the variable this package actually reads.
const (
	DefaultProxyListen = "0.0.0.0:3129"
	PodIPEnvVar        = podIPEnv
)

const (
	// podIPEnv is the downward-API variable the hosted chart sets to the fuse
	// Pod's own IP. It is the DEFAULT advertise address.
	//
	// It is a CLAIM ON change #76's chart, documented in the operator doc, not an
	// agreement already in place: #76 is unmerged, so this change names the
	// variable and defaults to it rather than depending on it existing.
	podIPEnv = "FUSE_POD_IP"

	// egressTLSMount is where the sidecar's credential Secret is mounted. It is
	// inside the sidecar's filesystem only.
	egressTLSMount = "/etc/fuse/egress-tls"

	// The three Secret keys, spelled the way Kubernetes' own kubernetes.io/tls
	// type spells the first two, so an operator inspecting the Secret sees
	// familiar names.
	secretKeyCert = "tls.crt"
	secretKeyKey  = "tls.key"
	secretKeyCA   = "ca.crt"

	// egressPolicyPrefix and egressSecretPrefix are the fixed prefixes of the
	// per-Pod objects. They are prefixes rather than whole names because both are
	// per-Pod and both must be findable from the Pod's name alone (the reaper and
	// an operator both work from that direction).
	egressPolicyPrefix = "fuse-egress-"
	egressSecretPrefix = "fuse-egress-tls-"

	// defaultProxyListen is the address fuse's TLS listener binds when the
	// operator set none. The PORT is what matters here — it is the port the
	// per-Pod policy permits and the port the sidecar dials — and it is read from
	// this value so the two cannot disagree.
	//
	// It is an alias of the EXPORTED DefaultProxyListen rather than a second
	// literal: the composition root has to bind the same address this package
	// derives the policy's permitted port from (change 0075, task 10), and two
	// spellings of one default is exactly how the listener and the policy would
	// come to disagree about which port a Pod may reach.
	defaultProxyListen = DefaultProxyListen

	// sidecarEntrypointPrefix is the arch-specific path of the forwarder inside
	// fuse's own image. The arch suffix is why the Pod carries a REQUIRED arch
	// node affinity: a Pod scheduled onto the other arch has no such file, and
	// under enforce that is a sandbox with no egress datapath at all.
	sidecarEntrypointPrefix = "/fuse-egress-forward-linux-"

	// sidecarImageRepo is fuse's own published image. The sidecar runs FUSE's
	// code, not the operator's: the relay is trusted to be byte-for-byte, and an
	// operator-supplied relay is a relay that could inspect or redirect a
	// sandbox's traffic.
	sidecarImageRepo = "ghcr.io/ethanhinson/fuse"
)

// metadataDenyV4 and metadataDenyV6 are the cloud instance-metadata endpoints.
//
// 169.254.169.254 is the near-universal one (AWS IMDS, GCE, Azure); 169.254.170.2
// is ECS task metadata, which hands out the task role's credentials; fd00:ec2::254
// is AWS IMDS over IPv6. They are denied in BOTH postures because a sandbox that
// can read them holds the NODE's cloud identity — a credential fuse never issued,
// cannot scope, and cannot revoke.
//
// The IPv6 entry is not optional garnish. A cluster with IPv6 egress and only the
// v4 denials has an open metadata path, and it is the one an operator is least
// likely to test.
var (
	metadataDenyV4 = []string{"169.254.169.254/32", "169.254.170.2/32"}
	metadataDenyV6 = []string{"fd00:ec2::254/128"}
)

// resolveAdvertise decides the address a sandbox reaches this instance's proxy on,
// and REFUSES when enforcement needs one and neither place supplied it.
//
// The refusal is at construction rather than at the first Provision on purpose: a
// substrate built without it would render, for every tenant, a sidecar whose
// `-upstream` is empty and a policy whose allowed ipBlock is nothing — and the
// symptom inside every sandbox would be network calls hanging, which is the
// hardest failure shape to attribute to a missing config value.
func resolveAdvertise(k sandbox.Kubernetes, enforcing bool) (string, error) {
	addr := deref(k.ProxyAdvertiseAddress, "")
	if addr == "" {
		addr = os.Getenv(podIPEnv)
	}
	if addr != "" {
		return addr, nil
	}
	if !enforcing {
		// Under allow-all there is no sidecar and no TLS listener to reach, so the
		// address is simply not part of the datapath. The metadata floor in that
		// posture is the policy's `except` list.
		return "", nil
	}
	return "", fmt.Errorf(
		"kubernetes: egress is enforcing but the proxy advertise address is unset; "+
			"set kubernetes.proxy.advertise_address or the $%s environment variable (the downward-API pod IP). "+
			"It must name THIS instance: a Service ClusterIP would load-balance a sandbox onto a replica that does not hold its policy",
		podIPEnv)
}

// proxyListenPort is the port the per-Pod policy permits and the sidecar dials.
//
// It is derived from kubernetes.proxy.listen — the same value the composition root
// passes to Proxy.ListenTLS — so the port a Pod is allowed to reach and the port
// fuse actually listens on come from one place. Parsing it here (rather than
// carrying a separate port knob) is what makes them unable to drift.
func proxyListenPort(k sandbox.Kubernetes) (int, error) {
	listen := deref(k.ProxyListen, defaultProxyListen)
	_, portStr, err := net.SplitHostPort(listen)
	if err != nil {
		return 0, fmt.Errorf("kubernetes: proxy.listen %q must be host:port: %w", listen, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("kubernetes: proxy.listen %q names no usable port", listen)
	}
	return port, nil
}

// enforcing reports whether this substrate provisions the egress sidecar.
//
// It is the POSTURE and not the allowlist's length: under enforce with an EMPTY
// allowlist (ADR-0053's salvaged posture) the sidecar and the TLS material are
// still provisioned and the proxy refuses everything. Deny-all is then a decision
// the operator can observe in a refusal hook, observably distinct from a missing
// datapath — which presents as a hang and tells them nothing.
func (s *Substrate) enforcing() bool { return s.egress.Mode == sandbox.EgressEnforce }

// egressPolicyName and egressSecretName are the per-Pod object names, derived from
// the Pod's name so an operator (and the garbage collector) can relate the three.
func egressPolicyName(pod string) string { return egressPolicyPrefix + pod }
func egressSecretName(pod string) string { return egressSecretPrefix + pod }

// renderEgressPolicy is the per-Pod allow, in whichever posture is configured.
//
// Egress ONLY — Ingress stays with the namespace floor. Adding Ingress here would
// mean this policy also decided reachability INTO the Pod, and since policies are
// additive the effect of an empty ingress rule set on a policy that declares the
// Ingress type is to allow nothing, which the floor already does. Declaring it
// would buy nothing and would make this object's purpose ambiguous.
func (s *Substrate) renderEgressPolicy(ns, pod string) *networkingv1.NetworkPolicy {
	var to []networkingv1.NetworkPolicyPeer
	var ports []networkingv1.NetworkPolicyPort

	if s.enforcing() {
		// EXACTLY ONE DESTINATION: this instance's proxy, on this instance's
		// listen port. Everything else — the metadata endpoints included — is
		// unreachable because it is not named, which is the strongest form the
		// floor takes.
		to = []networkingv1.NetworkPolicyPeer{{
			IPBlock: &networkingv1.IPBlock{CIDR: s.advertiseAddress + "/32"},
		}}
		port := intstr.FromInt32(int32(s.proxyPort)) // #nosec G115 -- validated 1..65535 by proxyListenPort
		ports = []networkingv1.NetworkPolicyPort{{Port: &port}}
	} else {
		// ALLOW-ALL, MINUS THE FLOOR. There is no proxy in the path, so the
		// metadata denial has to be written into the allow itself. `except` is the
		// only NetworkPolicy construct that can carve an address out of a
		// permitted block, which is why the rule is one broad ipBlock with an
		// exception list rather than an enumeration of permitted ranges.
		to = []networkingv1.NetworkPolicyPeer{
			{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: append([]string(nil), metadataDenyV4...)}},
			{IPBlock: &networkingv1.IPBlock{CIDR: "::/0", Except: append([]string(nil), metadataDenyV6...)}},
		}
		// No port restriction: allow-all means every port. The FLOOR is about
		// addresses, not ports — 169.254.169.254 is dangerous on any port.
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      egressPolicyName(pod),
			Namespace: ns,
			Labels:    map[string]string{labelManaged: "true"},
		},
		Spec: networkingv1.NetworkPolicySpec{
			// The Pod's own name, carried as a LABEL by renderPod: a podSelector
			// matches labels and cannot match a name.
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{labelPod: pod}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      []networkingv1.NetworkPolicyEgressRule{{To: to, Ports: ports}},
		},
	}
}

// renderEgressSecret holds the sidecar's credential. It carries no ownerReference
// yet: the Pod it must be owned by does not exist when this is called, so
// Provision sets it after the Create (see adoptSecret).
func renderEgressSecret(ns, pod string, cred sandbox.SandboxCredential) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      egressSecretName(pod),
			Namespace: ns,
			Labels:    map[string]string{labelManaged: "true", labelPod: pod},
		},
		// Opaque rather than kubernetes.io/tls: that type REQUIRES tls.crt and
		// tls.key to be a server certificate and rejects a bundle carrying a third
		// key, and this Secret carries the CA the sidecar verifies the proxy with.
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			secretKeyCert: cred.CertPEM,
			secretKeyKey:  cred.KeyPEM,
			secretKeyCA:   cred.CAPEM,
		},
	}
}

// renderSidecar is the egress forwarder container.
//
// Its argv is the whole of its configuration: there is no config file, no
// environment, and nothing it reads but the three PEM files in a read-only mount.
// That is deliberate — this container sits inside the sandbox's network namespace,
// so everything it can be told is something the sandbox's neighbour can observe.
func (s *Substrate) renderSidecar() corev1.Container {
	fals := false
	return corev1.Container{
		Name:  containerEgress,
		Image: s.sidecarImage,
		Command: []string{
			sidecarEntrypointPrefix + s.arch,
			"-listen", podEgressListen,
			"-upstream", "tls://" + net.JoinHostPort(s.advertiseAddress, strconv.Itoa(s.proxyPort)),
			"-tls-cert", egressTLSMount + "/" + secretKeyCert,
			"-tls-key", egressTLSMount + "/" + secretKeyKey,
			"-tls-ca", egressTLSMount + "/" + secretKeyCA,
		},
		// EMPTY for the same reason the workload's is: everything this container
		// needs is in argv and in one mounted file, so a baked variable could only
		// be a leak — and the assertPosture env check is written against `want`,
		// so an injector adding one here is caught too.
		Env:     nil,
		EnvFrom: nil,
		// The SAME hardened context as the workload. The sidecar shares the Pod's
		// network namespace and its loopback, so a privileged sidecar is a
		// privileged Pod — and the loopback is where every policy decision begins.
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: &fals,
			Privileged:               &fals,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: []corev1.VolumeMount{{
			Name:      volumeEgressTLS,
			MountPath: egressTLSMount,
			// READ-ONLY. The credential is fuse's, handed to the sidecar to
			// present; a writable mount is a sidecar that could replace it.
			ReadOnly: true,
		}},
		// NO workspace mount. The sidecar relays bytes and has no business in the
		// command's filesystem — and a sidecar that could read the workspace could
		// exfiltrate it over the one connection it is allowed to open.
	}
}

// resolveSidecarImage picks the forwarder's image: the operator's if they named
// one, else fuse's own published image at THIS binary's version.
//
// Version-pinned rather than `:latest`: the sidecar's entrypoint path and flag
// grammar are this binary's, so a floating tag is a sidecar that can be a
// different program than the one that rendered its argv.
func resolveSidecarImage(k sandbox.Kubernetes) string {
	if img := deref(k.SidecarImage, ""); img != "" {
		return img
	}
	return sidecarImageRepo + ":" + version.Version
}

// renderPodWithEgress renders the Pod together with the Secret its sidecar
// mounts, minting a credential under enforce.
//
// The two are returned together, and the Secret is nil under allow-all, because
// they are ONE decision: a Pod with a sidecar and no Secret never starts, and a
// Secret with no Pod is litter nothing collects. Returning them separately would
// let a caller create one without the other.
func (s *Substrate) renderPodWithEgress(ns, name string, p loopauth.Principal, now time.Time) (*corev1.Pod, *corev1.Secret, error) {
	pod := s.renderPod(ns, name, p, now)
	if !s.enforcing() {
		return pod, nil, nil
	}

	if s.credentials == nil {
		// Unreachable from a substrate built by newSubstrate, which refuses this
		// combination. Kept as a fail-closed guard rather than a nil dereference,
		// because the failure it guards is "a Pod with a sidecar and no
		// credential", which starts and then silently has no egress.
		return nil, nil, errors.New("kubernetes: enforcing egress with no credential source")
	}
	cred, err := s.credentials.EnrollSandbox(p)
	if err != nil {
		return nil, nil, fmt.Errorf("kubernetes: enroll egress credential for %s/%s: %w", ns, name, err)
	}

	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: volumeEgressTLS,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: egressSecretName(name),
				// 0400 for the owner alone. The mount is already read-only and
				// already sidecar-only; the mode is the third, independent layer,
				// and it costs nothing.
				DefaultMode: ptrInt32(0o400),
			},
		},
	})
	pod.Spec.Containers = append(pod.Spec.Containers, s.renderSidecar())

	return pod, renderEgressSecret(ns, name, cred), nil
}

// adoptSecret points the Secret at the Pod so Kubernetes garbage-collects it.
//
// An ownerReference rather than an explicit delete in Teardown: the Pod can end in
// ways fuse never observes (activeDeadlineSeconds, a node failure, an operator's
// kubectl), and a Secret whose deletion depended on fuse running would outlive
// every one of those. Its content is a client certificate whose serial the proxy
// may already have revoked — litter an operator cannot tell from a live credential.
func adoptSecret(sec *corev1.Secret, pod *corev1.Pod) {
	tru := true
	sec.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "v1",
		Kind:       "Pod",
		Name:       pod.Name,
		UID:        pod.UID,
		// The Secret exists only to serve this Pod, so its deletion must not wait
		// on anything else.
		BlockOwnerDeletion: &tru,
	}}
}

// ipBlockReaches reports whether an ipBlock permits addr — the CIDR contains it
// and no `except` entry excludes it.
//
// It exists so the metadata-floor assertion can be written as a REACHABILITY
// question rather than as a comparison of spellings. That matters: the two
// postures express the floor completely differently (one by omission, one by
// `except`), and a spelling check would have to be written twice and could pass
// twice while the property held in neither.
func ipBlockReaches(block *networkingv1.IPBlock, addr string) bool {
	ip := net.ParseIP(addr)
	if ip == nil {
		return false
	}
	_, cidr, err := net.ParseCIDR(block.CIDR)
	if err != nil || !cidr.Contains(ip) {
		return false
	}
	for _, ex := range block.Except {
		if _, exNet, exErr := net.ParseCIDR(ex); exErr == nil && exNet.Contains(ip) {
			return false
		}
	}
	return true
}

func ptrInt32(v int32) *int32 { return &v }
