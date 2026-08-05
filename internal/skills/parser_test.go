package skills

import "testing"

func TestParseSkillFrontmatter(t *testing.T) {
	src := []byte("---\nname: codeindex:impact\ndescription: Blast-radius navigation\nslash_command: /impact\n---\nBody text here.\n")
	s, err := ParseSkill("/x/SKILL.md", src)
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "codeindex:impact" {
		t.Errorf("name = %q", s.Name)
	}
	if s.Description != "Blast-radius navigation" {
		t.Errorf("description = %q", s.Description)
	}
	if s.SlashCommand != "/impact" {
		t.Errorf("slash = %q", s.SlashCommand)
	}
	if s.Body != "Body text here." {
		t.Errorf("body = %q", s.Body)
	}
}

func TestParseSkillContextAndAgent(t *testing.T) {
	src := []byte("---\nname: docket-adr\ndescription: record a decision\ncontext: fork\nagent: docket-adr\n---\nbody\n")
	s, err := ParseSkill("/x/SKILL.md", src)
	if err != nil {
		t.Fatal(err)
	}
	if s.Context != "fork" {
		t.Errorf("context = %q", s.Context)
	}
	if s.Agent != "docket-adr" {
		t.Errorf("agent = %q", s.Agent)
	}
}

func TestParseSkillNoFrontmatterIsError(t *testing.T) {
	_, err := ParseSkill("/x/SKILL.md", []byte("just a body\n"))
	if err == nil {
		t.Error("expected error for missing frontmatter")
	}
}

func TestParseSkillMissingNameIsError(t *testing.T) {
	_, err := ParseSkill("/x/SKILL.md", []byte("---\ndescription: x\n---\nbody\n"))
	if err == nil {
		t.Error("expected error for missing name")
	}
}

// Descriptions containing unquoted ': ' must parse without error.
func TestParseSkillDescriptionWithColons(t *testing.T) {
	src := []byte("---\nname: docket-brainstorm\ndescription: Bindable via `skills: brainstorm:` (the 0049 passthrough); invoked by docket-new-change.\n---\nbody\n")
	s, err := ParseSkill("/x/SKILL.md", src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Name != "docket-brainstorm" {
		t.Errorf("name = %q", s.Name)
	}
	if s.Description == "" {
		t.Error("description should not be empty")
	}
}
