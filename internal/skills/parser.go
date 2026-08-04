// Package skills discovers and parses SKILL.md files and exposes them to the
// agent as system-prompt context and shell slash commands.
package skills

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Skill is a parsed SKILL.md.
type Skill struct {
	Name         string
	Description  string
	SlashCommand string
	Body         string
	Path         string
}

// frontmatter mirrors the YAML header of a SKILL.md.
type frontmatter struct {
	Name         string `yaml:"name"`
	Description  string `yaml:"description"`
	SlashCommand string `yaml:"slash_command"`
}

// ParseSkill splits YAML frontmatter (delimited by leading `---` lines) from
// the markdown body and validates required fields.
func ParseSkill(path string, data []byte) (Skill, error) {
	text := string(data)
	if !strings.HasPrefix(text, "---\n") {
		return Skill{}, fmt.Errorf("%s: missing YAML frontmatter", path)
	}
	rest := text[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return Skill{}, fmt.Errorf("%s: unterminated frontmatter", path)
	}
	head := rest[:end]
	body := rest[end+len("\n---"):]
	body = strings.TrimPrefix(body, "\n")
	body = strings.TrimSpace(body)

	var fm frontmatter
	if err := yaml.Unmarshal([]byte(head), &fm); err != nil {
		return Skill{}, fmt.Errorf("%s: parse frontmatter: %w", path, err)
	}
	if fm.Name == "" {
		return Skill{}, fmt.Errorf("%s: skill name is required", path)
	}
	return Skill{
		Name:         fm.Name,
		Description:  fm.Description,
		SlashCommand: fm.SlashCommand,
		Body:         body,
		Path:         path,
	}, nil
}
