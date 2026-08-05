package skills

import (
	"log"
	"os"
	"path/filepath"
	"strings"
)

// Set is a collection of discovered skills with first-wins deduplication by
// skill name.
type Set struct {
	skills []Skill
}

// DefaultDirs returns the three skill discovery paths in precedence order.
func DefaultDirs() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{
		filepath.Join(home, ".fuse", "skills"),
		filepath.Join(home, ".claude", "skills"),
		filepath.Join(home, ".grok", "skills"),
	}
}

// Load scans each directory for immediate subdirectories containing a
// SKILL.md, parses them, and returns a Set. Missing directories are skipped.
// When two skills share a name, the one from the earlier directory wins.
func Load(dirs []string) (*Set, error) {
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
				// DirEntry.IsDir() returns false for symlinks; follow the link.
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

// LoadWithEmbedded runs Load(dirs) and then folds in the compiled-in embedded
// skills for any name not already present. Filesystem skills always win: a user
// skill named "research" shadows the embedded one (first-wins preserved), and
// embedded skills rank lowest.
func LoadWithEmbedded(dirs []string) (*Set, error) {
	set, err := Load(dirs)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, sk := range set.skills {
		seen[sk.Name] = true
	}
	emb, err := Embedded()
	if err != nil {
		return nil, err
	}
	for _, sk := range emb {
		if seen[sk.Name] {
			continue
		}
		seen[sk.Name] = true
		set.skills = append(set.skills, sk)
	}
	return set, nil
}

// All returns every loaded skill in discovery order.
func (s *Set) All() []Skill { return s.skills }

// SlashCommands maps each skill's slash command (including the leading slash)
// to its skill. When slash_command frontmatter is unset, falls back to /<name>.
func (s *Set) SlashCommands() map[string]Skill {
	out := map[string]Skill{}
	for _, sk := range s.skills {
		key := sk.SlashCommand
		if key == "" {
			key = "/" + sk.Name
		}
		out[key] = sk
	}
	return out
}

// Lookup finds a skill by name. Returns false if not found.
func (s *Set) Lookup(name string) (Skill, bool) {
	for _, sk := range s.skills {
		if sk.Name == name {
			return sk, true
		}
	}
	return Skill{}, false
}

// SystemPromptBlock renders a compact listing of available skills for
// injection into the model's system context. Empty when no skills loaded.
func (s *Set) SystemPromptBlock() string {
	if len(s.skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Available skills:\n")
	for _, sk := range s.skills {
		b.WriteString("- ")
		b.WriteString(sk.Name)
		if sk.Description != "" {
			b.WriteString(": ")
			b.WriteString(sk.Description)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
