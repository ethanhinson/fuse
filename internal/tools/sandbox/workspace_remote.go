package sandbox

import (
	"fmt"
	"path"
	"strings"
)

// containRemoteWorkingDir contains a model-supplied working_dir against the
// mount root of a sandbox on a filesystem FUSE DOES NOT HAVE.
//
// # Why this is not resolveWorkspace
//
// resolveWorkspace (workspace.go) is the containment algorithm for the LOCAL
// substrate, and it is correct there for a reason that does not travel: the
// trusted root it is handed is a real directory on fuse's own filesystem, bind-
// mounted into the container, so `filepath.EvalSymlinks` and `os.Stat` interrogate
// the very tree the sandbox will see. Canonicalising is what closes the one
// TOCTOU-shaped hole that exists locally — a symlink ALREADY in the tree pointing
// out of it.
//
// On a remote substrate the workspace is an emptyDir inside a Pod. fuse has no
// filesystem access to it at all. Pointing the host-canonicalising algorithm at a
// remote mount root therefore judges the WRONG SUBJECT, with two failure modes
// that are not hypothetical — they are the defect this function was written to
// repair:
//
//   - "/workspace" does not exist in fuse's own container, so EvalSymlinks fails
//     and EVERY non-empty working_dir is refused. The feature is inoperative
//     rather than unsafe.
//   - if fuse's image ever did carry a "/workspace", the containment decision —
//     ADR-0044's gate 4 — would be made against fuse's tree instead of the
//     sandbox's. A containment check on the wrong subject is worse than no check,
//     because it reads as one.
//
// So this is a purely LEXICAL check: `path.Clean` (slash paths — the in-sandbox
// path is POSIX regardless of what fuse is running on) plus a segment-boundary
// prefix test. No EvalSymlinks. No Stat. Nothing that consults any filesystem.
//
// # The trust ordering, unchanged
//
// The mount root is the substrate's own trusted answer (RemoteSandbox.MountRoot,
// snapshot at Acquire and re-asserted by the Pool). The workingDir is a SUBPATH
// REQUEST resolved against it, applied LAST, and it can never widen or replace the
// root — the learning `trusted-root-never-model-selectable`. An escape is REFUSED,
// never clamped to the root: silently rewriting an escape would run the command
// somewhere the caller did not ask for.
//
// # Residual risk, stated honestly
//
// A lexical check cannot see a SYMLINK inside the Pod. If the model plants
// /workspace/out -> / and then asks to run in /workspace/out, this function
// honours it and the in-Pod `cd --` follows the link to the Pod's own root.
//
// What that is worth, and what it is not:
//
//   - It does NOT cross the isolation boundary. The Pod is the containment unit;
//     the workspace is a per-Pod emptyDir, and a Pod is provisioned for exactly
//     one principal (change 0065's tenant scoping, carried onto this substrate).
//     Reaching the Pod's own "/" reaches the workload image plus that one
//     principal's own emptyDir — not another tenant's data, and not fuse's
//     filesystem.
//   - It is NOT closed here, and no claim is made that it is. Closing it properly
//     means resolving and validating the directory INSIDE the Pod — an in-Pod
//     realpath check before the cd — which is a substrate-side change and the
//     honest next step if the workload image ever stops being the trust boundary
//     it is today.
//   - Note also that a symlink is something the MODEL planted in its OWN
//     workspace during its own session. The local substrate's canonicalisation
//     defends against a symlink already present in a HOST tree fuse mounted in,
//     which is a materially different threat: there, following the link leaves
//     the mount and reaches the host.
//
// It returns the in-sandbox absolute working directory, or a refusal. On a
// refusal it returns the empty string, so a caller that ignored the error finds
// no directory to use.
func containRemoteWorkingDir(mountRoot string, workingDir string) (workdir string, err error) {
	workingDir = strings.TrimSpace(workingDir)

	// A substrate that reported no root — or a relative one, which is not a root
	// — has given us nothing to contain against. The one thing we must not do is
	// fall back to running wherever the model named because we have no trusted
	// answer.
	if mountRoot == "" || !strings.HasPrefix(mountRoot, "/") {
		return "", fmt.Errorf("%w: the substrate reported no in-sandbox workspace root, cannot place working_dir %q",
			ErrNoTrustedRoot, workingDir)
	}
	root := path.Clean(mountRoot)

	// A NUL cannot appear in a path the exec subresource will carry, and a value
	// containing one is a caller trying to truncate something downstream.
	if strings.ContainsRune(workingDir, 0) {
		return "", fmt.Errorf("%w: %q is not a usable path", ErrWorkingDirRefused, workingDir)
	}

	// THE DEFAULT, and the common case: no working_dir runs at the workspace
	// root itself.
	if workingDir == "" {
		return root, nil
	}

	// A relative working_dir is relative to the workspace — the only root the
	// model has any business naming a path against.
	candidate := workingDir
	if !strings.HasPrefix(candidate, "/") {
		candidate = root + "/" + candidate
	}

	// Clean BEFORE comparing. It is what collapses "..", doubled separators, and
	// trailing slashes, so the comparison below is between two normalised paths
	// rather than two spellings. Clean on an absolute path also discards any
	// leading ".." it cannot resolve, which is why the prefix test that follows is
	// the decision and not a formality.
	resolved := path.Clean(candidate)

	if resolved == root {
		return root, nil
	}
	// The separator is REQUIRED in the prefix test. Without it "/workspaceXXX"
	// passes as a subpath of "/workspace" — a sibling directory that merely shares
	// the root's text. A special case for a root of "/" because "/" + "/" is not
	// how that one spells its boundary.
	prefix := root + "/"
	if root == "/" {
		// A mount root of "/" narrows NOTHING, and this says so rather than
		// pretending otherwise: the Pod remains the isolation boundary, and a
		// substrate that declared the sandbox root as its workspace has declared no
		// narrowing within it. No substrate in this repo does (every one reports
		// "/workspace"), but a prefix of "//" would refuse every path including the
		// root itself, which is a worse answer than an honest one.
		prefix = "/"
	}
	if !strings.HasPrefix(resolved, prefix) {
		return "", fmt.Errorf("%w: %q escapes it", ErrWorkingDirRefused, workingDir)
	}
	return resolved, nil
}
