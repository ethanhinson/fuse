package skills

import _ "embed"

// embeddedResearch is the canonical source for the built-in research skill,
// compiled into the binary. go:embed can only reach files within this package's
// directory tree, so the single source lives at embedded/research.md.
//
//go:embed embedded/research.md
var embeddedResearch string

// embeddedResearchPath is the synthetic path recorded on the parsed skill so it
// is distinguishable from a filesystem-loaded skill.
const embeddedResearchPath = "embedded://research/SKILL.md"

// Embedded parses and returns the skills that are compiled into the binary.
// Currently this is just the research skill.
func Embedded() ([]Skill, error) {
	sk, err := ParseSkill(embeddedResearchPath, []byte(embeddedResearch))
	if err != nil {
		return nil, err
	}
	return []Skill{sk}, nil
}
