package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
)

// ErrNoTenantRoot reports that a principal's per-tenant mount root could not be
// resolved, so there is no host tree this principal's containers may see.
//
// It is DEGRADED-SAFE by construction: the only answer to it is to mount
// nothing. It is never a licence to substitute the process-wide trusted root, a
// shared parent, or any other tenant's tree — an unmounted container is a
// broken workspace, a wrongly-mounted one is a cross-tenant disclosure, and
// only one of those is recoverable.
var ErrNoTenantRoot = errors.New("sandbox: no per-tenant workspace root for this principal")

// TenantRoots resolves ONE principal's bind-mount source from its authenticated
// Principal.Tenant, by naming a directory under a parent the COMPOSITION ROOT
// declared (change 0065).
//
// # Why this type is concrete, and why the interface it satisfies is not exported
//
// Like WithEgressProxy's *Proxy, WithTenantRoots takes this concrete type
// rather than the unexported tenantRootSource interface, and deliberately. The
// value being supplied here IS the bind-mount source: a caller who could
// implement the interface could choose what host tree fuse mounts into a
// container a model drives, which is the exact hole this package exists to
// close. Handing the composition root a concrete resolver — whose only degree
// of freedom is WHICH parent directory the per-tenant trees live under — keeps
// host layout policy where it belongs (the composition root supplies the
// parent) while denying any caller the ability to answer "which tree does this
// tenant get" with arbitrary code.
//
// # The trust direction
//
// The tenant is read from loopauth.Principal.Tenant and from nothing else. That
// value is established at the authenticated Connect edge, long before this
// package is reached, and is fixed on the Runner at Acquire. It is never
// derived from `command`, from `working_dir`, or from any other tool argument:
// a tenant a model can name is a tenant a model can switch, and a switchable
// tenant is not an isolation boundary.
//
// # Layout
//
// Each tenant gets exactly one direct child of the declared parent, named by its
// sanitised tenant id. Siblings, never nested: sibling trees are mutually
// non-overlapping by construction, so "tenant A's root contains tenant B's" is
// unrepresentable rather than merely tested for. The parent itself is never
// returned — mounting it would hand every tenant every sibling, which is the
// pre-0065 behaviour this change exists to end.
//
// A TenantRoots is immutable after construction and safe for concurrent use.
type TenantRoots struct {
	// parent is the canonicalised host directory the per-tenant trees are
	// children of, resolved ONCE at construction. It is never returned as a
	// mount source itself.
	parent string

	// create records whether a tenant's tree may be materialised on first use.
	// An operator who provisions tenant directories out of band leaves this
	// off, and an unprovisioned tenant then resolves to nothing (degraded-safe)
	// rather than silently gaining a fresh empty tree.
	create bool
}

// NewTenantRoots returns a resolver mapping each tenant to its own child of
// parent.
//
// parent is canonicalised through the SAME resolveMountRoot every trusted root
// goes through, once, here — before any model has run. A parent that is not an
// existing directory yields a resolver that resolves NOTHING, which is the
// fail-closed direction: a bind-mount source the daemon has to invent is not a
// working tree, and would hand the model an empty box that merely LOOKS mounted.
//
// If create is true, a tenant's own subdirectory is created on first use with
// 0700 permissions — owner-only, because the whole point of the directory is
// that it is one tenant's and no other's. If false, a tenant whose directory
// does not already exist resolves to nothing.
func NewTenantRoots(parent string, create bool) *TenantRoots {
	return &TenantRoots{parent: resolveMountRoot(parent), create: create}
}

// Root returns the host directory p's containers may see, or an error.
//
// It satisfies the unexported tenantRootSource interface the container handler
// consumes. Errors are reported rather than swallowed so an Acquire fails
// LOUDLY — the same posture the egress socket takes — because "your workspace
// is mysteriously empty" is the same containment as an explicit refusal but an
// unreadable one.
func (t *TenantRoots) Root(p loopauth.Principal) (string, error) {
	if t == nil || t.parent == "" {
		// No usable parent was declared. Degraded-safe: no root at all.
		return "", fmt.Errorf("%w: no per-tenant workspace parent is configured", ErrNoTenantRoot)
	}

	name, ok := tenantDirName(p.Tenant)
	if !ok {
		// THE EMPTY / UNSAFE TENANT DECISION (change 0065).
		//
		// event.NormalizeTenant("") collapses an empty tenant to DefaultTenant
		// ("_default") at the storage layer. That collapse is deliberately NOT
		// applied here. Storage keying and filesystem isolation want opposite
		// things from an absent identity: storage wants every row to land
		// somewhere, whereas an unauthenticated or empty tenant sharing a
		// directory with whatever real tenant happens to be called "_default"
		// is precisely the cross-tenant disclosure this change closes. So an
		// empty tenant — and any tenant id that cannot be rendered as one safe
		// path segment — resolves to NO ROOT rather than to a shared one.
		//
		// The local single-tenant path does not depend on this: it configures
		// no resolver at all, and keeps the single trusted root unchanged (see
		// containerHandler.tenantRoots).
		return "", fmt.Errorf("%w: tenant %q is not a usable workspace identity", ErrNoTenantRoot, p.Tenant)
	}

	dir := filepath.Join(t.parent, name)
	if t.create {
		// 0700: owner-only. A per-tenant tree that another uid can read is not
		// an isolation boundary. MkdirAll is idempotent, so concurrent Acquires
		// for one tenant race harmlessly.
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("%w: create workspace for tenant %q: %w", ErrNoTenantRoot, p.Tenant, err)
		}
	}

	// Canonicalised here as well as by the caller. That is not redundant
	// belt-and-braces: it is what makes the containment assertion below
	// possible, since a path compared before its symlinks are resolved proves
	// nothing about where it actually lands.
	resolved := resolveMountRoot(dir)
	if resolved == "" {
		return "", fmt.Errorf("%w: tenant %q has no provisioned workspace", ErrNoTenantRoot, p.Tenant)
	}

	// THE STRUCTURAL CHECK. The resolved tree must be a strict descendant of
	// the declared parent, and never the parent itself. Sanitisation above
	// already makes ".." unrepresentable, but a SYMLINK planted at the tenant's
	// directory can still point anywhere on the host — including at another
	// tenant's tree, or at /. Refusing here rather than trusting the name is
	// the same canonicalise-then-compare discipline workspace() applies to
	// working_dir, applied one layer up to the root itself.
	rel, err := filepath.Rel(t.parent, resolved)
	if err != nil || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		// The host path is deliberately not echoed back; see containerWorkspace.
		return "", fmt.Errorf("%w: tenant %q resolves outside the workspace parent", ErrNoTenantRoot, p.Tenant)
	}

	return resolved, nil
}

// tenantDirName renders a tenant id as ONE safe path segment, reporting whether
// it could.
//
// It is an ALLOWLIST, not an escape: only characters that cannot mean anything
// to a path resolver survive, and anything else fails the whole id rather than
// being dropped or substituted. Dropping characters would let two distinct
// tenants collide on one directory — which is a cross-tenant disclosure arrived
// at by string manipulation — so a tenant id that is not already a safe segment
// gets no root at all.
//
// # Why uppercase is refused, and why it is refused rather than lowercased
//
// The allowlist is LOWERCASE-ONLY, and that is a security decision, not a
// stylistic one. Filesystems disagree about whether case distinguishes a name:
// APFS and HFS+ (macOS, the documented dev platform), NTFS, and any
// case-insensitive volume treat "Acme" and "acme" as ONE directory entry. An
// allowlist permitting both cases would therefore hand two DISTINCT tenants the
// same physical tree — and would do so silently, because every check downstream
// still sees two distinct strings: MkdirAll succeeds for both, filepath.Rel
// reports the two different segments "Acme" and "acme", so both pass Root's
// structural containment check and both -v mounts land on the same host tree.
// That is exactly the collision the paragraph above forbids, reached by a
// different route: not by manipulating the string, but by trusting a
// distinction the filesystem does not make. Refusing an uppercase id makes the
// colliding pair UNREPRESENTABLE rather than merely unlikely.
//
// Case-folding the id to lowercase would close the mount collision but IS the
// collision-by-string-manipulation this function exists to refuse: it would map
// the distinct tenants "Acme" and "acme" onto one directory deliberately, on
// every platform, including the case-sensitive ones where they are genuinely
// two tenants. Refusing keeps the file's posture — fail the whole id rather
// than substitute — and pushes the decision back to whoever mints tenant ids,
// where an uppercase id is a loud configuration error rather than a silent
// merge. Callers that want case-insensitive tenant names must normalise at the
// identity edge, where the two ids can still be recognised as one identity.
//
// The empty tenant fails here by construction, which is the empty-tenant
// decision documented at Root's call site.
func tenantDirName(tenant event.TenantID) (string, bool) {
	s := string(tenant)
	if s == "" || len(s) > 128 {
		return "", false
	}
	// "." and ".." are the two names that traverse; reject them explicitly
	// rather than relying on the character allowlist to have excluded ".".
	if s == "." || s == ".." {
		return "", false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		// Lowercase only: see the case-insensitive-filesystem reasoning above.
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.':
		default:
			return "", false
		}
	}
	return s, true
}
