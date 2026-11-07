package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sourceWriteSkill is a helper that writes a SKILL.md under dir/sub/.
func sourceWriteSkill(t *testing.T, dir, sub, content string) {
	t.Helper()
	d := filepath.Join(dir, sub)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDigestOfIsStableAcrossRepeatedLoads proves the digest is deterministic
// for the same directory contents.
func TestDigestOfIsStableAcrossRepeatedLoads(t *testing.T) {
	a := t.TempDir()
	sourceWriteSkill(t, a, "one", "---\nname: skill-a\ndescription: first\n---\nbody a\n")
	sourceWriteSkill(t, a, "two", "---\nname: skill-b\ndescription: second\n---\nbody b\n")

	src := FSSource{Dirs: []string{a}}
	set1, d1, err := src.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	set2, d2, err := src.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(set1.All()) != len(set2.All()) {
		t.Fatalf("load changed between calls: %d vs %d", len(set1.All()), len(set2.All()))
	}
	if d1 != d2 {
		t.Errorf("digest differs between loads: %q vs %q", d1, d2)
	}
}

// TestDigestOfChangesWhenBodyChanges proves the digest is sensitive to content.
func TestDigestOfChangesWhenBodyChanges(t *testing.T) {
	a := t.TempDir()
	sourceWriteSkill(t, a, "one", "---\nname: skill-a\ndescription: first\n---\nbody a\n")

	src := FSSource{Dirs: []string{a}}
	_, d1, err := src.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	os.Remove(filepath.Join(a, "one", "SKILL.md"))
	sourceWriteSkill(t, a, "one", "---\nname: skill-a\ndescription: first\n---\nbody b\n")
	_, d2, err := src.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d1 == d2 {
		t.Errorf("digest should have changed after body edit, both = %q", d1)
	}
}

// TestDigestOfChangesWhenNameChanges proves the digest is sensitive to name.
func TestDigestOfChangesWhenNameChanges(t *testing.T) {
	a := t.TempDir()
	sourceWriteSkill(t, a, "one", "---\nname: skill-a\ndescription: first\n---\nbody a\n")

	src := FSSource{Dirs: []string{a}}
	_, d1, err := src.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	os.Remove(filepath.Join(a, "one", "SKILL.md"))
	sourceWriteSkill(t, a, "one", "---\nname: skill-b\ndescription: first\n---\nbody a\n")
	_, d2, err := src.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d1 == d2 {
		t.Errorf("digest should have changed after name edit, both = %q", d1)
	}
}

// TestDigestOfStableAcrossInstances proves that two identical directories
// produce the same digest even loaded via different Source instances.
func TestDigestOfStableAcrossInstances(t *testing.T) {
	a := t.TempDir()
	sourceWriteSkill(t, a, "one", "---\nname: skill-a\ndescription: first\n---\nbody a\n")
	sourceWriteSkill(t, a, "two", "---\nname: skill-b\ndescription: second\n---\nbody b\n")

	src1 := FSSource{Dirs: []string{a}}
	_, d1, err := src1.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	src2 := FSSource{Dirs: []string{a}}
	_, d2, err := src2.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Errorf("digest differs across Source instances: %q vs %q", d1, d2)
	}
}

// TestNameUniqueness proves that FSSource deduplicates by name like Load().
func TestNameUniqueness(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	sourceWriteSkill(t, a, "impact", "---\nname: dup\ndescription: from-a\n---\nA\n")
	sourceWriteSkill(t, b, "impact", "---\nname: dup\ndescription: from-b\n---\nB\n")

	src := FSSource{Dirs: []string{a, b}}
	set, _, err := src.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(set.All()) != 1 {
		t.Fatalf("duplicate names should collapse to 1, got %d", len(set.All()))
	}
	if set.All()[0].Description != "from-a" {
		t.Errorf("first dir should win, got %q", set.All()[0].Description)
	}
}

// TestStaticSourceIsDeterministic proves the static source returns stable set+digest.
func TestStaticSourceIsDeterministic(t *testing.T) {
	a := t.TempDir()
	sourceWriteSkill(t, a, "one", "---\nname: skill-a\ndescription: first\n---\nbody a\n")
	set, err := Load([]string{a})
	if err != nil {
		t.Fatal(err)
	}
	src := NewStaticSource(set)
	_, d1, err := src.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, d2, err := src.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Errorf("StaticSource digest changed across loads: %q vs %q", d1, d2)
	}
}

// TestComposeEmbeddedIncludesEmbedded proves embedded skills appear when source
// returns an empty set.
func TestComposeEmbeddedIncludesEmbedded(t *testing.T) {
	a := t.TempDir()
	src := FSSource{Dirs: []string{a}}
	set, digest, err := ComposeEmbedded(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	_, ok := set.Lookup("research")
	if !ok {
		t.Errorf("ComposeEmbedded should include the embedded research skill")
	}
	if digest == "" {
		t.Errorf("ComposeEmbedded should produce a non-empty digest")
	}
}

// TestComposeEmbeddedUserShadowsEmbedded proves a filesystem skill named
// "research" shadows the compiled-in one.
func TestComposeEmbeddedUserShadowsEmbedded(t *testing.T) {
	a := t.TempDir()
	sourceWriteSkill(t, a, "research", "---\nname: research\ndescription: user-defined research\n---\nuser body\n")
	src := FSSource{Dirs: []string{a}}
	set, _, err := ComposeEmbedded(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	sk, ok := set.Lookup("research")
	if !ok {
		t.Fatal("research skill must exist")
	}
	if !strings.Contains(sk.Body, "user body") {
		t.Errorf("user's research skill should shadow embedded: body = %q", sk.Body)
	}
}

// TestDigestOfEmptySet is deterministic.
func TestDigestOfEmptySet(t *testing.T) {
	set, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	d := DigestOf(set)
	if d == "" {
		t.Errorf("digest of empty set should not be empty")
	}
	if DigestOf(set) != d {
		t.Errorf("digest of empty set not stable")
	}
}

// TestComposeEmbeddedDigestTracksSourceChanges proves that ComposeEmbedded
// produces a digest that changes when source content changes.
func TestComposeEmbeddedDigestTracksSourceChanges(t *testing.T) {
	a := t.TempDir()
	sourceWriteSkill(t, a, "one", "---\nname: skill-a\ndescription: first\n---\nbody a\n")
	src := FSSource{Dirs: []string{a}}
	_, d1, err := ComposeEmbedded(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}

	os.Remove(filepath.Join(a, "one", "SKILL.md"))
	sourceWriteSkill(t, a, "one", "---\nname: skill-a\ndescription: first\n---\nbody b\n")
	_, d2, err := ComposeEmbedded(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if d1 == d2 {
		t.Errorf("ComposeEmbedded digest should change when source changes, both = %q", d1)
	}
}
