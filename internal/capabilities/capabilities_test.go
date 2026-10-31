package capabilities

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ethanhinson/fuse/harness"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/skills"
	"github.com/ethanhinson/fuse/internal/version"
)

func TestModelsListsWhatTheServerCanRun(t *testing.T) {
	reg := model.NewRegistry("fast", map[string]model.ModelConfig{
		"fast":   {ID: "cloud/fast", MaxTokens: 4096, Persona: "coding", Label: "Fast", Description: "d", Tags: []string{"tools", "custom"}},
		"big":    {ID: "cloud/big", MaxTokens: 16384, ContextWindow: 1_000_000, Persona: "general"},
		"max":    {ID: "cli/claude-max", Persona: "coding"},
		"secret": {ID: "cloud/secret", Hidden: true},
	})
	want := []Model{
		{Alias: "big", GatewayID: "cloud/big", MaxOutputTokens: 16384, ContextWindow: 1_000_000, Persona: "general"},
		{Alias: "fast", GatewayID: "cloud/fast", MaxOutputTokens: 4096, ContextWindow: 128_000, ContextWindowIsDefault: true,
			Persona: "coding", IsDefault: true, Label: "Fast", Description: "d", Tags: []string{"tools", "custom"}},
	}
	if got := Models(reg, 128_000); !reflect.DeepEqual(got, want) {
		t.Fatalf("Models =\n %+v\nwant\n %+v", got, want)
	}
}

func TestModelsDoesNotAliasRegistryTags(t *testing.T) {
	reg := model.NewRegistry("a", map[string]model.ModelConfig{
		"a": {ID: "cloud/a", Tags: []string{"tools"}},
	})
	got := Models(reg, 1)
	if len(got) != 1 || len(got[0].Tags) != 1 {
		t.Fatalf("Models = %+v", got)
	}
	got[0].Tags[0] = "mutated"
	mc, err := reg.Resolve("a")
	if err != nil {
		t.Fatal(err)
	}
	if mc.Tags[0] != "tools" {
		t.Fatalf("registry tags changed to %q through the listing", mc.Tags[0])
	}
}

func TestModelsNilRegistry(t *testing.T) {
	if got := Models(nil, 1); len(got) != 0 {
		t.Fatalf("Models(nil) = %+v, want empty", got)
	}
}

func writeSkill(t *testing.T, root, dir, name, desc string) {
	t.Helper()
	p := filepath.Join(root, dir)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: " + desc + "\n---\nBody.\n"
	if err := os.WriteFile(filepath.Join(p, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSkillsListsInstalledSkillsOncePerName(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	writeSkill(t, first, "notes", "notes", "take notes")
	writeSkill(t, second, "notes-again", "notes", "shadowed notes")
	writeSkill(t, second, "review", "review", "review code")
	set, err := skills.Load([]string{first, second})
	if err != nil {
		t.Fatal(err)
	}
	got := Skills(set)
	// skills.Load already drops shadowed names, so the listing is set.All()
	// in discovery order, each described as Lookup describes it.
	var want []Skill
	for _, sk := range set.All() {
		found, ok := set.Lookup(sk.Name)
		if !ok {
			t.Fatalf("Lookup(%q) missed", sk.Name)
		}
		want = append(want, Skill{Name: sk.Name, Description: found.Description})
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Skills =\n %+v\nwant\n %+v", got, want)
	}
	wantExplicit := []Skill{{Name: "notes", Description: "take notes"}, {Name: "review", Description: "review code"}}
	if !reflect.DeepEqual(got, wantExplicit) {
		t.Fatalf("Skills =\n %+v\nwant\n %+v", got, wantExplicit)
	}
	if got := Skills(nil); len(got) != 0 {
		t.Fatalf("Skills(nil) = %+v, want empty", got)
	}
}

func TestServerInfo(t *testing.T) {
	want := Server{Version: version.Version, DigestAlgorithm: harness.DigestAlgorithm}
	if got := ServerInfo(); got != want {
		t.Fatalf("ServerInfo = %+v, want %+v", got, want)
	}
}
