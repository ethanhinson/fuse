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
	Context      string // e.g. "fork" — stored for future subagent dispatch
	Agent        string // subagent type hint — stored for future dispatch
	Body         string
	Path         string
}

// frontmatter holds the fields that are safe to parse with a strict YAML
// parser (no free-text values that might contain unquoted ': ').
type frontmatter struct {
	SlashCommand string `yaml:"slash_command"`
	Context      string `yaml:"context"`
	Agent        string `yaml:"agent"`
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

	// Extract name and description line-by-line to avoid YAML parser failures
	// on free-text values containing unquoted ': ' (e.g. inline code like
	// `skills: brainstorm:`). Other scalar fields (slash_command, context,
	// agent) are simple identifiers and safe to parse with yaml.Unmarshal.
	name, description := extractLineFields(head)
	if name == "" {
		return Skill{}, fmt.Errorf("%s: skill name is required", path)
	}

	// Omit lines handled above so YAML only sees safe scalars.
	var fm frontmatter
	if err := yaml.Unmarshal([]byte(stripLineFields(head)), &fm); err != nil {
		return Skill{}, fmt.Errorf("%s: parse frontmatter: %w", path, err)
	}
	return Skill{
		Name:         name,
		Description:  description,
		SlashCommand: fm.SlashCommand,
		Context:      fm.Context,
		Agent:        fm.Agent,
		Body:         body,
		Path:         path,
	}, nil
}

// extractLineFields reads name: and description: directly from raw frontmatter
// lines, returning them verbatim without YAML parsing.
func extractLineFields(head string) (name, description string) {
	for _, line := range strings.Split(head, "\n") {
		if v, ok := lineFieldValue(line, "name"); ok && name == "" {
			name = v
		}
		if v, ok := lineFieldValue(line, "description"); ok && description == "" {
			description = v
		}
	}
	return
}

// stripLineFields returns the frontmatter block with name: and description:
// lines removed so yaml.Unmarshal doesn't choke on their free-text values.
func stripLineFields(head string) string {
	var out []string
	for _, line := range strings.Split(head, "\n") {
		if _, ok := lineFieldValue(line, "name"); ok {
			continue
		}
		if _, ok := lineFieldValue(line, "description"); ok {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// lineFieldValue returns the value of a "key: value" line, stripping an outer
// matched pair of single or double quotes if present.
func lineFieldValue(line, key string) (string, bool) {
	prefix := key + ": "
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	v := strings.TrimSpace(line[len(prefix):])
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
	}
	return v, true
}
