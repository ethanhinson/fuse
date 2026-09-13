package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/ethanhinson/fuse/internal/event"
)

// TestNamespaceNameInjective is THE headline assertion of this task.
//
// A tenant id is rewritten to fit DNS-1123, which is a lossy transformation:
// "_default" and "-default" both slug to "-default", "Acme" and "acme" both
// lowercase to "acme", and an over-long id is truncated. ADR-0057 REFUSED that
// same rewriting for directory names precisely because a lossy map onto an
// identity-bearing name merges two tenants into one. It is acceptable here for
// exactly one reason: the hash suffix is computed over the RAW id, so the map
// stays injective — two distinct tenants can share a slug and can never share a
// name.
//
// If that suffix is ever dropped, weakened, or computed over the slug instead of
// the raw id, this test is what fails, and it fails as a cross-tenant isolation
// break rather than as a cosmetic naming bug.
func TestNamespaceNameInjective(t *testing.T) {
	tenants := []event.TenantID{
		"_default",
		"-default",
		"default",
		"Acme",
		"acme",
		"ACME",
		"acme-corp",
		"acme_corp",
		"acme.corp",
		// Over-long: the slug must be truncated, and the two differ only past the
		// truncation point, so only the hash can keep them apart.
		event.TenantID(strings.Repeat("a", 200) + "-one"),
		event.TenantID(strings.Repeat("a", 200) + "-two"),
		// Unicode: every byte outside [a-z0-9-] is replaced, so these slug to a
		// run of hyphens and are distinguishable by hash alone.
		"日本語",
		"中文",
		"café",
		"cafe",
		// Shapes that could produce an invalid leading/trailing hyphen.
		"-",
		"--",
		"_",
		"",
	}

	seen := make(map[string]event.TenantID, len(tenants))
	for _, tenant := range tenants {
		name := namespaceName("fuse-sb", tenant)

		if prior, dup := seen[name]; dup {
			t.Fatalf("namespaceName collision: tenants %q and %q both map to %q — the tenant→namespace map MUST be injective", prior, tenant, name)
		}
		seen[name] = tenant

		if len(name) > 63 {
			t.Errorf("namespaceName(%q) = %q is %d bytes; a DNS-1123 label is at most 63", tenant, name, len(name))
		}
		if errs := validateDNS1123Label(name); len(errs) > 0 {
			t.Errorf("namespaceName(%q) = %q is not a valid DNS-1123 label: %v", tenant, name, errs)
		}
		if !strings.HasPrefix(name, "fuse-sb-") {
			t.Errorf("namespaceName(%q) = %q does not carry the configured prefix", tenant, name)
		}
	}
}

// TestNamespaceNameHashesRawTenant pins WHICH string the suffix is computed over.
//
// A suffix over the slug would be perfectly valid DNS-1123 and would pass every
// shape assertion above while being useless: "Acme" and "acme" share a slug, so
// they would share a hash and therefore a namespace. This asserts the hash is the
// first 8 hex of sha256 of the RAW id, which is the only version that separates
// them.
func TestNamespaceNameHashesRawTenant(t *testing.T) {
	for _, tenant := range []event.TenantID{"Acme", "acme", "_default", "-default", ""} {
		sum := sha256.Sum256([]byte(tenant))
		want := hex.EncodeToString(sum[:])[:8]

		name := namespaceName("fuse-sb", tenant)
		if got := name[len(name)-8:]; got != want {
			t.Errorf("namespaceName(%q) = %q: suffix %q, want sha256(raw id) prefix %q", tenant, name, got, want)
		}
	}
}

// TestNamespaceNameDeterministic — the name is derived, never minted. Two
// instances of fuse, and the same instance across restarts, must agree on a
// tenant's namespace or each would create its own and the quota would not bind.
func TestNamespaceNameDeterministic(t *testing.T) {
	for range 4 {
		if a, b := namespaceName("fuse-sb", "acme"), namespaceName("fuse-sb", "acme"); a != b {
			t.Fatalf("namespaceName is not deterministic: %q then %q", a, b)
		}
	}
	if a, b := namespaceName("fuse-sb", "acme"), namespaceName("other", "acme"); a == b {
		t.Fatalf("namespaceName ignores the prefix: both prefixes yield %q", a)
	}
}

func TestEnsureNamespaceCreatesWithFloor(t *testing.T) {
	cs := fake.NewClientset()
	s := newTestSubstrate(t, cs)

	ns, err := s.ensureNamespace(context.Background(), loopPrincipal("acme").Tenant)
	if err != nil {
		t.Fatalf("ensureNamespace: %v", err)
	}
	if want := namespaceName(s.namespacePrefix, "acme"); ns != want {
		t.Fatalf("ensureNamespace = %q, want %q", ns, want)
	}

	got, err := cs.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("namespace not created: %v", err)
	}
	if got.Labels[labelManaged] != "true" {
		t.Errorf("namespace labels = %v, want %s=true (the reaper's selector)", got.Labels, labelManaged)
	}
	// The RAW tenant id lives in an annotation, not in the name: the name is
	// lossy and an operator must still be able to read back which tenant owns a
	// namespace.
	if got.Annotations[annotationTenant] != "acme" {
		t.Errorf("namespace annotations = %v, want %s=acme", got.Annotations, annotationTenant)
	}
}

func TestEnsureNamespaceAssertsDefaultDenyAndQuota(t *testing.T) {
	cs := fake.NewClientset()
	s := newTestSubstrate(t, cs)

	ns, err := s.ensureNamespace(context.Background(), "acme")
	if err != nil {
		t.Fatalf("ensureNamespace: %v", err)
	}

	pol, err := cs.NetworkingV1().NetworkPolicies(ns).Get(context.Background(), policyDefaultDeny, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("%s not created: %v", policyDefaultDeny, err)
	}
	// An EMPTY podSelector is what makes this the floor: it selects every Pod in
	// the namespace, including ones a future fuse version creates. A selector on
	// fuse's own label would leave anything else in the namespace unpoliced.
	if len(pol.Spec.PodSelector.MatchLabels) != 0 || len(pol.Spec.PodSelector.MatchExpressions) != 0 {
		t.Errorf("%s podSelector = %+v, want empty (select every Pod)", policyDefaultDeny, pol.Spec.PodSelector)
	}
	// BOTH policy types. Egress alone would leave the metadata endpoint denied
	// but leave the Pod reachable; Ingress alone would not touch metadata at all.
	wantTypes := map[networkingv1.PolicyType]bool{
		networkingv1.PolicyTypeIngress: false,
		networkingv1.PolicyTypeEgress:  false,
	}
	for _, pt := range pol.Spec.PolicyTypes {
		if _, ok := wantTypes[pt]; !ok {
			t.Errorf("%s unexpected policyType %q", policyDefaultDeny, pt)
			continue
		}
		wantTypes[pt] = true
	}
	for pt, seen := range wantTypes {
		if !seen {
			t.Errorf("%s is missing policyType %q — the floor is not closed in that direction", policyDefaultDeny, pt)
		}
	}
	// NO rules. A single empty rule entry means "allow everything" in the
	// NetworkPolicy semantics, which is the inverse of this object's purpose.
	if len(pol.Spec.Ingress) != 0 || len(pol.Spec.Egress) != 0 {
		t.Errorf("%s has rules (ingress %d, egress %d); a deny-all policy has NONE", policyDefaultDeny, len(pol.Spec.Ingress), len(pol.Spec.Egress))
	}
	if pol.Labels[labelManaged] != "true" {
		t.Errorf("%s labels = %v, want %s=true", policyDefaultDeny, pol.Labels, labelManaged)
	}

	q, err := cs.CoreV1().ResourceQuotas(ns).Get(context.Background(), quotaTenant, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("%s not created: %v", quotaTenant, err)
	}
	if got := q.Spec.Hard[corev1.ResourcePods]; got.Value() != 7 {
		t.Errorf("%s pods = %s, want 7 (tenant_quota.max_pods)", quotaTenant, got.String())
	}
	// The aggregate caps are per-Pod limits × max_pods, so the quota is the
	// cluster-side complement of the SAME numbers the Gate uses process-side.
	if got := q.Spec.Hard[corev1.ResourceLimitsMemory]; got.Value() != 7*512*1024*1024 {
		t.Errorf("%s limits.memory = %s, want 7×512Mi", quotaTenant, got.String())
	}
	if got := q.Spec.Hard[corev1.ResourceLimitsCPU]; got.MilliValue() != 7*1500 {
		t.Errorf("%s limits.cpu = %s, want 7×1.5", quotaTenant, got.String())
	}
	if q.Labels[labelManaged] != "true" {
		t.Errorf("%s labels = %v, want %s=true", quotaTenant, q.Labels, labelManaged)
	}
}

// TestEnsureNamespaceQuotaDerivesFromConcurrency — max_pods 0 means "derive it"
// (spec §3), so the quota falls back to concurrency.max_inflight_per_tenant
// rather than to zero. A literal zero would make every Provision fail admission.
func TestEnsureNamespaceQuotaDerivesFromConcurrency(t *testing.T) {
	cs := fake.NewClientset()
	s := newTestSubstrate(t, cs, func(o *Options) {
		o.Config.Kubernetes.TenantQuotaMaxPods = nil
		o.MaxPodsPerTenant = 11
	})

	ns, err := s.ensureNamespace(context.Background(), "acme")
	if err != nil {
		t.Fatalf("ensureNamespace: %v", err)
	}
	q, err := cs.CoreV1().ResourceQuotas(ns).Get(context.Background(), quotaTenant, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("%s not created: %v", quotaTenant, err)
	}
	if got := q.Spec.Hard[corev1.ResourcePods]; got.Value() != 11 {
		t.Errorf("%s pods = %s, want 11 (derived from concurrency.max_inflight_per_tenant)", quotaTenant, got.String())
	}
}

// TestEnsureNamespaceIdempotent — Provision RE-ASSERTS the floor every time, so
// a second call must succeed and must leave exactly one of each object. An
// operator (or a rogue controller) that deletes the default-deny policy gets it
// back on the next Acquire; that is why re-assertion is not wasted work.
func TestEnsureNamespaceIdempotent(t *testing.T) {
	cs := fake.NewClientset()
	s := newTestSubstrate(t, cs)

	for i := range 3 {
		if _, err := s.ensureNamespace(context.Background(), "acme"); err != nil {
			t.Fatalf("ensureNamespace call %d: %v", i+1, err)
		}
	}

	nss, err := cs.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list namespaces: %v", err)
	}
	if len(nss.Items) != 1 {
		t.Fatalf("got %d namespaces, want 1 (Get-then-Create must not duplicate)", len(nss.Items))
	}
	ns := nss.Items[0].Name
	pols, err := cs.NetworkingV1().NetworkPolicies(ns).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list policies: %v", err)
	}
	if len(pols.Items) != 1 {
		t.Fatalf("got %d network policies, want 1", len(pols.Items))
	}
}

// TestEnsureNamespaceReassertsDeletedFloor is the reason re-assertion exists:
// the default-deny policy is deleted out from under fuse and the next Provision
// must put it back rather than run a Pod in an unpoliced namespace.
func TestEnsureNamespaceReassertsDeletedFloor(t *testing.T) {
	cs := fake.NewClientset()
	s := newTestSubstrate(t, cs)

	ns, err := s.ensureNamespace(context.Background(), "acme")
	if err != nil {
		t.Fatalf("ensureNamespace: %v", err)
	}
	if err := cs.NetworkingV1().NetworkPolicies(ns).Delete(context.Background(), policyDefaultDeny, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete policy: %v", err)
	}

	if _, err := s.ensureNamespace(context.Background(), "acme"); err != nil {
		t.Fatalf("ensureNamespace after floor deletion: %v", err)
	}
	if _, err := cs.NetworkingV1().NetworkPolicies(ns).Get(context.Background(), policyDefaultDeny, metav1.GetOptions{}); err != nil {
		t.Fatalf("%s was NOT re-asserted after deletion: %v", policyDefaultDeny, err)
	}
}

// TestEnsureNamespaceToleratesAlreadyExists — two fuse instances racing to serve
// a tenant's first Acquire both see NotFound and both Create. The loser gets
// AlreadyExists, which is a SUCCESS: the namespace it needed exists.
func TestEnsureNamespaceToleratesAlreadyExists(t *testing.T) {
	cs := fake.NewClientset()
	s := newTestSubstrate(t, cs)
	want := namespaceName(s.namespacePrefix, "acme")

	// Reactors run before the tracker, so Get reports NotFound (nothing is
	// there yet) and Create then reports AlreadyExists — exactly the race.
	cs.PrependReactor("create", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewAlreadyExists(
			corev1.Resource("namespaces").WithVersion("v1").GroupResource(), want)
	})

	ns, err := s.ensureNamespace(context.Background(), "acme")
	if err != nil {
		t.Fatalf("ensureNamespace must tolerate AlreadyExists, got: %v", err)
	}
	if ns != want {
		t.Fatalf("ensureNamespace = %q, want %q", ns, want)
	}
}

// TestEnsureNamespaceCreateFailureRefuses — any OTHER Create error is a refusal.
// Returning a namespace name fuse could not confirm exists would put a Pod
// create (and the floor assertion) against a namespace that may not be there.
func TestEnsureNamespaceCreateFailureRefuses(t *testing.T) {
	cs := fake.NewClientset()
	s := newTestSubstrate(t, cs)

	cs.PrependReactor("create", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			corev1.Resource("namespaces").WithVersion("v1").GroupResource(), "", errForbidden)
	})

	if _, err := s.ensureNamespace(context.Background(), "acme"); err == nil {
		t.Fatal("ensureNamespace must refuse when the namespace cannot be created")
	}
}

// TestEnsureNamespaceFloorFailureRefuses — a namespace that exists but whose
// default-deny policy could not be asserted is NOT usable. Returning it would
// run a sandbox Pod with no network floor, which is the precise hole ADR-0058
// rule 4 forbids.
func TestEnsureNamespaceFloorFailureRefuses(t *testing.T) {
	cs := fake.NewClientset()
	s := newTestSubstrate(t, cs)

	cs.PrependReactor("*", "networkpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			networkingv1.Resource("networkpolicies").WithVersion("v1").GroupResource(), "", errForbidden)
	})

	if _, err := s.ensureNamespace(context.Background(), "acme"); err == nil {
		t.Fatal("ensureNamespace must refuse when the default-deny floor cannot be asserted")
	}
}
