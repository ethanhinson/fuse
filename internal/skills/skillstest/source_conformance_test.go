package skillstest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ethanhinson/fuse/internal/skills"
)

// writeSkillDir writes a seeded skill as a directory holding a SKILL.md (the
// frontmatter is synthesized from the carried skill's fields).

func writeSkillDir(t *testing.T, dir string, sk skills.Skill) {
	t.Helper()
	d := filepath.Join(dir, sk.Name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: " + sk.Name + "\ndescription: " + sk.Description + "\n---\n" + sk.Body + "\n"
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fsSourceFactory seeds a fresh temp dir and returns an FSSource over it.
// Seeds with the same name land in SEPARATE top-level dirs (dir0, dir1, …) so
// the first-wins precedence the suite asserts is meaningful on the
// filesystem: FSSource scans the dirs in order and a later dir holding the
// same name would win if the order were wrong.
func fsSourceFactory(t *testing.T, seed []skills.Skill) (skills.Source, func()) {
	var top []string
	byName := map[string]int{}
	for _, sk := range seed {
		idx := byName[sk.Name]
		byName[sk.Name] = idx + 1
		dir := filepath.Join(t.TempDir(), "d")
		_ = os.MkdirAll(dir, 0o755)
		top = append(top, dir)
		writeSkillDir(t, dir, sk)
	}
	return &skills.FSSource{Dirs: top}, nil
}

// TestFSSourceConformance runs the full source contract against FSSource.
func TestFSSourceConformance(t *testing.T) {
	RunSourceConformance(t, fsSourceFactory)
}

// TestStaticSourceConformance runs the full source contract against
// StaticSource, which is constructed from a set built out of the seed.
func TestStaticSourceConformance(t *testing.T) {
	RunSourceConformance(t, func(t *testing.T, seed []skills.Skill) (skills.Source, func()) {
		sk := make([]skills.Skill, len(seed))
		copy(sk, seed)
		return skills.NewStaticSource(skills.NewSet(sk)), nil
	})
}
