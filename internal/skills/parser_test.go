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

func TestParseSkillNoFrontmatterIsError(t *testing.T) {
	_, err := ParseSkill("/x/SKILL.md", []byte("just a body\n"))
	if err == nil {
		t.Fatal("expected error without frontmatter")
	}
}

func TestParseSkillMissingNameIsError(t *testing.T) {
	_, err := ParseSkill("/x/SKILL.md", []byte("---\ndescription: x\n---\nbody\n"))
	if err == nil {
		t.Fatal("expected error when name missing")
	}
}
