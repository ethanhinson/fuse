package workspace

import (
	"context"
	"fmt"
	"io/fs"
	"sync"
	"testing/fstest"

	"github.com/ethanhinson/fuse/internal/event"
)

// MemProvider is an in-memory Provider for tests, backed by a read/write
// fstest.MapFS. Each (tenant, loop) pair gets its own MapFS; a second Open of
// the same pair returns a workspace over the same MapFS (same files). It is the
// conformance-suite counterpart to LocalProvider: it exercises the same
// FileSystem contract without touching the disk.
type MemProvider struct {
	mu   sync.Mutex
	open map[ID]*memWorkspace
}

// NewMemProvider returns an empty MemProvider.
func NewMemProvider() *MemProvider {
	return &MemProvider{open: map[ID]*memWorkspace{}}
}

// Open returns the (tenant, loop) workspace, creating its MapFS on first open.
// Idempotent: a second open returns a workspace over the same MapFS.
func (p *MemProvider) Open(ctx context.Context, tenant event.TenantID, loop event.LoopID) (Workspace, error) {
	id := ID(string(tenant) + "/" + string(loop))
	p.mu.Lock()
	defer p.mu.Unlock()
	if w, ok := p.open[id]; ok {
		return w, nil
	}
	m := &fstest.MapFS{}
	// scratch/ exists after open (spec: every workspace starts with it).
	(*m)["scratch"] = &fstest.MapFile{Mode: fs.ModeDir | 0o700}
	w := &memWorkspace{id: id, fsys: m}
	p.open[id] = w
	return w, nil
}

// memWorkspace is the fstest.MapFS-backed Workspace.
type memWorkspace struct {
	id   ID
	fsys *fstest.MapFS
}

func (w *memWorkspace) ID() ID { return w.id }

func (w *memWorkspace) FS() FileSystem { return &memFS{fsys: w.fsys} }

func (w *memWorkspace) MountSource(ctx context.Context) (string, error) { return "", nil }

func (w *memWorkspace) Release(ctx context.Context) error { return nil }

// memFS adapts *fstest.MapFS to FileSystem. It is confined by construction:
// MapFS has no ".." and no absolute paths, so any name that would escape the
// workspace fails NormalizeName with ErrEscape.
type memFS struct {
	fsys *fstest.MapFS
}

func (m *memFS) Open(name string) (fs.File, error) {
	n, err := NormalizeName(name)
	if err != nil {
		return nil, err
	}
	f, err := m.fsys.Open(n)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (m *memFS) Stat(name string) (fs.FileInfo, error) {
	n, err := NormalizeName(name)
	if err != nil {
		return nil, err
	}
	fi, err := fs.Stat(m.fsys, n)
	if err != nil {
		return nil, err
	}
	return fi, nil
}

func (m *memFS) ReadDir(name string) ([]fs.DirEntry, error) {
	n, err := NormalizeName(name)
	if err != nil {
		return nil, err
	}
	des, err := fs.ReadDir(m.fsys, n)
	if err != nil {
		return nil, err
	}
	return des, nil
}

func (m *memFS) WriteFile(name string, data []byte, perm fs.FileMode) error {
	n, err := NormalizeName(name)
	if err != nil {
		return err
	}
	(*m.fsys)[n] = &fstest.MapFile{Data: data, Mode: perm}
	return nil
}

func (m *memFS) MkdirAll(name string, perm fs.FileMode) error {
	n, err := NormalizeName(name)
	if err != nil {
		return err
	}
	if n == "." {
		return nil // the root already exists
	}
	parts := splitPath(n)
	cur := ""
	for i, p := range parts {
		if i > 0 {
			cur += "/"
		}
		cur += p
		if _, ok := (*m.fsys)[cur]; ok {
			if cur == n {
				return nil
			}
			continue
		}
		mode := perm
		if mode&fs.ModeDir == 0 {
			mode |= fs.ModeDir
		}
		(*m.fsys)[cur] = &fstest.MapFile{Mode: mode}
	}
	return nil
}

func (m *memFS) Rename(old, new string) error {
	o, err := NormalizeName(old)
	if err != nil {
		return err
	}
	n, err := NormalizeName(new)
	if err != nil {
		return err
	}
	if _, ok := (*m.fsys)[o]; !ok {
		return fmt.Errorf("%w: no such file %q", fs.ErrNotExist, old)
	}
	(*m.fsys)[n] = (*m.fsys)[o]
	delete(*m.fsys, o)
	return nil
}

func (m *memFS) Remove(name string) error {
	n, err := NormalizeName(name)
	if err != nil {
		return err
	}
	if _, ok := (*m.fsys)[n]; !ok {
		return fmt.Errorf("%w: no such file %q", fs.ErrNotExist, name)
	}
	delete(*m.fsys, n)
	return nil
}

func (m *memFS) Walk(root string, fn fs.WalkDirFunc) error {
	n, err := NormalizeName(root)
	if err != nil {
		return err
	}
	return fs.WalkDir(m.fsys, n, fn)
}

// splitPath splits a clean relative path into segments.
func splitPath(p string) []string {
	if p == "" || p == "." {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i <= len(p); i++ {
		if i == len(p) || p[i] == '/' {
			if i > start {
				out = append(out, p[start:i])
			}
			start = i + 1
		}
	}
	return out
}