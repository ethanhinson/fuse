package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolveWorkspace resolves the mount SOURCE and the in-sandbox working
// directory for one Exec.
//
// This is where ADR-0044's containment constraint is enforced, and the split it
// makes is the whole point:
//
//   - The mount source is ALWAYS the trusted root passed in as `root`. It is
//     not a function of workingDir, and there is no branch on which model
//     output can reach it. This is what makes "mount my home directory"
//     unwritable.
//   - The model-supplied workingDir is a SUBPATH REQUEST resolved against that
//     root. It moves the working directory and nothing else, so the worst a
//     hostile value can do is name a directory the sandbox was already going to
//     be able to see.
//   - Anything that does not resolve inside the root is REFUSED, not clamped.
//     Silently rewriting an escape to the root would run a command somewhere
//     the caller did not ask for, which is how "cd /etc && rm -rf ." becomes a
//     surprise in the repo.
//
// Note that the returned workdir is an IN-SANDBOX path. The substrate resolves
// it inside the sandbox's own namespace, so a symlink the model plants in the
// tree after this check can only redirect the working directory to another path
// inside the mount — there is no TOCTOU window between here and the mount that
// reaches the host. The host-side canonicalisation below is what closes the
// window that DOES exist: a symlink already in the tree pointing out of it.
//
// # Three parameters, one algorithm
//
// The root is a PARAMETER rather than a field read (change 0065). That is the
// entire per-tenant change to this algorithm: WHICH root is mounted is settled
// at Acquire, from the authenticated Principal.Tenant, and handed in already
// canonicalised (see resolveMountRoot). Everything below — the canonical
// comparison, EvalSymlinks, the filepath.Rel + ".." rejection, the
// non-directory refusal, the refusal to disclose host paths — is UNCHANGED and
// must stay that way: the containment algorithm is not reimplemented in order
// to be made tenant-aware, it is simply pointed at a narrower root.
//
// The mountPoint is likewise a PARAMETER rather than the containerWorkspace
// constant (change 0075). That is the entire change this function needed in
// order to serve a REMOTE substrate as well as the local container one: the
// container handler passes containerWorkspace, and the remote adapter passes the
// remote sandbox's own MountRoot(). It is a TRUSTED value in both cases —
// a constant or a substrate-reported root — and it is never derived from model
// output, so it cannot be used to widen what the workingDir check contains.
//
// This function is deliberately the ONLY implementation of the algorithm, and
// every substrate reaches it. Duplicating it per handler is forbidden: two
// copies would drift, and a drift here is a containment hole rather than a bug.
//
// Consequently a caller MUST pass an already-canonicalised root. Every call path
// does: the container handler's h.root is canonicalised at construction, and its
// per-tenant root at Acquire, through that same one function.
func resolveWorkspace(root string, workingDir string, mountPoint string) (mount string, workdir string, err error) {
	workingDir = strings.TrimSpace(workingDir)

	if root == "" {
		if workingDir != "" {
			// The one thing we must not do here is fall back to mounting the
			// model's path because we have no trusted one.
			return "", "", fmt.Errorf("%w: cannot place working_dir %q", ErrNoTrustedRoot, workingDir)
		}
		return "", mountPoint, nil
	}

	// THE DEFAULT, and the common case: no working_dir at all still mounts the
	// working tree. An unmounted sandbox is an empty box the agent cannot work
	// in (ADR-0044: "The working tree must be mounted in for the model to see
	// the repo it edits").
	if workingDir == "" {
		return root, mountPoint, nil
	}

	// A relative working_dir is relative to the workspace — the only root the
	// model has any business naming a path against.
	candidate := workingDir
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, candidate)
	}

	// Canonicalise before comparing. A prefix test against an uncanonicalised
	// path is defeated by "..", by a doubled separator, and by a symlink; both
	// sides here are fully resolved, so the comparison is between real paths.
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		// Includes "does not exist", which is a refusal rather than a silent
		// fallback to the root: the caller asked to run somewhere specific.
		// The host path is deliberately NOT echoed back — the sandbox never
		// discloses this host's directory layout (see containerWorkspace).
		return "", "", fmt.Errorf("%w: %q could not be resolved", ErrWorkingDirRefused, workingDir)
	}

	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", "", fmt.Errorf("%w: %q escapes it", ErrWorkingDirRefused, workingDir)
	}
	if info, err := os.Stat(resolved); err != nil || !info.IsDir() {
		// In-tree but not a directory. Refused here so the caller gets the
		// reason, rather than at the substrate, where it surfaces as an opaque
		// runtime failure of a sandbox that was already created.
		return "", "", fmt.Errorf("%w: %q is not a directory", ErrWorkingDirRefused, workingDir)
	}
	if rel == "." {
		return root, mountPoint, nil
	}
	return root, mountPoint + "/" + filepath.ToSlash(rel), nil
}
