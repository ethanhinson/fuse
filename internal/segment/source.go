package segment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ethanhinson/fuse/internal/artifact"
)

// IndexArtifactID is the fixed artifact ID of a loop's segment index, stored as
// a KindSegmentIndex artifact. Each Archive rewrites it in place.
const IndexArtifactID = "index"

// Source reads a loop's archived segments. It is the store-backed counterpart of
// ReadIndex and LoadSegment.
type Source interface {
	// ReadIndex returns the loop's segment index. A missing index is an empty
	// Index and a nil error, so "no segments yet" reads like an empty selection.
	ReadIndex(ctx context.Context) (Index, error)
	// Load reads and parses the segment an index entry names (entry.Path is the
	// segment artifact ID).
	Load(ctx context.Context, e IndexEntry) (Segment, error)
}

// NewArtifactSource returns a Source over the binding's (tenant, loop). It is
// nil-safe: an invalid binding yields a Source whose ReadIndex is empty and
// whose Load reports artifact.ErrNotFound.
func NewArtifactSource(b artifact.Binding) Source { return artifactSource{b: b} }

type artifactSource struct{ b artifact.Binding }

func (s artifactSource) ReadIndex(ctx context.Context) (Index, error) {
	if !s.b.Valid() {
		return Index{}, nil
	}
	body, err := s.open(ctx, artifact.KindSegmentIndex, IndexArtifactID)
	if err != nil {
		if errors.Is(err, artifact.ErrNotFound) {
			return Index{}, nil
		}
		return Index{}, err
	}
	var idx Index
	if err := json.Unmarshal(body, &idx); err != nil {
		return Index{}, fmt.Errorf("segment index: %w", err)
	}
	return idx, nil
}

func (s artifactSource) Load(ctx context.Context, e IndexEntry) (Segment, error) {
	if !s.b.Valid() {
		return Segment{}, artifact.ErrNotFound
	}
	body, err := s.open(ctx, artifact.KindSegment, e.Path)
	if err != nil {
		return Segment{}, err
	}
	return ParseSegment(body)
}

func (s artifactSource) open(ctx context.Context, kind artifact.Kind, id string) ([]byte, error) {
	// The store normalizes the tenant and validates the ref.
	rc, _, err := s.b.Open(ctx, artifact.Ref{Tenant: s.b.Tenant, Loop: s.b.Loop, Kind: kind, ID: id})
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return artifact.ReadCapped(rc)
}
