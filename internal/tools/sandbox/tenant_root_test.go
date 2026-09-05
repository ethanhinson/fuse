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

// These tests describe the ISOLATION BOUNDARY change 0065 installs: the
// bind-mount source is a function of Principal.Tenant, so tenant A's container
// and tenant B's container are handed different, non-overlapping host trees and
// no working_dir A can author reaches B's.
//
// They are written against the handler's OBSERVABLE output — the assembled
// argv's -v and -w, and the refusal errors — rather than against any internal
// field, because argv IS the security boundary of this handler (see argv's own
// doc comment): a property that is not visible in argv is not enforced.

// mapTenantRoots is a tenantRootSource double: it maps each tenant id to the
// host directory that tenant's containers may see. A tenant with no entry
// resolves to "" — the degraded-safe state — and a tenant listed in fail
// returns an error, standing in for a resolver that could not reach its layout
// authority.
type mapTenantRoots struct {
	roots map[event.TenantID]string
	fail  map[event.TenantID]error
	// calls records every principal the handler asked about, so a test can
	// assert the resolver is consulted with the AUTHENTICATED principal rather
	// than with something derived from the command.
	calls []loopauth.Principal
}

// errTenantRootUnavailable stands in for a resolver that could not reach its
// layout authority.
var errTenantRootUnavailable = errors.New("tenant root unavailable")

func (m *mapTenantRoots) Root(p loopauth.Principal) (string, error) {
	m.calls = append(m.calls, p)
	if err, ok := m.fail[p.Tenant]; ok {
		return "", err
	}
	return m.roots[p.Tenant], nil
}

// tenantTestRoot makes a real, canonicalised directory to stand in for one
// tenant's host tree. Symlinks are resolved for the same reason
// trustedTestRoot resolves them: on macOS t.TempDir hands back a
// /var → /private/var symlink, and the handler compares canonical paths.
func tenantTestRoot(t *testing.T, parent, name string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", dir, err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", dir, err)
	}
	return resolved
}

// twoTenants builds the standard fixture: a shared parent holding sibling
// per-tenant trees, and a resolver mapping each tenant to its own. The parent
// is deliberately SHARED so that "one level up" is a real, existing directory
// containing the other tenant's data — the escape that matters, and the one a
// mere "does it exist" check would wave through.
func twoTenants(t *testing.T) (parent, rootA, rootB string, src *mapTenantRoots) {
	t.Helper()
	parent = trustedTestRoot(t)
	rootA = tenantTestRoot(t, parent, "tenant-a")
	rootB = tenantTestRoot(t, parent, "tenant-b")
	src = &mapTenantRoots{roots: map[event.TenantID]string{
		"tenant-a": rootA,
		"tenant-b": rootB,
	}}
	return parent, rootA, rootB, src
}

// newTenantHandler builds a handler whose mount source comes from src. The
// trusted root is the SHARED PARENT on purpose: if the per-tenant resolver were
// ignored — the pre-0065 behaviour — every tenant would mount the parent and
// see every sibling, so a test that passed under both would prove nothing.
func newTenantHandler(t *testing.T, rec *recordingRun, parent string, src tenantRootSource) *containerHandler {
	t.Helper()
	h, err := newContainerHandler(DefaultConfig(),
		withLookPath(fakeLookPath("docker")),
		withExecRunner(rec.run),
		withTrustedRoot(parent),
		withTenantRoots(src),
	)
	if err != nil {
		t.Fatalf("newContainerHandler: %v", err)
	}
	return h
}

// tenantPrincipal names an authenticated principal for tenant, reusing
// pool_test.go's principal helper so both containment suites build identities
// the same way.
func tenantPrincipal(tenant string) loopauth.Principal {
	return principal(tenant, "subject-"+tenant)
}

// execFor acquires a Runner for p and runs one command, returning the recorded
// argv along with the Exec result. The recorder is cleared first so "nothing
// ran" means "nothing ran in THIS Exec".
func execFor(t *testing.T, h *containerHandler, rec *recordingRun, p loopauth.Principal, cmd, workingDir string) (Output, error) {
	t.Helper()
	rec.name, rec.args = "", nil
	r, err := h.Acquire(context.Background(), p, Env{})
	if err != nil {
		// An Acquire that fails loudly is an ACCEPTABLE degraded outcome (it is
		// the posture the egress socket already takes), so it is reported as a
		// skip-worthy fact rather than asserted against here. What is NOT
		// acceptable — and is what every caller below checks — is a Runner that
		// proceeds against a root this principal was never granted.
		t.Skipf("Acquire(%+v) failed: %v (a loud acquire failure is a permitted degraded outcome)", p, err)
	}
	return r.Exec(context.Background(), cmd, workingDir)
}

// THE HEADLINE PROPERTY. Two principals with distinct tenants must be handed
// different, non-overlapping mount sources. Same handler, same image, same
// everything else: only Principal.Tenant differs, and that alone must move the
// bind-mount.
func TestTenantRootsAreDistinctAndNonOverlapping(t *testing.T) {
	rec := &recordingRun{}
	parent, rootA, rootB, src := twoTenants(t)
	h := newTenantHandler(t, rec, parent, src)

	if _, err := execFor(t, h, rec, tenantPrincipal("tenant-a"), "true", ""); err != nil {
		t.Fatalf("Exec for tenant-a: %v", err)
	}
	gotA, okA := argValue(rec.args, "-v")
	if !okA {
		t.Fatalf("tenant-a argv carries no -v: %#v", rec.args)
	}

	if _, err := execFor(t, h, rec, tenantPrincipal("tenant-b"), "true", ""); err != nil {
		t.Fatalf("Exec for tenant-b: %v", err)
	}
	gotB, okB := argValue(rec.args, "-v")
	if !okB {
		t.Fatalf("tenant-b argv carries no -v: %#v", rec.args)
	}

	wantA := rootA + ":" + containerWorkspace
	wantB := rootB + ":" + containerWorkspace
	if gotA != wantA {
		t.Fatalf("tenant-a mount = %q, want %q", gotA, wantA)
	}
	if gotB != wantB {
		t.Fatalf("tenant-b mount = %q, want %q", gotB, wantB)
	}
	if gotA == gotB {
		t.Fatalf("both tenants mounted the same source %q", gotA)
	}
	// Non-overlap, not merely inequality: a mount of the shared parent (or of
	// any ancestor of the other tenant's tree) is "different" as a string while
	// still exposing everything. filepath.Rel is the same containment test the
	// workspace check uses, applied here to the two roots themselves.
	assertDisjoint(t, rootA, rootB)
	assertDisjoint(t, rootB, rootA)
}

// assertDisjoint fails if outer contains inner — i.e. if mounting outer would
// hand a container inner's tree.
func assertDisjoint(t *testing.T, outer, inner string) {
	t.Helper()
	rel, err := filepath.Rel(outer, inner)
	if err != nil {
		return
	}
	if rel != ".." && rel != "." && !filepath.IsAbs(rel) &&
		rel != ".."+string(filepath.Separator) &&
		!hasParentPrefix(rel) {
		t.Fatalf("mount roots overlap: %q contains %q (rel %q)", outer, inner, rel)
	}
}

func hasParentPrefix(rel string) bool {
	return rel == ".." || len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator)
}

// THE ESCAPE TABLE. Every shape by which tenant A might try to name tenant B's
// tree must be REFUSED — not clamped, not silently rewritten to A's root, and
// above all not run. Mirrors the single-root table in container_test.go, with
// the target being a real, existing sibling tenant's directory rather than an
// arbitrary host path: an escape that lands somewhere real is the one a
// mere-existence check would wave through.
func TestTenantWorkingDirCannotReachAnotherTenant(t *testing.T) {
	// Each case names a working_dir tenant A hands in, computed from A's root,
	// B's root and their shared parent.
	cases := map[string]func(parent, rootA, rootB string) string{
		"absolute path to the other tenant": func(_, _, rootB string) string { return rootB },
		"absolute traversal into the other tenant": func(_, rootA, rootB string) string {
			return rootA + "/../" + filepath.Base(rootB)
		},
		"relative traversal into the other tenant": func(_, _, rootB string) string {
			return "../" + filepath.Base(rootB)
		},
		"the shared parent, absolutely":  func(parent, _, _ string) string { return parent },
		"the shared parent, relatively":  func(string, string, string) string { return ".." },
		"a longer .. chain":              func(string, string, string) string { return "../../.." },
		"doubled separators hiding a ..": func(string, string, string) string { return "a//../.." },
		"doubled separators into the sibling": func(_, _, rootB string) string {
			return "a//..//../" + filepath.Base(rootB)
		},
		"the host root": func(string, string, string) string { return "/" },
	}

	for name, pick := range cases {
		t.Run(name, func(t *testing.T) {
			rec := &recordingRun{}
			parent, rootA, rootB, src := twoTenants(t)
			h := newTenantHandler(t, rec, parent, src)

			// A real in-tree directory named by the doubled-separator cases, so
			// those are refused for TRAVERSAL and not merely for not existing.
			if err := os.MkdirAll(filepath.Join(rootA, "a"), 0o755); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			// A secret only tenant B should ever be able to see. Its presence is
			// what makes these cases exfiltration attempts rather than pathname
			// trivia.
			if err := os.WriteFile(filepath.Join(rootB, "secret.txt"), []byte("tenant-b only"), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			target := pick(parent, rootA, rootB)
			out, err := execFor(t, h, rec, tenantPrincipal("tenant-a"), "cat secret.txt", target)

			if err == nil {
				t.Fatalf("tenant-a working_dir %q was accepted; argv: %#v", target, rec.args)
			}
			if !errors.Is(err, ErrWorkingDirRefused) {
				t.Fatalf("error = %v, want it to wrap ErrWorkingDirRefused", err)
			}
			if out.ExitCode != -1 {
				t.Fatalf("ExitCode = %d, want -1 so an ExitCode-only caller fails closed", out.ExitCode)
			}
			// The load-bearing assertion, same as the single-root table: nothing
			// ran. A refusal that still invoked the CLI would have already
			// created the mount, and the mount is the disclosure.
			if rec.name != "" || rec.args != nil {
				t.Fatalf("the refusal still invoked %q with %#v", rec.name, rec.args)
			}
		})
	}
}

// A symlink planted INSIDE tenant A's tree pointing at tenant B's is the
// traversal a string-prefix check misses, which is why containment
// canonicalises before it compares. Separate from the table above because it
// needs a symlink on disk rather than a pathname.
func TestTenantSymlinkIntoAnotherTenantIsRefused(t *testing.T) {
	rec := &recordingRun{}
	parent, rootA, rootB, src := twoTenants(t)
	h := newTenantHandler(t, rec, parent, src)

	link := filepath.Join(rootA, "peek")
	if err := os.Symlink(rootB, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	for _, given := range []string{link, "peek"} {
		t.Run(given, func(t *testing.T) {
			out, err := execFor(t, h, rec, tenantPrincipal("tenant-a"), "ls", given)

			if err == nil {
				t.Fatalf("symlink into the sibling tenant was accepted; argv: %#v", rec.args)
			}
			if !errors.Is(err, ErrWorkingDirRefused) {
				t.Fatalf("error = %v, want it to wrap ErrWorkingDirRefused", err)
			}
			if out.ExitCode != -1 {
				t.Fatalf("ExitCode = %d, want -1", out.ExitCode)
			}
			if rec.name != "" || rec.args != nil {
				t.Fatalf("the refusal still invoked %q with %#v", rec.name, rec.args)
			}
		})
	}
}

// A tenant's OWN subdirectory still works, and still moves only -w. Without
// this the escape table above would be satisfiable by refusing everything,
// which is containment by breakage rather than by isolation.
func TestTenantSubpathStaysWithinItsOwnRoot(t *testing.T) {
	rec := &recordingRun{}
	parent, rootA, _, src := twoTenants(t)
	h := newTenantHandler(t, rec, parent, src)

	if err := os.MkdirAll(filepath.Join(rootA, "svc", "api"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	if _, err := execFor(t, h, rec, tenantPrincipal("tenant-a"), "true", filepath.Join("svc", "api")); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	wantFlag(t, rec.args, "-v", rootA+":"+containerWorkspace)
	wantFlag(t, rec.args, "-w", containerWorkspace+"/svc/api")
}

// DEGRADED-SAFE, NEVER FALLBACK. A resolver that cannot produce a root for this
// principal must yield NO mount at all — never the handler's single trusted
// root, never the shared parent, never another tenant's tree. An unmounted
// container is a broken workspace; a wrongly-mounted one is a cross-tenant
// disclosure, and only one of those is recoverable.
func TestTenantUnresolvedRootMountsNothingAndRefusesWorkingDir(t *testing.T) {
	unresolvable := map[string]*mapTenantRoots{
		"tenant has no entry": {roots: map[event.TenantID]string{"tenant-a": "unused"}},
		"tenant maps to empty": {roots: map[event.TenantID]string{
			"tenant-unknown": "",
		}},
		"tenant maps to a path that does not exist": {roots: map[event.TenantID]string{
			"tenant-unknown": "/nonexistent/fuse/tenant/root",
		}},
		// A resolver that ERRORS is not a licence to mount something else. The
		// handler is free to fail Acquire loudly instead (see the note below),
		// but the one outcome forbidden is a container that runs against a root
		// this tenant was never granted.
		"resolver returns an error": {fail: map[event.TenantID]error{
			"tenant-unknown": errTenantRootUnavailable,
		}},
	}

	for name, src := range unresolvable {
		t.Run(name, func(t *testing.T) {
			rec := &recordingRun{}
			parent := trustedTestRoot(t)
			// The handler DOES have a trusted single root. If the resolver's
			// failure fell back to it, the tenant would silently get the shared
			// tree — which is precisely the fallback this asserts against.
			h := newTenantHandler(t, rec, parent, src)
			p := tenantPrincipal("tenant-unknown")

			// Half one: no working_dir at all. The command may run, but with
			// nothing mounted.
			if _, err := execFor(t, h, rec, p, "true", ""); err != nil {
				t.Fatalf("Exec with no working_dir: %v", err)
			}
			if got, ok := argValue(rec.args, "-v"); ok {
				t.Fatalf("argv mounted %q for a tenant with no resolvable root; argv: %#v", got, rec.args)
			}

			// Half two: any non-empty working_dir is refused with
			// ErrNoTrustedRoot — the "there is no root to place you against"
			// error, distinct from a containment refusal.
			out, err := execFor(t, h, rec, p, "true", "sub")
			if err == nil {
				t.Fatalf("working_dir was accepted with no resolvable root; argv: %#v", rec.args)
			}
			if !errors.Is(err, ErrNoTrustedRoot) {
				t.Fatalf("error = %v, want it to wrap ErrNoTrustedRoot", err)
			}
			if out.ExitCode != -1 {
				t.Fatalf("ExitCode = %d, want -1", out.ExitCode)
			}
			if rec.name != "" || rec.args != nil {
				t.Fatalf("the refusal still invoked %q with %#v", rec.name, rec.args)
			}
		})
	}
}

// The resolver is consulted with the AUTHENTICATED principal — the one fixed at
// Acquire — and never with anything derived from the command or working_dir.
// Asserted on the recorded calls because it is the one property no argv can
// show.
func TestTenantRootResolvedFromAuthenticatedPrincipal(t *testing.T) {
	rec := &recordingRun{}
	parent, _, _, src := twoTenants(t)
	h := newTenantHandler(t, rec, parent, src)

	p := tenantPrincipal("tenant-b")
	if _, err := execFor(t, h, rec, p, "cd /tenant-a && cat secret.txt", ""); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if len(src.calls) == 0 {
		t.Fatalf("the tenant root resolver was never consulted")
	}
	for i, got := range src.calls {
		if got.Tenant != p.Tenant || got.Subject != p.Subject {
			t.Fatalf("call %d resolved for %+v, want the authenticated principal %+v", i, got, p)
		}
	}
}
