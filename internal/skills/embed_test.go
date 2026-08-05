package skills

import (
	"strings"
	"testing"
)

func TestEmbeddedReturnsResearchSkill(t *testing.T) {
	emb, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	var research *Skill
	for i := range emb {
		if emb[i].Name == "research" {
			research = &emb[i]
			break
		}
	}
	if research == nil {
		t.Fatalf("expected an embedded skill named research, got %v", emb)
	}
	if research.SlashCommand != "/research" {
		t.Errorf("slash command = %q, want /research", research.SlashCommand)
	}
	if strings.TrimSpace(research.Body) == "" {
		t.Error("embedded research skill body is empty")
	}
}

func TestLoadWithEmbeddedIncludesResearchOnEmptyDirs(t *testing.T) {
	set, err := LoadWithEmbedded([]string{"/nonexistent/dir"})
	if err != nil {
		t.Fatal(err)
	}
	sk, ok := set.Lookup("research")
	if !ok {
		t.Fatal("expected embedded research to be present with no filesystem dirs")
	}
	if sk.SlashCommand != "/research" {
		t.Errorf("slash command = %q, want /research", sk.SlashCommand)
	}
	cmds := set.SlashCommands()
	if _, ok := cmds["/research"]; !ok {
		t.Error("/research not registered in SlashCommands")
	}
}

func TestLoadWithEmbeddedUserResearchShadowsEmbedded(t *testing.T) {
	a := t.TempDir()
	writeSkill(t, a, "research", "---\nname: research\nslash_command: /research\ndescription: user research\n---\nuser body wins\n")

	set, err := LoadWithEmbedded([]string{a})
	if err != nil {
		t.Fatal(err)
	}
	// Only one research skill — the embedded one is dropped.
	count := 0
	for _, sk := range set.All() {
		if sk.Name == "research" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 research skill, got %d", count)
	}
	sk, ok := set.Lookup("research")
	if !ok {
		t.Fatal("expected research skill present")
	}
	if sk.Body != "user body wins" {
		t.Errorf("body = %q, want user body to shadow embedded", sk.Body)
	}
}
