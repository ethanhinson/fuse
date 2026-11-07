package skills

import "context"

// StaticSource holds a pre-loaded skill set for tests. Its digest is computed
// once at construction.
type StaticSource struct {
	Set    *Set
	Digest string
}

// Load returns the pre-loaded set unchanged. Safe to call repeatedly.
func (s StaticSource) Load(ctx context.Context) (*Set, string, error) {
	return s.Set, s.Digest, nil
}

// NewStaticSource builds a StaticSource from a pre-loaded set, computing its
// digest.
func NewStaticSource(set *Set) StaticSource {
	return StaticSource{Set: set, Digest: DigestOf(set)}
}