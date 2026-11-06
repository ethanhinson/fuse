package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
)

// Source is the abstract skill-installation seam. A Source loads the skill set
// the operator installed — from a filesystem directory, a Postgres table, or a
// test fixture — and returns them as a loading-time Set plus a Digest that lets
// the caller detect changes without re-reading every skill body.
type Source interface {
	// Load returns the installed skill set and its digest. It must be safe to
	// call repeatedly: a caller that refreshes on a ticker calls Load on each
	// tick to detect changes.
	Load(ctx context.Context) (*Set, string, error)
}

// DigestOf computes a sha256 hex digest over a sorted (name, body) tuple from
// each skill in the set, so two identically-loaded sets produce the same digest
// and a change to any skill's name or body changes the digest.
func DigestOf(set *Set) string {
	var pairs []string
	for _, sk := range set.All() {
		// Description is part of the digest (change 0134): it is model-visible
		// (loader.go renders it into the brief), so a description-only edit is
		// a real change the drift stream must pick up.
		pairs = append(pairs, sk.Name+"\x00"+sk.Description+"\x00"+sk.Body)
	}
	sort.Strings(pairs)
	h := sha256.New()
	for _, p := range pairs {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)[:])
}

// NewSet returns a Set over skills in the given order, deduplicating by name
// (first wins). It is the construction seam Source implementations (FSSource,
// the Postgres source) use to hand a loaded set back.
func NewSet(skills []Skill) *Set {
	out := &Set{}
	seen := map[string]bool{}
	for _, sk := range skills {
		if seen[sk.Name] {
			continue
		}
		seen[sk.Name] = true
		out.skills = append(out.skills, sk)
	}
	return out
}

// ComposeEmbedded loads from source, then folds in compiled-in embedded skills
// for any name not already present. Filesystem skills always win: a user skill
// named "research" shadows the embedded one (first-wins preserved), and
// embedded skills rank lowest. This replaces LoadWithEmbedded.
func ComposeEmbedded(ctx context.Context, src Source) (*Set, string, error) {
	set, digest, err := src.Load(ctx)
	if err != nil {
		return nil, "", err
	}
	seen := map[string]bool{}
	for _, sk := range set.All() {
		seen[sk.Name] = true
	}
	emb, err := Embedded()
	if err != nil {
		return nil, "", err
	}
	for _, sk := range emb {
		if seen[sk.Name] {
			continue
		}
		seen[sk.Name] = true
		set.skills = append(set.skills, sk)
	}
	// Recompute digest after folding embedded.
	digest = DigestOf(set)
	return set, digest, nil
}
