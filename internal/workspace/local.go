package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ethanhinson/fuse/internal/event"
)

// LocalProvider is a Provider whose workspaces are directories under one parent:
// <parent>/<tenant>/<loop>, with the tenant and loop directories created 0700
// (owner-only, as TenantRoots does today). FS() is implemented on *os.Root (Go
// 1.24+), so escapes are refused by kernel path resolution inside the root: a
// "..", an absolute path, or a symlink that would leave the workspace all fail
// with ErrEscape.
//
// A LocalProvider is immutable after construction and safe for concurrent use.
type LocalProvider struct {
	// Parent is the host directory the per-(tenant, loop) trees are children
	// of. It is created on first Open (like TenantRoots.create=true).
	Parent string
}

// Open returns the workspace at <Parent>/<tenant>/<loop>, creating the tenant
// and loop directories (0700) on first open. It is idempotent: a second open
// returns a fresh Workspace over the same directory with the same contents.
func (p *LocalProvider) Open(ctx context.Context, tenant event.TenantID, loop event.LoopID) (Workspace, error) {
	segBase, ok := segDirName(string(tenant))
	if !ok {
		return nil, fmt.Errorf("workspace: tenant %q is not a usable workspace identity", tenant)
	}
	segLoop, ok := segDirName(string(loop))
	if !ok {
		return nil, fmt.Errorf("workspace: loop %q is not a usable workspace identity", loop)
	}
	dir := filepath.Join(p.Parent, segBase, segLoop)

	// 0700: owner-only, exactly as TenantRoots does. MkdirAll is idempotent, so
	// concurrent Opens for one workspace race harmlessly.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("workspace: create workspace dir: %w", err)
	}
	// Re-assert 0700 on both the loop dir and its tenant parent: MkdirAll leaves
	// a pre-existing directory's mode alone, and a tree provisioned wide out of
	// band must not stay wide.
	for _, d := range []string{dir, filepath.Dir(dir)} {
		if err := os.Chmod(d, 0o700); err != nil {
			return nil, fmt.Errorf("workspace: restrict workspace dir to owner-only: %w", err)
		}
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("workspace: open root %q: %w", dir, err)
	}
	// scratch/ exists after open (spec: every workspace starts with it).
	if err := os.MkdirAll(filepath.Join(dir, "scratch"), 0o700); err != nil {
		return nil, fmt.Errorf("workspace: create scratch dir: %w", err)
	}
	return &localWorkspace{id: ID(segBase + "/" + segLoop), root: root, dir: dir}, nil
}

// localWorkspace is the *os.Root-backed Workspace. MountSource returns the
// underlying directory (the container substrate bind-mounts it at /workspace);
// Release closes the root handle and, in this change, never deletes contents.
type localWorkspace struct {
	id   ID
	root *os.Root
	dir  string
}

func (w *localWorkspace) ID() ID { return w.id }

func (w *localWorkspace) FS() FileSystem { return &rootFS{root: w.root} }

func (w *localWorkspace) MountSource(ctx context.Context) (string, error) { return w.dir, nil }

func (w *localWorkspace) Release(ctx context.Context) error {
	if w.root != nil {
		return w.root.Close()
	}
	return nil
}

// rootFS adapts *os.Root's confined FS view to the FileSystem interface.
//
// Confinement comes from the kernel: Root.FS() returns an fs.FS whose path
// resolution cannot leave the root directory. NormalizeName rejects ".." and
// absolute paths before they reach the FS (returning ErrEscape); a symlink
// planted inside the workspace that resolves outside fails the Root's own
// resolution with "path escapes from parent", which escapeErr maps to ErrEscape.
type rootFS struct {
	root *os.Root
}

func (r *rootFS) Open(name string) (fs.File, error) {
	n, err := NormalizeName(name)
	if err != nil {
		return nil, err
	}
	f, err := r.root.FS().Open(n)
	if err != nil {
		return nil, escapeErr(n, err)
	}
	return f, nil
}

func (r *rootFS) Stat(name string) (fs.FileInfo, error) {
	n, err := NormalizeName(name)
	if err != nil {
		return nil, err
	}
	fi, err := fs.Stat(r.root.FS(), n)
	if err != nil {
		return nil, escapeErr(n, err)
	}
	return fi, nil
}

func (r *rootFS) ReadDir(name string) ([]fs.DirEntry, error) {
	n, err := NormalizeName(name)
	if err != nil {
		return nil, err
	}
	des, err := fs.ReadDir(r.root.FS(), n)
	if err != nil {
		return nil, escapeErr(n, err)
	}
	return des, nil
}

func (r *rootFS) WriteFile(name string, data []byte, perm fs.FileMode) error {
	n, err := NormalizeName(name)
	if err != nil {
		return err
	}
	if err := r.root.WriteFile(n, data, perm); err != nil {
		return escapeErr(n, err)
	}
	return nil
}

func (r *rootFS) MkdirAll(name string, perm fs.FileMode) error {
	n, err := NormalizeName(name)
	if err != nil {
		return err
	}
	if err := r.root.MkdirAll(n, perm); err != nil {
		return escapeErr(n, err)
	}
	return nil
}

func (r *rootFS) Rename(old, new string) error {
	o, err := NormalizeName(old)
	if err != nil {
		return err
	}
	n, err := NormalizeName(new)
	if err != nil {
		return err
	}
	if err := r.root.Rename(o, n); err != nil {
		return escapeErr(n, err)
	}
	return nil
}

func (r *rootFS) Remove(name string) error {
	n, err := NormalizeName(name)
	if err != nil {
		return err
	}
	if err := r.root.Remove(n); err != nil {
		return escapeErr(n, err)
	}
	return nil
}

func (r *rootFS) Walk(root string, fn fs.WalkDirFunc) error {
	n, err := NormalizeName(root)
	if err != nil {
		return err
	}
	// fs.WalkDir over the confined FS view: the walk is bounded by the same
	// kernel resolution, so a symlink escaping the root surfaces as an error
	// instead of following it.
	return fs.WalkDir(r.root.FS(), n, fn)
}

// escapeErr wraps an error from Root.FS() as ErrEscape when it is a path-escape
// refusal (the "path escapes from parent" refusal the kernel emits for a
// symlink pointing outside the root, or fs.ErrInvalid, which the FS adapter
// emits for ".." / absolute paths that NormalizeName did not already reject).
// Any other error — most importantly fs.ErrNotExist — passes through unchanged.
func escapeErr(name string, err error) error {
	if err == nil {
		return nil
	}
	if isEscapeErr(err) {
		return fmt.Errorf("%w: %q", ErrEscape, name)
	}
	return err
}

// isEscapeErr reports whether err is the Root's path-escape refusal.
func isEscapeErr(err error) bool {
	if errors.Is(err, fs.ErrInvalid) {
		return true
	}
	// "path escapes from parent" is the kernel's refusal for a symlink whose
	// target leaves the root. It is surfaced as the PathError.Err text; there is
	// no exported sentinel (Go's own os package matches this text internally).
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return strings.Contains(pe.Err.Error(), "path escapes from parent")
	}
	return false
}

// segDirName renders one identity component (tenant or loop) as a single safe
// path segment, reporting whether it could — the same allowlist
// sandbox.tenantDirName applies to tenants: lowercase letters, digits, "-",
// "_" and "."; no leading dot; not "." or ".."; non-empty; ≤128 bytes. The
// allowlist makes ".." and absolute escapes unrepresentable by construction,
// and the lowercase-only rule keeps case-insensitive filesystems from merging
// two distinct identities (a collision that would be a cross-tenant
// disclosure).
func segDirName(s string) (string, bool) {
	if s == "" || len(s) > 128 {
		return "", false
	}
	if s == "." || s == ".." {
		return "", false
	}
	if s[0] == '.' {
		return "", false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.':
		default:
			return "", false
		}
	}
	return s, true
}