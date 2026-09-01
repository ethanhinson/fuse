package tui

import (
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/model"
)

func modelArgCompleter(t *testing.T) *slashCompleter {
	t.Helper()
	reg := completerReg(
		SlashEntry{Command: "/model", Syntax: "NAME", Kind: KindBuiltin, expand: func() string { return "/model " }},
		SlashEntry{Command: "/models", Kind: KindBuiltin, expand: func() string { return "/models" }},
	)
	t.Cleanup(reg.Close)
	mreg := model.NewRegistry("glm", map[string]model.ModelConfig{
		"glm":            {ID: "cloud/glm-5.2", Persona: "general"},
		"deepseek-flash": {ID: "cloud/deepseek-v4-flash", Persona: "coding"},
		"deepseek-pro":   {ID: "cloud/deepseek-v4-pro", Persona: "coding"},
	})
	return newSlashCompleter(reg).withModelArg(modelAliasCompletions(mreg))
}

func TestModelArgPrefix(t *testing.T) {
	cases := []struct {
		in     string
		arg    string
		wantOK bool
	}{
		{"/model ", "", true},
		{"/model gl", "gl", true},
		{"/model glm", "glm", true},
		{"/model", "", false},   // no space yet — still command completion
		{"/models", "", false},  // different command
		{"/models ", "", false}, // /models arg is not an alias slot
		{"/model glm ", "", false},
		{"/mode auto", "", false},
	}
	for _, c := range cases {
		arg, ok := modelArgPrefix(c.in)
		if ok != c.wantOK || (ok && arg != c.arg) {
			t.Errorf("modelArgPrefix(%q) = (%q, %v), want (%q, %v)", c.in, arg, ok, c.arg, c.wantOK)
		}
	}
}

func TestCompleterArgModeListsAliases(t *testing.T) {
	c := modelArgCompleter(t)
	c.activate("/model ")

	if !c.argMode {
		t.Fatal("expected argMode after '/model '")
	}
	if len(c.visible) != 3 {
		t.Fatalf("want 3 aliases, got %d: %v", len(c.visible), c.visible)
	}
	// Entries expand to the full "/model <alias>" command.
	got := c.visible[0].Expansion()
	if got != "/model deepseek-flash" { // entries are alias-sorted
		t.Errorf("first expansion = %q", got)
	}
}

func TestCompleterArgModeFiltersByPrefix(t *testing.T) {
	c := modelArgCompleter(t)
	c.activate("/model deep")

	if len(c.visible) != 2 {
		t.Fatalf("want 2 deepseek aliases, got %d: %v", len(c.visible), c.visible)
	}
	for _, e := range c.visible {
		if e.Expansion() != "/model deepseek-flash" && e.Expansion() != "/model deepseek-pro" {
			t.Errorf("unexpected entry %q", e.Expansion())
		}
	}
}

func TestCompleterCommandModeUnaffected(t *testing.T) {
	c := modelArgCompleter(t)
	// "/model" without a trailing space still completes command names.
	c.activate("/model")
	if c.argMode {
		t.Error("'/model' (no space) must not enter argMode")
	}
	// Both /model and /models match the command prefix.
	if len(c.visible) != 2 {
		t.Errorf("want 2 command matches, got %d: %v", len(c.visible), c.visible)
	}
}

func TestCompleterDeactivateClearsArgMode(t *testing.T) {
	c := modelArgCompleter(t)
	c.activate("/model glm")
	if !c.argMode {
		t.Fatal("expected argMode")
	}
	c.deactivate()
	if c.argMode {
		t.Error("deactivate must clear argMode")
	}
}

func TestModelAliasCompletionsMarksDefault(t *testing.T) {
	mreg := model.NewRegistry("glm", map[string]model.ModelConfig{
		"glm": {ID: "cloud/glm-5.2", Persona: "general"},
	})
	entries := modelAliasCompletions(mreg)("")
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if !strings.Contains(entries[0].Description, "(default)") {
		t.Errorf("default alias not marked: %q", entries[0].Description)
	}
	if !strings.Contains(entries[0].Description, "cloud/glm-5.2") {
		t.Errorf("description missing id: %q", entries[0].Description)
	}
}
