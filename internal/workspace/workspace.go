// Package workspace gives every loop a confined filesystem: the file tools
// (read_file, write_file, edit_file, list_directory, grep) operate on a
// FileSystem view rooted at the loop's own directory, and the bash substrate
// bind-mounts that same directory at /workspace. A loop can therefore read and
// write exactly one tree — its own — and no path it can name reaches another
// loop's or another tenant's files.
//
// The confinement is enforced by construction: every FileSystem method takes
// workspace-relative names, and the implementations (os.Root on the local
// provider, fstest.MapFS on the memory provider) reject "..", absolute, and
// symlink escapes with ErrEscape. The loop's identity is recorded as a
// loop.workspace event on its durable stream so a resume on another instance
// re-opens the same directory.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/ethanhinson/fuse/internal/event"
)

// ID is a loop workspace's identity: "<tenant>/<loop>" by default. Callers
// treat it as opaque.
type ID string

// FileSystem is the confined filesystem the file tools operate on. Every path
// is workspace-relative; "..", absolute paths, and symlinks that resolve
// outside the workspace are rejected with ErrEscape. Implementations must be
// safe for concurrent use.
type FileSystem interface {
	Open(name string) (fs.File, error)
	Stat(name string) (fs.FileInfo, error)
	ReadDir(name string) ([]fs.DirEntry, error)
	WriteFile(name string, data []byte, perm fs.FileMode) error
	MkdirAll(name string, perm fs.FileMode) error
	Rename(old, new string) error
	Remove(name string) error
	Walk(root string, fn fs.WalkDirFunc) error
}

// Workspace is one loop's confined filesystem plus its identity and lifecycle.
type Workspace interface {
	// ID is the workspace identity, "<tenant>/<loop>" by default. It is opaque
	// to callers but stable across opens of the same workspace.
	ID() ID

	// FS returns the confined filesystem the file tools operate on.
	FS() FileSystem

	// MountSource returns the host path the bash substrate bind-mounts at
	// /workspace, or "" when the workspace cannot be mounted (the memory
	// provider). It is safe to call at any time and returns the same value for
	// the lifetime of the workspace.
	MountSource(ctx context.Context) (string, error)

	// Release is called from LoopTeardown. In this change it never deletes
	// workspace contents; the local provider's Release is a no-op.
	Release(ctx context.Context) error
}

// Provider opens a workspace for a (tenant, loop) pair. Open is idempotent:
// it returns the same files on every call. First open creates the workspace.
type Provider interface {
	Open(ctx context.Context, tenant event.TenantID, loop event.LoopID) (Workspace, error)
}

// ErrEscape is returned by every FileSystem method for a name that would leave
// the workspace: "..", an absolute path, or a symlink resolving outside it.
// File tools translate it into a tool result that reports the rejection; it
// never falls through to the host filesystem.
var ErrEscape = errors.New("workspace: path escapes the workspace")

// NormalizeName validates and normalizes one workspace-relative path.
//
// It rejects absolute paths and ".." traversal, and collapses "./", repeated
// slashes, and trailing slashes so that the path the FileSystem sees is the
// clean relative path the model intended. "." itself is preserved (the
// workspace root). It deliberately does NOT resolve symlinks — the underlying
// implementations (os.Root, fstest.MapFS) reject a symlink that resolves
// outside the workspace, which is the confinement boundary.
func NormalizeName(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%w: empty path", ErrEscape)
	}
	if name[0] == '/' {
		return "", fmt.Errorf("%w: absolute path %q", ErrEscape, name)
	}
	clean := cleanRel(name)
	// Reject any path whose cleaning leaves a ".." segment — a single ".."
	// or a "a/../../b" both escape the workspace root.
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/..") && (strings.HasPrefix(strings.SplitN(clean, "/..", 2)[1], "/") || strings.SplitN(clean, "/..", 2)[1] == "") {
		return "", fmt.Errorf("%w: path %q leaves the workspace", ErrEscape, name)
	}
	// Also reject ".." appearing as a complete interior segment.
	for _, seg := range strings.Split(clean, "/") {
		if seg == ".." {
			return "", fmt.Errorf("%w: path %q leaves the workspace", ErrEscape, name)
		}
	}
	return clean, nil
}

// cleanRel cleans a relative path without ever resolving ".", ".." or symlinks
// into a name that could traverse: it removes "./" prefixes, collapses
// duplicate slashes, and strips a trailing slash, but leaves any ".." segment
// in place for NormalizeName to reject.
func cleanRel(p string) string {
	if p == "" {
		return ""
	}
	// Strip a leading "./" or "./" runs and collapse duplicate slashes,
	// but preserve ".." segments for NormalizeName to reject. We do NOT call
	// path.Clean (which would collapse ".." and could mask an escape).
	segs := strings.Split(p, "/")
	out := make([]string, 0, len(segs))
	for _, s := range segs {
		switch {
		case s == "" || s == ".":
			// drop empty (from duplicate slashes) and "." segments
			if s == "." && len(out) == 0 && len(segs) == 1 {
				// preserve a lone "." meaning the workspace root
				out = append(out, ".")
			}
		default:
			out = append(out, s)
		}
	}
	clean := strings.Join(out, "/")
	// Strip a trailing slash if one survived.
	if len(clean) > 1 && clean[len(clean)-1] == '/' {
		clean = clean[:len(clean)-1]
	}
	return clean
}