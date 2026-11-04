package tools

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ethanhinson/fuse/internal/artifact"
	"github.com/ethanhinson/fuse/internal/event"
)

// errArtifactsUnavailable is returned when a tool is handed an art:// ref but its
// context carries no artifact binding (a session with no artifact store).
var errArtifactsUnavailable = errors.New("artifact refs are not available in this session")

// artifactNotFound is the single error every unresolvable ref collapses to:
// malformed, missing, or owned by another tenant or loop. Keeping one message
// means a caller cannot probe for another tenant's artifacts.
func artifactNotFound(path string) error {
	return fmt.Errorf("artifact not found: %s", path)
}

// artifactIOError is a store or decode failure reported to the model. Its text
// is fixed and names only the ref: the underlying error can carry a backend's
// host path (a filesystem store's EACCES or EIO names its tenant tree), and tool
// output is durable. The cause stays reachable through Unwrap for callers.
type artifactIOError struct {
	msg string
	err error
}

func (e *artifactIOError) Error() string { return e.msg }
func (e *artifactIOError) Unwrap() error { return e.err }

// artifactFailed builds the fixed "<what> failed: <ref>" error. Only the size
// cap, a sentinel with no path in it, is named, so the model knows to narrow.
func artifactFailed(what, path string, err error) error {
	msg := fmt.Sprintf("%s failed: %s", what, path)
	if errors.Is(err, artifact.ErrTooLarge) {
		msg += ": exceeds the artifact size cap"
	}
	return &artifactIOError{msg: msg, err: err}
}

// readArtifact resolves path when it is an art:// ref. handled is false for any
// other path, so the caller falls back to the filesystem. The ref is resolved
// only through the loop's binding on ctx, and the body is gunzipped when it
// carries the gzip magic, matching archive.Open's on-disk behavior.
func readArtifact(ctx context.Context, path string) (data []byte, handled bool, err error) {
	if !artifact.IsRef(path) {
		return nil, false, nil
	}
	b, ok := artifact.BindingFrom(ctx)
	if !ok || !b.Valid() {
		return nil, true, errArtifactsUnavailable
	}
	ref, err := artifact.ParseRef(path)
	if err != nil {
		return nil, true, artifactNotFound(path)
	}
	rc, _, err := b.Open(ctx, ref)
	if err != nil {
		if errors.Is(err, artifact.ErrNotFound) || errors.Is(err, artifact.ErrInvalidRef) {
			return nil, true, artifactNotFound(path)
		}
		return nil, true, artifactFailed("artifact read", path, err)
	}
	defer rc.Close()
	body, err := readLimited(rc)
	if err != nil {
		return nil, true, artifactFailed("artifact read", path, err)
	}
	if isGzipMagic(body) {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, true, artifactFailed("artifact read", path, err)
		}
		defer zr.Close()
		if body, err = readLimited(zr); err != nil {
			return nil, true, artifactFailed("artifact read", path, err)
		}
	}
	return body, true, nil
}

// readLimited reads r fully, refusing anything over artifact.MaxArtifactBytes
// (which also bounds a decompression bomb).
func readLimited(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, artifact.MaxArtifactBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > artifact.MaxArtifactBytes {
		return nil, artifact.ErrTooLarge
	}
	return body, nil
}

// isGzipMagic reports whether b begins with the gzip magic number (RFC 1952).
func isGzipMagic(b []byte) bool { return len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b }

// listKinds is the order a kind-less prefix lists a loop's artifacts in.
var listKinds = []artifact.Kind{artifact.KindSpill, artifact.KindSegment, artifact.KindSegmentIndex}

// listArtifacts lists the artifacts under an art:// prefix (art://t/l,
// art://t/l/<kind>, or a full ref). handled is false for any other path. The
// prefix's tenant and loop must be the binding's own; anything else is
// "artifact not found", indistinguishable from an empty miss.
func listArtifacts(ctx context.Context, path string) (string, bool, error) {
	if !artifact.IsRef(path) {
		return "", false, nil
	}
	b, ok := artifact.BindingFrom(ctx)
	if !ok || !b.Valid() {
		return "", true, errArtifactsUnavailable
	}
	p, err := artifact.ParsePrefix(path)
	if err != nil ||
		event.NormalizeTenant(p.Tenant) != event.NormalizeTenant(b.Tenant) ||
		p.Loop != b.Loop {
		return "", true, artifactNotFound(path)
	}
	kinds := listKinds
	if p.Kind != "" {
		kinds = []artifact.Kind{p.Kind}
	}
	var lines []string
	for _, k := range kinds {
		metas, err := b.List(ctx, k)
		if err != nil {
			return "", true, artifactFailed("artifact list", path, err)
		}
		for _, m := range metas {
			if p.ID != "" && m.Ref.ID != p.ID {
				continue
			}
			tool := m.Tool
			if tool == "" {
				tool = "-"
			}
			lines = append(lines, fmt.Sprintf("%s  %d bytes  %s  %s",
				m.Ref.String(), m.Size, tool, m.CreatedAt.UTC().Format(time.RFC3339)))
		}
	}
	if p.ID != "" && len(lines) == 0 {
		return "", true, artifactNotFound(path)
	}
	if len(lines) == 0 {
		return "(no artifacts)", true, nil
	}
	return strings.Join(lines, "\n"), true, nil
}
