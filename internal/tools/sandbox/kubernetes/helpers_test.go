package kubernetes

import (
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// errForbidden is the cause a forbidden-reactor reports. Its text is never
// asserted on; only the refusal is.
var errForbidden = errors.New("rbac: fuse may not do that")

func ptr[T any](v T) *T { return &v }

// loopPrincipal is the one shape of principal these tests provision for.
func loopPrincipal(tenant event.TenantID) loopauth.Principal {
	return loopauth.Principal{Tenant: tenant, Subject: "subject-" + string(tenant)}
}

// newTestSubstrate builds a substrate over a fake clientset with a FIXED,
// fully-specified posture, so every golden and every quota assertion in this
// package reads against known numbers rather than against whatever the
// substrate's defaults happen to be that week.
//
// The mutators run after the baseline so a test can express exactly the one
// field it cares about.
func newTestSubstrate(t *testing.T, cs kubernetes.Interface, mutate ...func(*Options)) *Substrate {
	t.Helper()

	opts := Options{
		Config: sandbox.Config{
			Image: "alpine:3.20",
			Limits: sandbox.Limits{
				MemoryBytes: ptr(int64(512 * 1024 * 1024)),
				CPUs:        ptr("1.5"),
				FsizeBytes:  ptr(int64(64 * 1024 * 1024)),
			},
			Kubernetes: sandbox.Kubernetes{
				NamespacePrefix:         ptr("fuse-sb"),
				TenantQuotaMaxPods:      ptr(int64(7)),
				StartupTimeout:          ptr(2 * time.Second),
				PodMaxLifetime:          ptr(4 * time.Hour),
				WorkspaceSizeLimitBytes: ptr(int64(2 * 1024 * 1024 * 1024)),
			},
		},
		MaxPodsPerTenant: 4,
		InstanceID:       "instance-a",
		Arch:             "arm64",
	}
	for _, m := range mutate {
		m(&opts)
	}

	s, err := newSubstrate(cs, opts)
	if err != nil {
		t.Fatalf("newSubstrate: %v", err)
	}
	return s
}

// TestSubstrateNameMatchesConfigValue pins the one string that must be spelled
// identically in three places: the value an operator types in `handler:`, the
// factory key the composition root registers, and the handler label on every
// event and metric. A drift would make a configured deployment file its events
// under a name no dashboard queries — and would make the factory lookup miss,
// which selectHandler answers by REFUSING, so the drift presents as "kubernetes
// is not available" with the config plainly naming it.
func TestSubstrateNameMatchesConfigValue(t *testing.T) {
	if SubstrateName != sandbox.HandlerKubernetes {
		t.Fatalf("SubstrateName = %q, want sandbox.HandlerKubernetes = %q", SubstrateName, sandbox.HandlerKubernetes)
	}
	if (&Substrate{}).Name() != sandbox.HandlerKubernetes {
		t.Fatalf("Substrate.Name() = %q, want %q", (&Substrate{}).Name(), sandbox.HandlerKubernetes)
	}
}

// TestNewSubstrateRefusesRefusedBlock is the load-time refusal fact honoured at
// construction. An absent block (all-nil, Refused false) means "use the
// substrate's defaults" and must BUILD; a refused one means the operator's block
// was discarded and must build NOTHING — the security-knob-inert-at-composition-
// root failure is precisely a factory that treats the two as the same.
func TestNewSubstrateRefusesRefusedBlock(t *testing.T) {
	if _, err := newSubstrate(fake.NewClientset(), Options{
		Config: sandbox.Config{Kubernetes: sandbox.Kubernetes{Refused: true}},
	}); err == nil {
		t.Fatal("newSubstrate must refuse a Refused kubernetes: block")
	}

	// The absent block, by contrast, is the ordinary hosted default.
	s, err := newSubstrate(fake.NewClientset(), Options{Config: sandbox.Config{}})
	if err != nil {
		t.Fatalf("newSubstrate with an ABSENT kubernetes: block must use substrate defaults: %v", err)
	}
	if s.namespacePrefix != defaultNamespacePrefix {
		t.Errorf("namespacePrefix = %q, want the default %q", s.namespacePrefix, defaultNamespacePrefix)
	}
	if s.image != defaultImage {
		t.Errorf("image = %q, want the default %q", s.image, defaultImage)
	}
	if s.serviceAccount != defaultServiceAccount {
		t.Errorf("serviceAccount = %q, want the default %q", s.serviceAccount, defaultServiceAccount)
	}
}

// TestNewSubstrateRefusesBadNamespacePrefix — a prefix that is not a DNS-1123
// label makes EVERY namespace creation for EVERY tenant fail at the API server
// forever. Refusing at construction turns that into one legible startup error.
func TestNewSubstrateRefusesBadNamespacePrefix(t *testing.T) {
	for _, bad := range []string{"Fuse_SB", "fuse sb", "-fuse", "fuse-", strings.Repeat("f", 64)} {
		if _, err := newSubstrate(fake.NewClientset(), Options{
			Config: sandbox.Config{Kubernetes: sandbox.Kubernetes{NamespacePrefix: ptr(bad)}},
		}); err == nil {
			t.Errorf("newSubstrate accepted namespace_prefix %q, which is not a DNS-1123 label", bad)
		}
	}
}

// loopauthWithSubject is loopPrincipal with an explicit subject, for the
// assertion that a Pod name is scoped by subject and not by tenant alone.
func loopauthWithSubject(tenant event.TenantID, subject string) loopauth.Principal {
	return loopauth.Principal{Tenant: tenant, Subject: subject}
}
