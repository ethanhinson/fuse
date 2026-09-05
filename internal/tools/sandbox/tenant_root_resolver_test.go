package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
)

// These tests cover the EXPORTED half of change 0065's seam — the concrete
// TenantRoots resolver a composition root supplies through WithTenantRoots —
// which tenant_root_test.go deliberately does not exercise (it drives the
// handler through a map double, so it proves the handler's behaviour and
// nothing about the resolver's).
//
// The distinction matters because these two files fail for different reasons: a
// bug here is a resolver that hands out the wrong tree, and a bug there is a
// handler that ignores the tree it was handed.

// THE HEADLINE PROPERTY of the resolver itself: distinct tenants get distinct,
// mutually non-overlapping sibling trees, and neither is the parent.
func TestTenantRootsResolverGivesEachTenantASiblingTree(t *testing.T) {
	parent := trustedTestRoot(t)
	src := NewTenantRoots(parent, true)

	a, err := src.Root(principal("tenant-a", "alice"))
	if err != nil {
		t.Fatalf("Root(tenant-a): %v", err)
	}
	b, err := src.Root(principal("tenant-b", "bob"))
	if err != nil {
		t.Fatalf("Root(tenant-b): %v", err)
	}

	if a == b {
		t.Fatalf("both tenants resolved to %q", a)
	}
	if a == parent || b == parent {
		t.Fatalf("a tenant resolved to the shared parent %q (a=%q b=%q)", parent, a, b)
	}
	// Siblings, so neither contains the other. This is the property that makes
	// cross-tenant containment structural rather than merely checked.
	assertDisjoint(t, a, b)
	assertDisjoint(t, b, a)

	// Stable across calls: a Runner re-acquired for one tenant must land in the
	// same tree, not a fresh one.
	again, err := src.Root(principal("tenant-a", "someone-else"))
	if err != nil {
		t.Fatalf("Root(tenant-a) again: %v", err)
	}
	if again != a {
		t.Fatalf("tenant-a resolved to %q then %q; the mapping must be stable", a, again)
	}
}

// Only Principal.Tenant selects the tree. Two principals of one tenant share
// it; the Subject must not shard it, or the tenant boundary would silently
// become a per-user one and the shared-workspace assumption would break.
func TestTenantRootsResolverKeysOnTenantNotSubject(t *testing.T) {
	src := NewTenantRoots(trustedTestRoot(t), true)

	first, err := src.Root(principal("acme", "alice"))
	if err != nil {
		t.Fatalf("Root(alice): %v", err)
	}
	second, err := src.Root(principal("acme", "bob"))
	if err != nil {
		t.Fatalf("Root(bob): %v", err)
	}
	if first != second {
		t.Fatalf("same tenant resolved to %q and %q; only Tenant may select the tree", first, second)
	}
}

// THE EMPTY-TENANT DECISION, asserted rather than merely documented. An empty
// tenant must NOT collapse to event.DefaultTenant's directory the way storage
// keying does: an unauthenticated identity sharing a tree with whatever real
// tenant is called "_default" is the disclosure this change closes.
func TestTenantRootsResolverRefusesUnsafeTenantIdentities(t *testing.T) {
	parent := trustedTestRoot(t)
	src := NewTenantRoots(parent, true)

	// A real tenant that happens to be named like the storage default. If the
	// empty tenant collapsed, it would land here.
	defaultNamed, err := src.Root(loopauth.Principal{Tenant: event.DefaultTenant, Subject: "real"})
	if err != nil {
		t.Fatalf("Root(%q): %v", event.DefaultTenant, err)
	}

	unsafe := map[string]event.TenantID{
		"empty":               "",
		"dot":                 ".",
		"dotdot":              "..",
		"traversal":           "../tenant-b",
		"absolute":            "/etc",
		"separator":           "a/b",
		"null byte":           "a\x00b",
		"space":               "a b",
		"backslash traversal": `..\tenant-b`,

		// THE CASE-COLLISION PAIR. On a case-insensitive filesystem (APFS,
		// HFS+, NTFS) "Acme" and "acme" name ONE directory, so permitting
		// uppercase would silently merge two distinct tenants onto one host
		// tree while filepath.Rel still reports two distinct segments — both
		// pass the structural check, both get mounted, and the suite stays
		// green. An uppercase id is therefore refused outright, making the
		// colliding pair unrepresentable rather than merely unlikely.
		"uppercase":      "Acme",
		"mixed case":     "aCme",
		"uppercase only": "ACME",
		"uppercase tail": "acmE",

		// DOT-LEADING AND ALL-DOT IDS. "." and ".." are refused explicitly
		// above; "..." and longer runs are legal, non-traversing directory
		// names, so nothing escapes containment — but they are still an
		// oddity for a value that becomes a host directory name, and a
		// dot-leading tree is invisible to a casual `ls` of the parent,
		// which works against the operator legibility the layout policy is
		// otherwise careful about. Refusing the leading dot takes the whole
		// family at once. It stops there: "_default" is a real identity (see
		// the resolve assertion above), so the rule is NOT "alphanumeric
		// first".
		"three dots":  "...",
		"four dots":   "....",
		"many dots":   ".....",
		"leading dot": ".acme",
		"hidden tree": ".hidden-tenant",
	}
	for name, tenant := range unsafe {
		t.Run(name, func(t *testing.T) {
			got, err := src.Root(loopauth.Principal{Tenant: tenant, Subject: "s"})
			if err == nil {
				t.Fatalf("tenant %q resolved to %q; unsafe identities must get no root", tenant, got)
			}
			if !errors.Is(err, ErrNoTenantRoot) {
				t.Fatalf("error = %v, want it to wrap ErrNoTenantRoot", err)
			}
			if got != "" {
				t.Fatalf("a refused tenant still returned the root %q", got)
			}
			if got == defaultNamed {
				t.Fatalf("tenant %q collapsed onto the %q tree", tenant, event.DefaultTenant)
			}
			// Nothing was created for it either: a refused identity must not
			// leave a directory behind that a later, laxer check could accept.
			if entries, err := os.ReadDir(parent); err == nil {
				for _, e := range entries {
					if e.Name() != string(event.DefaultTenant) {
						t.Fatalf("refused tenant %q created %q under the parent", tenant, e.Name())
					}
				}
			}
		})
	}
}

// The case-collision property stated directly, as a PAIR rather than as two
// independent refusals: whatever else changes about the allowlist, a tenant id
// and its case-variant must never both resolve, because on a case-insensitive
// volume "both resolve" means "both resolve to the same tree".
func TestTenantRootsResolverRefusesCaseVariantTenantCollisions(t *testing.T) {
	parent := trustedTestRoot(t)
	src := NewTenantRoots(parent, true)

	lower, err := src.Root(principal("acme", "s"))
	if err != nil {
		t.Fatalf("Root(acme): %v", err)
	}
	upper, uerr := src.Root(principal("Acme", "s"))
	if uerr == nil {
		t.Fatalf("tenant %q resolved to %q (lowercase sibling %q); on a "+
			"case-insensitive filesystem these are ONE directory, so the "+
			"uppercase id must be refused rather than mounted", "Acme", upper, lower)
	}
	if !errors.Is(uerr, ErrNoTenantRoot) {
		t.Fatalf("error = %v, want it to wrap ErrNoTenantRoot", uerr)
	}
	if upper != "" {
		t.Fatalf("a refused tenant still returned the root %q", upper)
	}
}

// A SYMLINK planted where a tenant's directory belongs can point anywhere on
// the host, including at another tenant's tree. The resolver canonicalises and
// then compares — the same discipline workspace() applies to working_dir — so
// the escape is refused rather than mounted.
func TestTenantRootsResolverRefusesASymlinkedTenantDirectory(t *testing.T) {
	parent := trustedTestRoot(t)
	outside := trustedTestRoot(t)
	if err := os.Symlink(outside, filepath.Join(parent, "evil")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	// create=false: the symlink is already in place, standing in for one an
	// attacker (or a careless provisioning script) planted.
	src := NewTenantRoots(parent, false)

	got, err := src.Root(principal("evil", "s"))
	if err == nil {
		t.Fatalf("a symlinked tenant directory resolved to %q, escaping the parent", got)
	}
	if !errors.Is(err, ErrNoTenantRoot) {
		t.Fatalf("error = %v, want it to wrap ErrNoTenantRoot", err)
	}
	if got != "" {
		t.Fatalf("a refused tenant still returned the root %q", got)
	}
}

// Without create, an unprovisioned tenant resolves to NOTHING rather than
// silently gaining a fresh empty tree. This is the operator-provisions-out-of-band
// posture, and its failure mode must be degraded, never widened.
func TestTenantRootsResolverWithoutCreateRefusesUnprovisionedTenants(t *testing.T) {
	parent := trustedTestRoot(t)
	provisioned := filepath.Join(parent, "known")
	if err := os.MkdirAll(provisioned, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	src := NewTenantRoots(parent, false)

	if _, err := src.Root(principal("known", "s")); err != nil {
		t.Fatalf("Root(known): %v", err)
	}
	got, err := src.Root(principal("unknown", "s"))
	if err == nil {
		t.Fatalf("an unprovisioned tenant resolved to %q", got)
	}
	if !errors.Is(err, ErrNoTenantRoot) {
		t.Fatalf("error = %v, want it to wrap ErrNoTenantRoot", err)
	}
}

// A parent that is not an existing directory yields a resolver that resolves
// nothing at all — the fail-closed direction. It must never fall through to
// creating one, or to some ambient default.
func TestTenantRootsResolverWithNoUsableParentResolvesNothing(t *testing.T) {
	for name, parent := range map[string]string{
		"empty":           "",
		"does not exist":  "/nonexistent/fuse/tenant/parent",
		"whitespace":      "   ",
		"not a directory": tenantRootsRegularFile(t),
	} {
		t.Run(name, func(t *testing.T) {
			src := NewTenantRoots(parent, true)
			got, err := src.Root(principal("acme", "s"))
			if err == nil {
				t.Fatalf("resolver with parent %q handed out %q", parent, got)
			}
			if !errors.Is(err, ErrNoTenantRoot) {
				t.Fatalf("error = %v, want it to wrap ErrNoTenantRoot", err)
			}
		})
	}
}

func tenantRootsRegularFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(trustedTestRoot(t), "a-file")
	if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// LOCAL PARITY, asserted at the Service seam. With no WithTenantRoots declared,
// the container handler must hold NO resolver at all — not a non-nil interface
// wrapping a nil *TenantRoots, which would read as "configured" at Acquire and
// turn every resolver-less deployment into one that mounts nothing.
//
// This is the nil-interface trap WithEgressProxy already documents, asserted
// here rather than left to inference because its failure mode is silent.
func TestServiceWithoutTenantRootsLeavesTheHandlerResolverless(t *testing.T) {
	for name, opts := range map[string][]ServiceOption{
		"no option at all":      nil,
		"explicit nil resolver": {WithTenantRoots(nil)},
	} {
		t.Run(name, func(t *testing.T) {
			h := tenantRootsHandlerFromService(t, opts...)
			if h.tenantRoots != nil {
				t.Fatalf("handler holds a resolver (%#v) with none declared; "+
					"the single trusted root must remain the default", h.tenantRoots)
			}
		})
	}
}

// And the positive: a declared resolver reaches the handler.
func TestServiceWithTenantRootsReachesTheHandler(t *testing.T) {
	src := NewTenantRoots(trustedTestRoot(t), true)
	h := tenantRootsHandlerFromService(t, WithTenantRoots(src))
	if h.tenantRoots == nil {
		t.Fatalf("a declared resolver never reached the handler")
	}
}

// tenantRootsHandlerFromService builds a Service with the container substrate
// selected and returns its container handler, so a test can assert on what the
// composition-root options actually installed.
func tenantRootsHandlerFromService(t *testing.T, opts ...ServiceOption) *containerHandler {
	t.Helper()
	base := []ServiceOption{
		WithTrustedRoot(trustedTestRoot(t)),
		withContainerLookPath(fakeLookPath("docker")),
		withContainerExec(func(context.Context, string, ...string) ([]byte, int, error) {
			return nil, 0, nil
		}),
	}
	svc, err := NewService(DefaultConfig(), append(base, opts...)...)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	h, ok := svc.handler.(*containerHandler)
	if !ok {
		t.Fatalf("selected handler is %T, want *containerHandler", svc.handler)
	}
	return h
}

// A PRE-EXISTING tenant tree is re-restricted to 0700, not merely left as
// found. This is the same hazard hostedWorkspaceParent guards one level up:
// MkdirAll leaves an existing directory's mode alone, so a tenant tree created
// 0755 by an earlier fuse build, restored from a backup, or provisioned out of
// band would otherwise keep a mode that lets any uid on the host read that
// tenant's files. The parent being 0700 mitigates that but does not close it —
// same-uid processes still read it, and the mitigation evaporates the moment
// the parent's mode changes.
func TestTenantRootsResolverReassertsOwnerOnlyOnExistingTenantTree(t *testing.T) {
	parent := trustedTestRoot(t)
	// Provisioned out of band, world-readable — the exact shape the guard is for.
	existing := filepath.Join(parent, "acme")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatalf("Mkdir(%q): %v", existing, err)
	}
	if err := os.Chmod(existing, 0o755); err != nil { // defeat any umask narrowing
		t.Fatalf("Chmod(%q): %v", existing, err)
	}

	got, err := NewTenantRoots(parent, true).Root(principal("acme", "alice"))
	if err != nil {
		t.Fatalf("Root(acme): %v", err)
	}

	info, err := os.Stat(got)
	if err != nil {
		t.Fatalf("Stat(%q): %v", got, err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("pre-existing tenant tree %q has mode %#o, want 0700; a tree "+
			"another uid can read is not an isolation boundary", got, perm)
	}
}
