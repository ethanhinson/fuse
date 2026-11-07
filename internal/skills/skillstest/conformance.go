// Package skillstest is the installed-skills source conformance suite (change
// 0134). RunSourceConformance exercises the contract every skills.Source must
// satisfy: load round-trip, digest stability across instances, digest
// sensitivity to body and name, name uniqueness, and embedded composition
// precedence (ADR-0003: source skills rank above compiled-in embedded skills).
//
// A source implementation's own test calls RunSourceConformance with a factory
// that builds a FRESH source over the given seed skills (replacing whatever its
// backing store held), so the suite runs against FSSource (a directory),
// StaticSource (a pre-loaded set) and the Postgres source (a table) alike.
package skillstest

import (
	"context"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/skills"
)

// SourceFactory builds a fresh source holding exactly seed (replacing any prior
// contents of the backing store) and returns it with a release func (nil is
// fine). Each call must produce an independent source, so a test can Load two
// of them and compare digests.
type SourceFactory func(t *testing.T, seed []skills.Skill) (skills.Source, func())

// ConformanceSkillA/B are the canonical two skills the suite seeds sources
// with. A factory that cannot install skills dynamically (StaticSource) still
// satisfies them, because seed arrives before the source is constructed.
var (
	ConformanceSkillA = skills.NewCarried("skill-a", "first", "body a")
	ConformanceSkillB = skills.NewCarried("skill-b", "second", "body b")
)

// RunSourceConformance runs the full source contract against the factory.
func RunSourceConformance(t *testing.T, factory SourceFactory) {
	t.Helper()
	t.Run("LoadRoundTrip", func(t *testing.T) { loadRoundTrip(t, factory) })
	t.Run("DigestStableAcrossInstances", func(t *testing.T) { digestStableAcrossInstances(t, factory) })
	t.Run("DigestChangesWithBody", func(t *testing.T) { digestChangesWithBody(t, factory) })
	t.Run("DigestChangesWithName", func(t *testing.T) { digestChangesWithName(t, factory) })
	t.Run("NameUniqueness", func(t *testing.T) { nameUniqueness(t, factory) })
	t.Run("ComposeEmbeddedBelowSource", func(t *testing.T) { composeEmbeddedBelowSource(t, factory) })
}

// loadRoundTrip proves Load returns exactly the seeded skills, in name order
// (the set's discovery order for a deterministic source).
func loadRoundTrip(t *testing.T, factory SourceFactory) {
	t.Helper()
	src, close := factory(t, []skills.Skill{ConformanceSkillA, ConformanceSkillB})
	if close != nil {
		defer close()
	}
	set, digest, err := src.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if digest == "" {
		t.Fatal("digest must be non-empty")
	}
	if set == nil || len(set.All()) != 2 {
		t.Fatalf("Len() = %d, want 2", len(set.All()))
	}
	names := map[string]bool{}
	for _, sk := range set.All() {
		names[sk.Name] = true
	}
	for _, want := range []string{"skill-a", "skill-b"} {
		if !names[want] {
			t.Errorf("loaded set missing %q", want)
		}
	}
}

// digestStableAcrossInstances proves two independently-constructed sources
// over identical contents produce the same digest.
func digestStableAcrossInstances(t *testing.T, factory SourceFactory) {
	t.Helper()
	seed := []skills.Skill{ConformanceSkillA, ConformanceSkillB}
	src1, close1 := factory(t, seed)
	if close1 != nil {
		defer close1()
	}
	src2, close2 := factory(t, seed)
	if close2 != nil {
		defer close2()
	}
	_, d1, err := src1.Load(context.Background())
	if err != nil {
		t.Fatalf("Load 1: %v", err)
	}
	_, d2, err := src2.Load(context.Background())
	if err != nil {
		t.Fatalf("Load 2: %v", err)
	}
	if d1 != d2 {
		t.Errorf("digests differ across instances: %q vs %q", d1, d2)
	}
}

// digestChangesWithBody proves a body edit changes the digest.
func digestChangesWithBody(t *testing.T, factory SourceFactory) {
	t.Helper()
	mut := ConformanceSkillA
	mut.Body = "body a edited"
	src1, close1 := factory(t, []skills.Skill{ConformanceSkillA})
	if close1 != nil {
		defer close1()
	}
	src2, close2 := factory(t, []skills.Skill{mut})
	if close2 != nil {
		defer close2()
	}
	_, d1, err := src1.Load(context.Background())
	if err != nil {
		t.Fatalf("Load 1: %v", err)
	}
	_, d2, err := src2.Load(context.Background())
	if err != nil {
		t.Fatalf("Load 2: %v", err)
	}
	if d1 == d2 {
		t.Errorf("digest should change with body: both %q", d1)
	}
}

// digestChangesWithName proves a rename changes the digest.
func digestChangesWithName(t *testing.T, factory SourceFactory) {
	t.Helper()
	mut := ConformanceSkillA
	mut.Name = "skill-renamed"
	src1, close1 := factory(t, []skills.Skill{ConformanceSkillA})
	if close1 != nil {
		defer close1()
	}
	src2, close2 := factory(t, []skills.Skill{mut})
	if close2 != nil {
		defer close2()
	}
	_, d1, err := src1.Load(context.Background())
	if err != nil {
		t.Fatalf("Load 1: %v", err)
	}
	_, d2, err := src2.Load(context.Background())
	if err != nil {
		t.Fatalf("Load 2: %v", err)
	}
	if d1 == d2 {
		t.Errorf("digest should change with name: both %q", d1)
	}
}

// nameUniqueness proves a source collapses duplicate names, first wins.
func nameUniqueness(t *testing.T, factory SourceFactory) {
	t.Helper()
	dupB := ConformanceSkillB
	dupB.Description = "from-second"
	src, close := factory(t, []skills.Skill{ConformanceSkillB, dupB})
	if close != nil {
		defer close()
	}
	set, _, err := src.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(set.All()) != 1 {
		t.Fatalf("duplicate names should collapse to 1, got %d", len(set.All()))
	}
	if set.All()[0].Description != "second" {
		t.Errorf("first seed should win, got %q", set.All()[0].Description)
	}
}

// composeEmbeddedBelowSource proves ADR-0003 precedence: a source skill named
// "research" shadows the compiled-in embedded one (first-wins), and the
// embedded skills appear when the source lacks them.
func composeEmbeddedBelowSource(t *testing.T, factory SourceFactory) {
	t.Helper()
	userResearch := skills.NewCarried("research", "user-defined research", "user body")
	src, close := factory(t, []skills.Skill{userResearch})
	if close != nil {
		defer close()
	}
	set, digest, err := skills.ComposeEmbedded(context.Background(), src)
	if err != nil {
		t.Fatalf("ComposeEmbedded: %v", err)
	}
	if digest == "" {
		t.Fatal("digest empty after compose")
	}
	sk, ok := set.Lookup("research")
	if !ok {
		t.Fatal("research skill must exist after compose")
	}
	if !strings.Contains(sk.Body, "user body") {
		t.Errorf("user research should shadow embedded: body = %q", sk.Body)
	}
}