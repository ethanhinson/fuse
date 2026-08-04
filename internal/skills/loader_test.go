package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, dir, sub, content string) {
	t.Helper()
	d := filepath.Join(dir, sub)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDiscoversSkillsAcrossDirs(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	writeSkill(t, a, "impact", "---\nname: codeindex:impact\ndescription: blast radius\nslash_command: /impact\n---\ndo impact\n")
	writeSkill(t, b, "route", "---\nname: multi-model:route\ndescription: pick a model\nslash_command: /route\n---\ndo route\n")

	set, err := Load([]string{a, b, "/nonexistent/dir"})
	if err != nil {
		t.Fatal(err)
	}
	if len(set.All()) != 2 {
		t.Fatalf("expected 2 skills, got %d", len(set.All()))
	}

	cmds := set.SlashCommands()
	if _, ok := cmds["/impact"]; !ok {
		t.Error("/impact not registered")
	}
	if _, ok := cmds["/route"]; !ok {
		t.Error("/route not registered")
	}

	block := set.SystemPromptBlock()
	if !strings.Contains(block, "codeindex:impact") || !strings.Contains(block, "blast radius") {
		t.Errorf("system block = %q", block)
	}
}

func TestLoadEmptyWhenNoDirs(t *testing.T) {
	set, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.All()) != 0 {
		t.Fatalf("expected no skills, got %d", len(set.All()))
	}
	if set.SystemPromptBlock() != "" {
		t.Errorf("empty set should yield empty block, got %q", set.SystemPromptBlock())
	}
}

func TestSlashCommandsAutoDerived(t *testing.T) {
	a := t.TempDir()
	// skill without slash_command — should get /<name> auto-derived
	writeSkill(t, a, "docket-status", "---\nname: docket-status\ndescription: show board\n---\nbody\n")
	set, err := Load([]string{a})
	if err != nil {
		t.Fatal(err)
	}
	cmds := set.SlashCommands()
	if _, ok := cmds["/docket-status"]; !ok {
		t.Error("auto-derived /docket-status not in SlashCommands")
	}
}

func TestSlashCommandsExplicitWins(t *testing.T) {
	a := t.TempDir()
	writeSkill(t, a, "s", "---\nname: my-skill\nslash_command: /ms\n---\nbody\n")
	set, err := Load([]string{a})
	if err != nil {
		t.Fatal(err)
	}
	cmds := set.SlashCommands()
	if _, ok := cmds["/ms"]; !ok {
		t.Error("explicit slash_command /ms should be present")
	}
	if _, ok := cmds["/my-skill"]; ok {
		t.Error("auto-derived /my-skill should not appear when explicit slash_command is set")
	}
}

func TestLookupFound(t *testing.T) {
	a := t.TempDir()
	writeSkill(t, a, "conv", "---\nname: docket-convention\ndescription: the contract\n---\nconvention body\n")
	set, err := Load([]string{a})
	if err != nil {
		t.Fatal(err)
	}
	sk, ok := set.Lookup("docket-convention")
	if !ok {
		t.Fatal("expected Lookup to find docket-convention")
	}
	if sk.Body != "convention body" {
		t.Errorf("body = %q", sk.Body)
	}
}

func TestLookupNotFound(t *testing.T) {
	set, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	_, ok := set.Lookup("missing")
	if ok {
		t.Fatal("expected Lookup to return false for unknown skill")
	}
}

func TestLoadFirstDirWinsOnDuplicateName(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	writeSkill(t, a, "impact", "---\nname: dup\ndescription: from-a\nslash_command: /dup\n---\nA\n")
	writeSkill(t, b, "impact", "---\nname: dup\ndescription: from-b\nslash_command: /dup\n---\nB\n")
	set, err := Load([]string{a, b})
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
