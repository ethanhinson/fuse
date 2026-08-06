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

// TestEmbeddedResearchCarriesBudgetGuidance locks in the runaway-fan-out fix:
// the embedded research skill must reference the injected spawn-budget line and
// require a final cited synthesis, so a future edit cannot silently drop the
// budget-aware guidance this change added.
func TestEmbeddedResearchCarriesBudgetGuidance(t *testing.T) {
	emb, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	var body string
	for i := range emb {
		if emb[i].Name == "research" {
			body = emb[i].Body
			break
		}
	}
	if body == "" {
		t.Fatal("embedded research skill not found or empty")
	}
	for _, want := range []string{
		"agent budget:",             // references the injected budget line
		"never count it",            // tells the model not to tally its own spawns
		"facet-researcher",          // change 0034: worker-typed spawns replace prose
		`worker: "facet-researcher"`, // the model passes the worker param
		"do the facet work directly", // the 0033-era fallback line when stripped
		"Completion contract",       // the final-synthesis requirement
		"numbered source list",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("embedded research body missing %q", want)
		}
	}
	// The unenforceable prose rules the workflow replaces must be GONE — their
	// enforcement now lives in the worker allowlist + pool, not in text a child
	// never sees (change 0034).
	if strings.Contains(body, "MUST NOT call spawn") {
		t.Error("research body should no longer carry the unenforceable 'MUST NOT call spawn' prose (0034)")
	}
}

// TestEmbeddedResearchPinsCitationStyle locks in the citation-style fix: the
// completion contract must MANDATE [N] numeric markers (not markdown-link
// citations) plus a numbered ## Sources list, so a model that would otherwise
// emit [title](url) citations is told the required form explicitly.
func TestEmbeddedResearchPinsCitationStyle(t *testing.T) {
	emb, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	var body string
	for i := range emb {
		if emb[i].Name == "research" {
			body = emb[i].Body
			break
		}
	}
	if body == "" {
		t.Fatal("embedded research skill not found or empty")
	}
	for _, want := range []string{
		"MANDATORY",       // the elements are non-optional
		"`[N]`",           // the required inline marker form is named
		"NOT inline markdown links", // markdown-link citations are excluded
		"## Sources",      // the numbered source list heading is required
	} {
		if !strings.Contains(body, want) {
			t.Errorf("embedded research citation contract missing %q", want)
		}
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
