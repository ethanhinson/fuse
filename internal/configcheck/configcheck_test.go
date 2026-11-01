package configcheck

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
)

func TestRegistryOverlaysConfigEntries(t *testing.T) {
	cfg := config.Default()
	cfg.Models.Default = "mine"
	cfg.Models.Entries = map[string]config.ModelConfig{
		"mine": {ID: "cloud/mine", MaxTokens: 10},
		"glm":  {ID: "cloud/glm-override"},
	}
	reg := Registry(cfg)
	if reg.Default != "mine" {
		t.Fatalf("Default = %q", reg.Default)
	}
	if mc, err := reg.Resolve("mine"); err != nil || mc.ID != "cloud/mine" || mc.MaxTokens != 10 {
		t.Fatalf("Resolve(mine) = %+v, %v", mc, err)
	}
	if mc, _ := reg.Resolve("glm"); mc.ID != "cloud/glm-override" {
		t.Fatalf("config entry did not replace built-in: %+v", mc)
	}
	if !reg.Has("deepseek-flash") {
		t.Fatal("built-in aliases lost")
	}
}

func TestModelRefProblemsPathsAndText(t *testing.T) {
	cfg := config.Default()
	cfg.Models.Default = "nope"
	cfg.Context.Summarization.Model = "ghost"
	cfg.Context.Relevance.ClassifierModel = "ghost"
	cfg.Permissions.Auto.ClassifierModel = "ghost"
	cfg.Orchestrator.Model = "ghost"
	ps := ModelRefProblems(cfg, Registry(cfg))
	want := []string{"models.default", "context.summarization.model", "context.relevance.classifier_model", "permissions.auto.classifier_model", "orchestrator.model"}
	if len(ps) != len(want) {
		t.Fatalf("got %d problems: %+v", len(ps), ps)
	}
	for i, p := range ps {
		if p.Path != want[i] {
			t.Errorf("problem %d path = %q, want %q", i, p.Path, want[i])
		}
	}
	if got := ps[0].Error(); got != "config: models.default references unknown model \"nope\"; run `fuse models` to list configured aliases" {
		t.Errorf("CLI text changed: %q", got)
	}
	if ps[0].Message != `references unknown model "nope"` {
		t.Errorf("Message = %q", ps[0].Message)
	}
}

func TestProblemsCombinesStructuralAndModelRefs(t *testing.T) {
	cfg := config.Default()
	cfg.MaxTokens = -1
	cfg.Orchestrator.Model = "ghost"
	ps := Problems(cfg)
	if len(ps) != 2 || ps[0].Path != "max_tokens" || ps[1].Path != "orchestrator.model" {
		t.Fatalf("Problems = %+v", ps)
	}
	if len(Problems(config.Default())) != 0 {
		t.Fatal("default config must be clean")
	}
}

func TestRegistryCarriesModelMetadata(t *testing.T) {
	no := false
	cfg := config.Default()
	cfg.Models.Entries = map[string]config.ModelConfig{
		"fast":   {ID: "cloud/fast", Label: "Fast coder", Description: "Cheap", Tags: []string{"tools", "vision"}},
		"hidden": {ID: "cloud/hidden", Advertise: &no},
	}
	reg := Registry(cfg)
	fast, err := reg.Resolve("fast")
	if err != nil {
		t.Fatal(err)
	}
	if fast.Label != "Fast coder" || fast.Description != "Cheap" || !reflect.DeepEqual(fast.Tags, []string{"tools", "vision"}) || fast.Hidden {
		t.Fatalf("fast = %+v", fast)
	}
	hidden, err := reg.Resolve("hidden")
	if err != nil {
		t.Fatal(err)
	}
	if !hidden.Hidden {
		t.Fatalf("hidden = %+v, want Hidden", hidden)
	}
}

func TestProblemsFlagsOverlongInstructions(t *testing.T) {
	cfg := config.Default()
	cfg.Instructions = strings.Repeat("a", config.MaxInstructionsChars+1)
	found := false
	for _, p := range Problems(cfg) {
		if p.Path == "instructions" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected a problem at instructions")
	}
}
