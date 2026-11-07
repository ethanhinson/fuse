package skills

import (
	"context"
	"log"
	"os"
	"path/filepath"
)

// FSSource implements Source over a list of directories. It scans each
// directory's immediate subdirectories for SKILL.md files, parses them, and
// deduplicates by name (first-wins). Missing directories are skipped.
type FSSource struct {
	Dirs []string
}

// Load returns a Set loaded from the configured directories and its digest.
func (s FSSource) Load(ctx context.Context) (*Set, string, error) {
	set, err := loadDirs(s.Dirs)
	if err != nil {
		return nil, "", err
	}
	return set, DigestOf(set), nil
}

// loadDirs is the internal filesystem loader — the same logic as the old Load().
func loadDirs(dirs []string) (*Set, error) {
	set := &Set{}
	seen := map[string]bool{}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, e := range entries {
			if !e.IsDir() {
				info, err := os.Stat(filepath.Join(dir, e.Name()))
				if err != nil || !info.IsDir() {
					continue
				}
			}
			path := filepath.Join(dir, e.Name(), "SKILL.md")
			data, err := os.ReadFile(path)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, err
			}
			sk, err := ParseSkill(path, data)
			if err != nil {
				log.Printf("[skills] skipping %s: %v", path, err)
				continue
			}
			if seen[sk.Name] {
				continue
			}
			seen[sk.Name] = true
			set.skills = append(set.skills, sk)
		}
	}
	return set, nil
}
