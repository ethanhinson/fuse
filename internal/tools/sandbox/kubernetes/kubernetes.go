// Package kubernetes implements sandbox.RemoteSubstrate on top of Kubernetes.
//
// # Where the dependency boundary is, and why
//
// This package imports internal/tools/sandbox. internal/tools/sandbox must NEVER
// import this one, nor anything under k8s.io/. That is ADR-0058 rule 1, and it is
// asserted mechanically by TestSandboxDoesNotImportControlPlaneSDK in the sandbox
// package rather than left to review.
//
// The reason is not tidiness. internal/tools/sandbox is the leaf the bash tool
// depends on; a client-go import there would drag a scheme registry, a rest
// config loader and dozens of transitive modules into every binary and every test
// that touches a shell command — and, worse, it would invert the seam. Once the
// package that defines Handler knows what a Pod is, the adapter stops being an
// adapter and the "one substrate, one code path" property ADR-0044 was protecting
// is gone.
//
// Construction is therefore registered from the composition root through
// sandbox.WithHandlerFactory, which is the only place the two halves meet.
//
// # What this package promises
//
// Everything Kubernetes hands back is a CLAIM. A Pod created with a posture is
// not a Pod running with that posture: admission webhooks mutate, defaulting
// fills, and a cluster's own controllers inject. So Provision's contract
// (sandbox.RemoteSubstrate) is confirm-or-tear-down, and the confirmation is a
// read-back of the ADMITTED object asserted field by field against the demand.
package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// SubstrateName is the bounded handler identifier this substrate reports. It
// agrees with sandbox.HandlerKubernetes by construction (asserted in tests): the
// config value an operator types and the event label a dashboard groups by must
// be the same string, or a `handler: kubernetes` deployment files its events
// under a name nobody can query.
const SubstrateName = sandbox.HandlerKubernetes

// Object names and metadata keys.
//
// Every one of these is FIXED rather than configurable. They name objects fuse
// creates in a namespace fuse owns, and an operator-settable name here would buy
// nothing except the ability to have two fuse versions disagree about which
// policy is the floor.
const (
	// labelManaged marks every object fuse creates. It is the reaper's selector,
	// which is why it is on the namespace as well as on the Pods: a reaper that
	// had to enumerate all namespaces would sweep an operator's own workloads.
	labelManaged = "fuse.dev/managed"
	// labelPrincipal is the HASH of the principal a Pod belongs to, never the
	// identity itself: a label value is world-readable to anyone with namespace
	// list, has a 63-byte ceiling, and must be DNS-safe.
	labelPrincipal = "fuse.dev/principal"
	// labelInstance names the fuse instance that owns a Pod. It is deliberately
	// NOT part of the reaper's selector — an orphan is precisely a Pod whose
	// owning instance is gone — and exists for operator legibility.
	labelInstance = "fuse.dev/instance"
	// labelPod carries a Pod's own name so a per-Pod NetworkPolicy can select it.
	// NetworkPolicy podSelector matches labels, not names.
	labelPod = "fuse.dev/pod"

	// annotationTenant carries the RAW tenant id. The namespace NAME is lossy
	// (see namespaceName), so this is the only place an operator can read back
	// which tenant a namespace belongs to.
	annotationTenant = "fuse.dev/tenant"
	// annotationHeartbeat is the RFC 3339 instant the owning instance last
	// asserted ownership. It is the reaper's whole input.
	annotationHeartbeat = "fuse.dev/heartbeat"

	// policyDefaultDeny is the namespace floor: every Pod, both directions, no
	// rules. NetworkPolicies are additive, so this plus one per-Pod allow is
	// exactly the intended reachability set.
	policyDefaultDeny = "fuse-default-deny"
	// quotaTenant bounds an orphan storm per tenant even before the reaper runs.
	quotaTenant = "fuse-tenant"
)

// Substrate defaults. Each is the value a field the operator left unset resolves
// to; none of them is the zero value, which is why sandbox.Kubernetes carries
// every field as a pointer.
const (
	defaultNamespacePrefix  = "fuse-sb"
	defaultImage            = "alpine:3.20"
	defaultServiceAccount   = "fuse-sandbox"
	defaultStartupTimeout   = 60 * time.Second
	defaultPodMaxLifetime   = 4 * time.Hour
	defaultWorkspaceSize    = int64(2 * 1024 * 1024 * 1024)
	defaultMaxPodsPerTenant = 8
)

// Options is everything the composition root must hand this substrate that is
// not already in the sandbox Config.
//
// It exists as a struct rather than a variadic option list because every field is
// REQUIRED information the substrate cannot invent: an instance id it made up
// would make two instances look like one to the reaper, and an arch it guessed
// would pin Pods to nodes whose sidecar entrypoint does not exist.
type Options struct {
	// Config is the trusted, load-once sandbox configuration. Its Kubernetes
	// block supplies every operator-settable knob; its Limits and Egress are the
	// posture the Pod must be built to.
	Config sandbox.Config

	// MaxPodsPerTenant is the resolved concurrency.max_inflight_per_tenant, used
	// to size the ResourceQuota when kubernetes.tenant_quota.max_pods is unset or
	// an explicit 0. The quota is the cluster-side complement of the same number
	// the in-process Gate enforces.
	MaxPodsPerTenant int64

	// InstanceID is this fuse instance's node id, recorded on every Pod. It is
	// for legibility and for the heartbeat's owner, NOT for the reaper's
	// selection.
	InstanceID string

	// Arch is the GOARCH sandbox Pods must be scheduled onto. It defaults to this
	// binary's runtime.GOARCH, which is the correct answer because the egress
	// sidecar's entrypoint path is arch-specific and the sidecar image is fuse's
	// own.
	Arch string
}

// Substrate is a Kubernetes-backed sandbox.RemoteSubstrate.
//
// Everything resolved from config is resolved ONCE, here, at construction. A
// Provision that re-read config could observe a different posture per Pod, and
// "which posture is this Pod running" would stop having an answer.
type Substrate struct {
	cs kubernetes.Interface

	namespacePrefix string
	image           string
	serviceAccount  string
	runtimeClass    string
	startupTimeout  time.Duration
	podMaxLifetime  time.Duration
	workspaceSize   int64

	limits sandbox.Limits
	egress sandbox.Egress

	// maxPodsPerTenant is the resolved concurrency.max_inflight_per_tenant, kept
	// distinct from quotaMaxPods so the derivation is legible: one is what the
	// Gate enforces in-process, the other is what the cluster enforces.
	maxPodsPerTenant int64
	quotaMaxPods     int64

	instanceID string
	arch       string
}

// Name and Reap are in place from this task; Provision arrives with the Pod
// (task 5) and Verify with the canary pair (task 9), at which point the
// compile-time conformance assertion
//
//	var _ sandbox.RemoteSubstrate = (*Substrate)(nil)
//
// is declared. It is deliberately NOT declared yet: a stub method that returned
// a nil error to satisfy the interface early would be a substrate claiming to
// have verified a floor it never probed, which is the one failure ADR-0058
// rule 3 exists to make impossible.

// New builds a Substrate against a real cluster.
//
// In-cluster configuration is the DEFAULT and kubeconfig is the exception,
// because the shipped posture is a fuse Pod talking to its own API server; a
// laptop driving kind is the dev loop and says so explicitly.
func New(opts Options) (*Substrate, error) {
	cfg, err := restConfig(opts.Config.Kubernetes)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes: client: %w", err)
	}
	return newSubstrate(cs, opts)
}

// restConfig resolves the client configuration, in-cluster unless a kubeconfig
// was named.
func restConfig(k sandbox.Kubernetes) (*rest.Config, error) {
	path := deref(k.Kubeconfig, "")
	kctx := deref(k.Context, "")
	if path == "" && kctx == "" {
		cfg, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("kubernetes: in-cluster config (set kubernetes.kubeconfig for an out-of-cluster fuse): %w", err)
		}
		return cfg, nil
	}

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if path != "" {
		rules.ExplicitPath = path
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{CurrentContext: kctx}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kubernetes: kubeconfig: %w", err)
	}
	return cfg, nil
}

// newSubstrate is the constructor every test drives, over any
// kubernetes.Interface (the generated fake included). Splitting it from New is
// what makes the whole of this package's behaviour reachable with no cluster.
func newSubstrate(cs kubernetes.Interface, opts Options) (*Substrate, error) {
	if cs == nil {
		return nil, errors.New("kubernetes: substrate needs a clientset")
	}
	// A REFUSED block is not "no configuration" — it is configuration fuse threw
	// away (WarnBadKubernetes). Building from substrate defaults here would hand
	// the operator a working sandbox in the wrong namespace, under the wrong
	// service account, with an unbounded workspace, and never tell them: the
	// security-knob-inert-at-composition-root failure exactly.
	if opts.Config.Kubernetes.Refused {
		return nil, errors.New("kubernetes: the kubernetes: config block was refused at load (bad_kubernetes); refusing to build a substrate from defaults the operator did not choose")
	}

	k := opts.Config.Kubernetes

	prefix := deref(k.NamespacePrefix, defaultNamespacePrefix)
	if errs := validateDNS1123Label(prefix); len(errs) > 0 {
		return nil, fmt.Errorf("kubernetes: namespace_prefix %q is not a DNS-1123 label: %s", prefix, strings.Join(errs, "; "))
	}

	// The image falls back through kubernetes.image, then the top-level image:,
	// then the pinned default — in that order, because the more specific setting
	// wins and the pinned default is what the canary's `nc` requirement is stated
	// against.
	image := deref(k.Image, "")
	if image == "" {
		image = opts.Config.Image
	}
	if image == "" {
		image = defaultImage
	}

	arch := opts.Arch
	if arch == "" {
		arch = runtime.GOARCH
	}

	maxPods := opts.MaxPodsPerTenant
	if maxPods <= 0 {
		maxPods = defaultMaxPodsPerTenant
	}

	s := &Substrate{
		cs:              cs,
		namespacePrefix: prefix,
		image:           image,
		serviceAccount:  deref(k.ServiceAccount, defaultServiceAccount),
		runtimeClass:    deref(k.RuntimeClass, ""),
		startupTimeout:  derefDuration(k.StartupTimeout, defaultStartupTimeout),
		podMaxLifetime:  derefDuration(k.PodMaxLifetime, defaultPodMaxLifetime),
		// fsize bounds ONE file on the local substrate; here the only expression
		// available is a bound on the whole workspace, so an explicit
		// workspace.size_limit wins and limits.fsize is the fallback
		// approximation (spec §4).
		workspaceSize:    resolveWorkspaceSize(k.WorkspaceSizeLimitBytes, opts.Config.Limits.FsizeBytes),
		limits:           opts.Config.Limits,
		egress:           opts.Config.Egress,
		maxPodsPerTenant: maxPods,
		// An explicitly configured tenant_quota.max_pods overrides the derived
		// concurrency number. An explicit 0 means "derive it" (spec §6) and is
		// therefore NOT honoured as a ceiling of zero, which would refuse every
		// Pod the operator was trying to permit.
		quotaMaxPods: derefPositiveInt64(k.TenantQuotaMaxPods, maxPods),
		instanceID:   opts.InstanceID,
		arch:         arch,
	}
	return s, nil
}

// resolveWorkspaceSize picks the emptyDir bound: the explicit knob, else
// limits.fsize as an approximation, else the pinned default.
func resolveWorkspaceSize(explicit, fsize *int64) int64 {
	if explicit != nil && *explicit > 0 {
		return *explicit
	}
	if fsize != nil && *fsize > 0 {
		return *fsize
	}
	return defaultWorkspaceSize
}

func derefPositiveInt64(p *int64, def int64) int64 {
	if p == nil || *p <= 0 {
		return def
	}
	return *p
}

// Name reports the bounded substrate identifier.
func (s *Substrate) Name() string { return SubstrateName }

// namespaceName maps a tenant id onto a DNS-1123 namespace name.
//
// # Why this map may rewrite an identity where ADR-0057 refused to
//
// The tenant id is an identity. ADR-0057 REFUSED to slug it into a host
// directory name, because a lossy rewrite of an identity-bearing name merges two
// distinct tenants onto one path ("Acme" and "acme"; "_default" and "-default"),
// and a merged tenant filesystem is a cross-tenant read.
//
// A Kubernetes namespace cannot hold a raw tenant id: the API server enforces
// DNS-1123 and would reject "_default" outright, so "refuse the id" is not an
// option that leaves the substrate usable at all. What makes the rewrite
// acceptable is that it is composed with a HASH OF THE RAW ID, so the composite
// map is INJECTIVE: two tenants may share a slug and can never share a name.
// That is the only property that matters here — legibility of the slug is a
// convenience, and the raw id is preserved exactly in the fuse.dev/tenant
// annotation.
//
// The hash is therefore load-bearing security, not a uniquifier. Compute it over
// the slug instead of the raw id and "Acme"/"acme" collide again, silently, with
// every assertion about DNS validity still passing. TestNamespaceNameInjective
// and TestNamespaceNameHashesRawTenant exist to catch exactly that edit.
//
// The truncation order is likewise deliberate: the suffix is appended AFTER the
// slug is cut to fit, so an over-long tenant loses slug bytes and never hash
// bytes.
func namespaceName(prefix string, tenant event.TenantID) string {
	sum := sha256.Sum256([]byte(tenant))
	h8 := hex.EncodeToString(sum[:])[:8]

	// prefix + "-" + slug + "-" + h8 must fit 63.
	budget := 63 - len(prefix) - 1 - 1 - len(h8)
	slug := dns1123Slug(string(tenant), budget)
	if slug == "" {
		// Every byte was unrepresentable, or the tenant was empty. The hash alone
		// still distinguishes it; "t" is a filler so the name never has an empty
		// middle label segment.
		slug = "t"
	}
	return prefix + "-" + slug + "-" + h8
}

// dns1123Slug lowercases s, replaces every byte outside [a-z0-9-] with '-',
// truncates to budget, and trims so the result neither starts nor ends with '-'.
//
// Collapsing runs of '-' is deliberately NOT done: it would be a second lossy
// step buying nothing (the hash already carries uniqueness) while making the
// slug harder to relate back to the id an operator typed.
func dns1123Slug(s string, budget int) string {
	if budget <= 0 {
		return ""
	}
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
			b = append(b, c)
		case c >= 'A' && c <= 'Z':
			// Case-folding is safe HERE only because the hash below is computed
			// over the unfolded id; see namespaceName.
			b = append(b, c+('a'-'A'))
		default:
			b = append(b, '-')
		}
	}
	if len(b) > budget {
		b = b[:budget]
	}
	// A leading or trailing '-' is not a valid DNS-1123 label boundary, and
	// truncation can create a trailing one.
	return strings.Trim(string(b), "-")
}

// validateDNS1123Label reports why name is not a DNS-1123 label, or nothing.
func validateDNS1123Label(name string) []string {
	return validation.IsDNS1123Label(name)
}

// ensureNamespace makes the tenant's namespace exist with its floor asserted,
// and returns its name.
//
// It is called on EVERY Provision, not only on the first, and re-asserts the
// default-deny policy and the quota each time. That is not wasted work: an
// operator or a controller that deletes the floor must not thereby obtain an
// unpoliced namespace, and a fuse that only asserted the floor at creation time
// would run sandbox Pods in one and never notice.
//
// Any failure is a REFUSAL. Returning a namespace whose floor could not be
// asserted would place a Pod in it anyway, which is the precise hole ADR-0058
// rule 4 exists to forbid.
func (s *Substrate) ensureNamespace(ctx context.Context, tenant event.TenantID) (string, error) {
	name := namespaceName(s.namespacePrefix, tenant)

	if _, err := s.cs.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{}); err != nil {
		if !apierrors.IsNotFound(err) {
			return "", fmt.Errorf("kubernetes: namespace %s: %w", name, err)
		}
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Labels:      map[string]string{labelManaged: "true"},
				Annotations: map[string]string{annotationTenant: string(tenant)},
			},
		}
		if _, err := s.cs.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
			// AlreadyExists is SUCCESS: two instances racing a tenant's first
			// Acquire both see NotFound and both Create, and the loser needed
			// exactly the namespace the winner made.
			if !apierrors.IsAlreadyExists(err) {
				return "", fmt.Errorf("kubernetes: create namespace %s: %w", name, err)
			}
		}
	}

	if err := s.assertDefaultDeny(ctx, name); err != nil {
		return "", err
	}
	if err := s.assertQuota(ctx, name); err != nil {
		return "", err
	}
	return name, nil
}

// assertDefaultDeny puts (or puts back) the namespace floor.
func (s *Substrate) assertDefaultDeny(ctx context.Context, ns string) error {
	pol := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      policyDefaultDeny,
			Namespace: ns,
			Labels:    map[string]string{labelManaged: "true"},
		},
		Spec: networkingv1.NetworkPolicySpec{
			// EMPTY selector = every Pod in the namespace, including Pods a
			// future fuse version creates and any object an operator puts here.
			// A selector on fuse's own label would leave everything else
			// unpoliced.
			PodSelector: metav1.LabelSelector{},
			// BOTH directions. Egress alone leaves the Pod reachable; Ingress
			// alone does not touch the metadata endpoint at all.
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			// NO rules. One empty rule entry means "allow all" in NetworkPolicy
			// semantics, which is the exact inverse of this object.
		},
	}
	return s.applyPolicy(ctx, ns, pol)
}

// applyPolicy creates pol, or updates the existing object's spec to match.
//
// Update-to-match rather than leave-alone: a policy that exists with the WRONG
// spec is worse than one that is absent, because it looks like the floor and is
// not. The whole spec is overwritten so a partially edited policy cannot survive.
func (s *Substrate) applyPolicy(ctx context.Context, ns string, pol *networkingv1.NetworkPolicy) error {
	api := s.cs.NetworkingV1().NetworkPolicies(ns)
	if _, err := api.Create(ctx, pol, metav1.CreateOptions{}); err == nil {
		return nil
	} else if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("kubernetes: create networkpolicy %s/%s: %w", ns, pol.Name, err)
	}

	cur, err := api.Get(ctx, pol.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("kubernetes: networkpolicy %s/%s: %w", ns, pol.Name, err)
	}
	cur.Spec = pol.Spec
	if cur.Labels == nil {
		cur.Labels = map[string]string{}
	}
	for k, v := range pol.Labels {
		cur.Labels[k] = v
	}
	if _, err := api.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("kubernetes: update networkpolicy %s/%s: %w", ns, pol.Name, err)
	}
	return nil
}

// assertQuota sizes the per-tenant ResourceQuota from the same numbers the
// in-process Gate uses.
//
// The quota is the CLUSTER-side complement of the Gate, not a duplicate of it:
// the Gate bounds in-flight Execs in this process, and the quota bounds Pods a
// tenant can hold across every instance — including Pods leaked by an instance
// that died. That is what makes an orphan storm bounded before the reaper runs.
func (s *Substrate) assertQuota(ctx context.Context, ns string) error {
	maxPods := s.maxPods()

	hard := corev1.ResourceList{
		corev1.ResourcePods: *resource.NewQuantity(maxPods, resource.DecimalSI),
	}
	// A cap the operator did not set is NOT quota'd at zero: an absent limit
	// means "the substrate's default", and a zero aggregate would refuse every
	// Pod.
	if s.limits.MemoryBytes != nil && *s.limits.MemoryBytes > 0 {
		hard[corev1.ResourceLimitsMemory] = *resource.NewQuantity(*s.limits.MemoryBytes*maxPods, resource.BinarySI)
	}
	if s.limits.CPUs != nil {
		if q, err := resource.ParseQuantity(*s.limits.CPUs); err == nil {
			hard[corev1.ResourceLimitsCPU] = *resource.NewMilliQuantity(q.MilliValue()*maxPods, resource.DecimalSI)
		}
	}

	q := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      quotaTenant,
			Namespace: ns,
			Labels:    map[string]string{labelManaged: "true"},
		},
		Spec: corev1.ResourceQuotaSpec{Hard: hard},
	}

	api := s.cs.CoreV1().ResourceQuotas(ns)
	if _, err := api.Create(ctx, q, metav1.CreateOptions{}); err == nil {
		return nil
	} else if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("kubernetes: create resourcequota %s/%s: %w", ns, quotaTenant, err)
	}

	cur, err := api.Get(ctx, quotaTenant, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("kubernetes: resourcequota %s/%s: %w", ns, quotaTenant, err)
	}
	cur.Spec = q.Spec
	if cur.Labels == nil {
		cur.Labels = map[string]string{}
	}
	cur.Labels[labelManaged] = "true"
	if _, err := api.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("kubernetes: update resourcequota %s/%s: %w", ns, quotaTenant, err)
	}
	return nil
}

// maxPods is the per-tenant Pod ceiling the quota is sized from, resolved once
// at construction.
func (s *Substrate) maxPods() int64 { return s.quotaMaxPods }

func deref(p *string, def string) string {
	if p == nil || *p == "" {
		return def
	}
	return *p
}

func derefDuration(p *time.Duration, def time.Duration) time.Duration {
	if p == nil || *p <= 0 {
		return def
	}
	return *p
}
