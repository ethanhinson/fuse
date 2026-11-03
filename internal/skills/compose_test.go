package skills

import (
	"strings"
	"testing"
)

func TestNewCarriedBuildsWithoutParsing(t *testing.T) {
	if CarriedPath != "spec" {
		t.Fatalf("CarriedPath = %q, want %q", CarriedPath, "spec")
	}
	got := NewCarried("triage", "Sort issues: label and size", "# Triage\nbody")
	want := Skill{
		Name:        "triage",
		Description: "Sort issues: label and size",
		Body:        "# Triage\nbody",
		Path:        CarriedPath,
	}
	if got != want {
		t.Fatalf("NewCarried = %+v, want %+v", got, want)
	}
	if got.SlashCommand != "" || got.Context != "" || got.Agent != "" {
		t.Fatalf("carried skill has dispatch hints: %+v", got)
	}
}

func composedNames(s *Set) []string {
	var out []string
	for _, sk := range s.All() {
		out = append(out, sk.Name)
	}
	return out
}

func TestComposeKeepsSpecOrderAndShadows(t *testing.T) {
	src := &Set{skills: []Skill{{Name: "a", Body: "A"}, {Name: "b", Body: "B"}, {Name: "c"}}}
	x := NewCarried("x", "does x", "X")
	aCarried := NewCarried("a", "carried a", "carried A")
	got := src.Compose([]Entry{
		{Name: "c"},
		{Name: "x", Carried: &x},
		{Name: "a", Carried: &aCarried},
		{Name: "missing"},
	})
	if n := strings.Join(composedNames(got), ","); n != "c,x,a" {
		t.Fatalf("Compose names = %s, want c,x,a", n)
	}
	if sk, ok := got.Lookup("a"); !ok || sk.Body != "carried A" {
		t.Fatalf("Lookup(a) = %+v, %v; want the carried body", sk, ok)
	}
	if _, ok := got.Lookup("b"); ok {
		t.Fatal("an unlisted installed skill is still found")
	}
	if sk, ok := src.Lookup("a"); !ok || sk.Body != "A" {
		t.Fatalf("source Lookup(a) = %+v, %v; Compose mutated the source set", sk, ok)
	}
	if len(src.All()) != 3 {
		t.Fatal("Compose mutated the source set")
	}
}

func TestComposeFirstEntryOfANameWins(t *testing.T) {
	src := &Set{skills: []Skill{{Name: "a", Body: "A"}}}
	aCarried := NewCarried("a", "carried a", "carried A")
	got := src.Compose([]Entry{{Name: "a", Carried: &aCarried}, {Name: "a"}})
	if len(got.All()) != 1 || got.All()[0].Body != "carried A" {
		t.Fatalf("Compose = %+v, want only the carried a", got.All())
	}
}

func TestComposeOnNilSetUsesCarriedOnly(t *testing.T) {
	x := NewCarried("x", "does x", "X")
	got := (*Set)(nil).Compose([]Entry{{Name: "x", Carried: &x}, {Name: "a"}})
	if n := strings.Join(composedNames(got), ","); n != "x" {
		t.Fatalf("nil Compose names = %s, want x", n)
	}
}

func TestComposedSetListsCarriedSkillsInThePromptBlock(t *testing.T) {
	src := &Set{skills: []Skill{{Name: "a", Description: "installed a"}}}
	x := NewCarried("x", "Sort issues: label and size", "X")
	block := src.Compose([]Entry{{Name: "a"}, {Name: "x", Carried: &x}}).SystemPromptBlock()
	if !strings.Contains(block, "- x: Sort issues: label and size") {
		t.Fatalf("prompt block does not list the carried skill:\n%s", block)
	}
	if !strings.Contains(block, "- a: installed a") {
		t.Fatalf("prompt block does not list the installed skill:\n%s", block)
	}
}
