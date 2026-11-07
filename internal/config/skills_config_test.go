package config

import (
	"os"
	"testing"
)

func TestSkillsDirsDefaultUnset(t *testing.T) {
	var c Config
	if len(c.Skills.Dirs) != 0 {
		t.Fatalf("default Dirs = %v, want unset (call site falls back to DefaultDirs)", c.Skills.Dirs)
	}
}

func TestSkillsSectionParses(t *testing.T) {
	c, err := FromYAML([]byte(`
models: {default: fast, fast: {id: cloud/fast}}
skills:
  dirs: [/srv/skills-a, /srv/skills-b]
`), "operator")
	if err != nil {
		t.Fatalf("FromYAML: %v", err)
	}
	if len(c.Skills.Dirs) != 2 || c.Skills.Dirs[0] != "/srv/skills-a" || c.Skills.Dirs[1] != "/srv/skills-b" {
		t.Errorf("Dirs = %v, want [/srv/skills-a /srv/skills-b]", c.Skills.Dirs)
	}
}

// TestSkillsConfigIgnoredFromRepoFile: .fuse.local.yml is repo-plantable, and a
// skill body is an instruction the model is told it must follow. A cloned repo
// must never choose where installed skills come from.
func TestSkillsConfigIgnoredFromRepoFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	plant := "skills:\n  dirs: [/tmp/planted-skills]\n"
	if err := os.WriteFile(".fuse.local.yml", []byte(plant), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Skills.Dirs) != 0 {
		t.Errorf("a repo file set the skill directories: %v", c.Skills.Dirs)
	}
}
