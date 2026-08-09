// Package archive provides a small, domain-agnostic gzip archiver used by the
// non-destructive age sweeps (change 0030 scope expansion). Instead of deleting
// stale files, a sweep calls Archive to gzip the file in place to "<path>.gz"
// and drop a YAML "<path>.gz.meta.yml" sidecar describing WHAT the file held, so
// the model can still recover the data. Open reads either the plaintext or the
// gzipped form transparently.
//
// The helper stays generic (bytes + a caller-supplied MetaFunc) so it never
// imports session or tools — that would create an import cycle. Each caller
// (session logs, tool-output spill) supplies its own domain metadata fields.
package archive

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// gzMagic is the two-byte gzip magic number (RFC 1952). A file whose first two
// bytes match is treated as gzip and decompressed on read.
var gzMagic = []byte{0x1f, 0x8b}

// GzSuffix is the extension Archive appends to a compressed file.
const GzSuffix = ".gz"

// MetaSuffix is the extension of the sidecar frontmatter file (appended after
// GzSuffix, e.g. "session.jsonl.gz.meta.yml").
const MetaSuffix = ".meta.yml"

// MetaFunc lets a caller contribute domain-specific frontmatter fields from the
// decompressed content. It receives the original (decompressed) bytes and
// returns fields merged into the sidecar on top of the common fields Archive
// always writes. A nil MetaFunc means "common fields only".
type MetaFunc func(content []byte) map[string]any

// Archive gzips path -> path+".gz" (0o600), writes a YAML sidecar
// path+".gz.meta.yml" describing the file, then removes the original. It is
// idempotent and best-effort friendly:
//
//   - if path already ends in ".gz", it is a no-op (returns "", nil);
//   - if path+".gz" already exists, it is a no-op (returns the existing meta
//     path, nil) — the file was archived on a prior sweep.
//
// metaPath is the path of the sidecar written (or that already exists).
func Archive(path string, meta MetaFunc) (metaPath string, err error) {
	if hasGzSuffix(path) {
		return "", nil // already compressed — nothing to do
	}
	gzPath := path + GzSuffix
	metaPath = gzPath + MetaSuffix
	if _, statErr := os.Stat(gzPath); statErr == nil {
		return metaPath, nil // already archived on a prior sweep — idempotent
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	compressed, err := gzipBytes(content)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(gzPath, compressed, 0o600); err != nil {
		return "", err
	}

	if err := writeMeta(metaPath, path, content, compressed, meta); err != nil {
		// The sidecar is best-effort metadata; the compressed payload is already
		// safe on disk. Remove the original anyway so the sweep's non-destructive
		// contract holds (data preserved as .gz), but surface the sidecar error.
		_ = os.Remove(path)
		return metaPath, err
	}

	if err := os.Remove(path); err != nil {
		return metaPath, err
	}
	return metaPath, nil
}

// Open reads path transparently: it prefers path; if path is missing it tries
// path+".gz"; and it gunzips whenever the opened bytes begin with the gzip magic
// number. It returns the decompressed content. A truncated or corrupt gzip
// stream yields an error (never a panic).
func Open(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !hasGzSuffix(path) {
			// Fall back to the archived form.
			if gb, gerr := os.ReadFile(path + GzSuffix); gerr == nil {
				return gunzipIfNeeded(gb)
			}
		}
		return nil, err
	}
	return gunzipIfNeeded(b)
}

// gunzipIfNeeded decompresses b when it starts with the gzip magic, else returns
// b unchanged. This keeps OLD plaintext files readable alongside new .gz ones.
func gunzipIfNeeded(b []byte) ([]byte, error) {
	if !isGzip(b) {
		return b, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("gzip open: %w", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("gzip read: %w", err)
	}
	return out, nil
}

// gzipBytes returns content compressed with gzip's default settings.
func gzipBytes(content []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(content); err != nil {
		return nil, fmt.Errorf("gzip write: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("gzip close: %w", err)
	}
	return buf.Bytes(), nil
}

// writeMeta builds and writes the YAML sidecar. The common fields ("WHAT is in
// the file" at the storage layer) are always present; the caller's MetaFunc adds
// domain fields on top.
func writeMeta(metaPath, originalPath string, content, compressed []byte, meta MetaFunc) error {
	fields := map[string]any{
		"archived_at":      time.Now().UTC().Format(time.RFC3339),
		"original_name":    baseName(originalPath),
		"original_bytes":   len(content),
		"compressed_bytes": len(compressed),
	}
	if meta != nil {
		for k, v := range meta(content) {
			fields[k] = v
		}
	}
	out, err := yaml.Marshal(fields)
	if err != nil {
		return fmt.Errorf("meta marshal: %w", err)
	}
	return os.WriteFile(metaPath, out, 0o600)
}

// isGzip reports whether b begins with the gzip magic number.
func isGzip(b []byte) bool {
	return len(b) >= 2 && b[0] == gzMagic[0] && b[1] == gzMagic[1]
}

// hasGzSuffix reports whether path ends in the gzip suffix.
func hasGzSuffix(path string) bool {
	return len(path) >= len(GzSuffix) && path[len(path)-len(GzSuffix):] == GzSuffix
}

// baseName returns the final path element (avoids importing path/filepath for a
// single use and keeps the sidecar's original_name free of directory noise).
func baseName(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[i+1:]
		}
	}
	return path
}
