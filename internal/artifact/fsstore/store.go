// Package fsstore is the local-filesystem artifact.Store backend.
//
// Layout under the base directory:
//
//	<base>/<tenant>/<loop>/<kind>/<id>              gzip-compressed body
//	<base>/<tenant>/<loop>/<kind>/.meta/<id>.json   artifact.Meta
//
// The path is derived from the ref, so the ref's segments are the tenant
// boundary on the host filesystem. On top of artifact.Ref validation this store
// refuses any uppercase character and any trailing '.', because a
// case-insensitive or trailing-dot-stripping filesystem would otherwise map two
// distinct tenants (or loops, or ids) onto one directory. It refuses rather than
// normalizes: "Acme" is an error, never an alias of "acme".
package fsstore

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ethanhinson/fuse/internal/artifact"
	"github.com/ethanhinson/fuse/internal/event"
)

const (
	metaDir = ".meta"
	metaExt = ".json"
	dirPerm = 0o700
)

// Store is a filesystem artifact store. Several Store values (or processes) may
// share one base directory: every write lands by atomic rename.
type Store struct {
	base string
	// mu orders, within this process, the body+meta rename pair of a Put
	// against the body-open+meta-read pair of an Open, so a concurrent
	// overwrite never pairs one Put's body with another's meta.
	mu sync.RWMutex
}

var _ artifact.Store = (*Store)(nil)

// New returns a store rooted at baseDir. Nothing is created until the first Put.
func New(baseDir string) *Store { return &Store{base: baseDir} }

// fsSafe refuses segments that a case-insensitive or trailing-dot-stripping
// filesystem would alias onto another segment's directory.
func fsSafe(what, s string) error {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			return fmt.Errorf("%w: %s %q has an uppercase character", artifact.ErrInvalidRef, what, s)
		}
	}
	if strings.HasSuffix(s, ".") {
		return fmt.Errorf("%w: %s %q ends with '.'", artifact.ErrInvalidRef, what, s)
	}
	return nil
}

// checkScope validates an already-normalized (tenant, loop, kind) for this store.
func checkScope(tenant event.TenantID, loop event.LoopID, kind artifact.Kind) error {
	if err := artifact.ValidateScope(tenant, loop, kind); err != nil {
		return err
	}
	if err := fsSafe("tenant", string(tenant)); err != nil {
		return err
	}
	return fsSafe("loop", string(loop))
}

// checkRef normalizes the tenant and validates ref; allowEmptyID admits the
// id-less ref Put allocates for. It runs before any filesystem access.
func checkRef(ref artifact.Ref, allowEmptyID bool) (artifact.Ref, error) {
	ref.Tenant = event.NormalizeTenant(ref.Tenant)
	var err error
	if allowEmptyID {
		err = ref.ValidateForPut()
	} else {
		err = ref.Validate()
	}
	if err != nil {
		return artifact.Ref{}, err
	}
	if err := checkScope(ref.Tenant, ref.Loop, ref.Kind); err != nil {
		return artifact.Ref{}, err
	}
	if err := fsSafe("id", ref.ID); err != nil {
		return artifact.Ref{}, err
	}
	return ref, nil
}

func (s *Store) kindDir(tenant event.TenantID, loop event.LoopID, kind artifact.Kind) string {
	return filepath.Join(s.base, string(tenant), string(loop), string(kind))
}

func (s *Store) paths(ref artifact.Ref) (body, meta string) {
	dir := s.kindDir(ref.Tenant, ref.Loop, ref.Kind)
	return filepath.Join(dir, ref.ID), filepath.Join(dir, metaDir, ref.ID+metaExt)
}

// Put streams r through gzip into a temp file beside its destination, counting
// uncompressed bytes against artifact.MaxArtifactBytes, then renames the body
// and then the meta into place. Any failure removes the temp files, so a
// refused or broken Put leaves no body and no meta behind.
func (s *Store) Put(ctx context.Context, ref artifact.Ref, r io.Reader, meta artifact.Meta) (artifact.Meta, error) {
	ref, err := checkRef(ref, true)
	if err != nil {
		return artifact.Meta{}, err
	}
	if err := ctx.Err(); err != nil {
		return artifact.Meta{}, err
	}
	if ref.ID == "" {
		ref.ID = artifact.NewID()
		if err := fsSafe("id", ref.ID); err != nil {
			return artifact.Meta{}, err
		}
	}
	bodyPath, metaPath := s.paths(ref)
	if err := os.MkdirAll(filepath.Dir(metaPath), dirPerm); err != nil {
		return artifact.Meta{}, err
	}

	bodyTmp, size, err := writeBodyTemp(filepath.Dir(bodyPath), ref.ID, r)
	if err != nil {
		return artifact.Meta{}, err
	}
	meta.Ref = ref
	meta.Size = size
	meta.CreatedAt = time.Now().UTC()
	meta.Labels = maps.Clone(meta.Labels)
	mb, err := json.Marshal(meta)
	if err != nil {
		_ = os.Remove(bodyTmp)
		return artifact.Meta{}, err
	}
	metaTmp, err := writeTemp(filepath.Dir(metaPath), ref.ID, func(w io.Writer) error {
		_, err := w.Write(mb)
		return err
	})
	if err != nil {
		_ = os.Remove(bodyTmp)
		return artifact.Meta{}, err
	}
	if err := ctx.Err(); err != nil {
		_ = os.Remove(bodyTmp)
		_ = os.Remove(metaTmp)
		return artifact.Meta{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Rename(bodyTmp, bodyPath); err != nil {
		_ = os.Remove(bodyTmp)
		_ = os.Remove(metaTmp)
		return artifact.Meta{}, err
	}
	if err := os.Rename(metaTmp, metaPath); err != nil {
		// The new body is in place but its meta is not: drop both so the ref
		// reads as consistently absent rather than as a body/meta mismatch.
		_ = os.Remove(metaTmp)
		_ = os.Remove(bodyPath)
		_ = os.Remove(metaPath)
		return artifact.Meta{}, err
	}
	out := meta
	out.Labels = maps.Clone(meta.Labels)
	return out, nil
}

// writeBodyTemp gzips r into a temp file in dir and returns its path and the
// uncompressed byte count. The cap is enforced while reading: one byte past
// artifact.MaxArtifactBytes refuses the body.
func writeBodyTemp(dir, id string, r io.Reader) (string, int64, error) {
	var n int64
	path, err := writeTemp(dir, id, func(w io.Writer) error {
		zw := gzip.NewWriter(w)
		var err error
		n, err = io.Copy(zw, io.LimitReader(r, artifact.MaxArtifactBytes+1))
		if err != nil {
			return err
		}
		if n > artifact.MaxArtifactBytes {
			return artifact.ErrTooLarge
		}
		return zw.Close()
	})
	return path, n, err
}

// writeTemp creates a temp file in dir (its name starts with '.', so it can
// never collide with an id or be listed as one), fills it, syncs and closes it.
// On any error the temp file is removed.
func writeTemp(dir, id string, fill func(io.Writer) error) (_ string, err error) {
	f, err := os.CreateTemp(dir, "."+id+".tmp-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(name)
		}
	}()
	if err = fill(f); err != nil {
		return "", err
	}
	if err = f.Sync(); err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	return name, nil
}

// Open returns the gunzipped body. A missing body or meta is ErrNotFound, and
// so is a meta that names any ref other than the one requested: the full key is
// re-asserted against what is on disk, never inferred from the path alone.
func (s *Store) Open(ctx context.Context, ref artifact.Ref) (io.ReadCloser, artifact.Meta, error) {
	ref, err := checkRef(ref, false)
	if err != nil {
		return nil, artifact.Meta{}, err
	}
	if err := ctx.Err(); err != nil {
		return nil, artifact.Meta{}, err
	}
	bodyPath, metaPath := s.paths(ref)

	s.mu.RLock()
	f, err := os.Open(bodyPath)
	if err != nil {
		s.mu.RUnlock()
		return nil, artifact.Meta{}, notFound(err)
	}
	meta, err := readMeta(metaPath)
	s.mu.RUnlock()
	if err != nil {
		_ = f.Close()
		return nil, artifact.Meta{}, notFound(err)
	}
	if meta.Ref != ref {
		_ = f.Close()
		return nil, artifact.Meta{}, artifact.ErrNotFound
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		_ = f.Close()
		return nil, artifact.Meta{}, fmt.Errorf("artifact: %s: corrupt body: %w", ref, err)
	}
	return &gzipFile{Reader: zr, f: f}, meta, nil
}

// List returns the (tenant, loop, kind) artifacts ordered by CreatedAt, then
// ID. A directory that was never written is an empty list.
func (s *Store) List(ctx context.Context, tenant event.TenantID, loop event.LoopID, kind artifact.Kind) ([]artifact.Meta, error) {
	tenant = event.NormalizeTenant(tenant)
	if err := checkScope(tenant, loop, kind); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.kindDir(tenant, loop, kind), metaDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []artifact.Meta
	for _, e := range entries {
		name := e.Name()
		id, ok := strings.CutSuffix(name, metaExt)
		if !ok || !e.Type().IsRegular() {
			continue // temp files and anything that is not a meta record
		}
		want, err := checkRef(artifact.Ref{Tenant: tenant, Loop: loop, Kind: kind, ID: id}, false)
		if err != nil {
			continue // not a name this store could have written
		}
		m, err := readMeta(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if m.Ref != want {
			continue // a meta naming another ref is never served
		}
		out = append(out, m)
	}
	artifact.SortMetas(out)
	return out, nil
}

func readMeta(path string) (artifact.Meta, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return artifact.Meta{}, err
	}
	var m artifact.Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return artifact.Meta{}, fmt.Errorf("artifact: corrupt meta %s: %w", filepath.Base(path), err)
	}
	return m, nil
}

func notFound(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return artifact.ErrNotFound
	}
	return err
}

// gzipFile closes both the gzip reader and the file under it.
type gzipFile struct {
	*gzip.Reader
	f *os.File
}

func (g *gzipFile) Close() error {
	zerr := g.Reader.Close()
	if err := g.f.Close(); err != nil {
		return err
	}
	return zerr
}
