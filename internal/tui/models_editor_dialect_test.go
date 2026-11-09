package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/model"
)

func TestToConfigModelCarriesDialectOptions(t *testing.T) {
	off := false
	got := toConfigModel(model.ModelConfig{ID: "cloud/x", DialectOptions: model.DialectOptions{CacheMarkers: &off, ReasoningField: "reasoning"}})
	if got.DialectOptions.CacheMarkers == nil || *got.DialectOptions.CacheMarkers || got.DialectOptions.ReasoningField != "reasoning" {
		t.Fatalf("options = %+v", got.DialectOptions)
	}
}

func TestEditPreservesDialectOptions(t *testing.T) {
	on := true
	reg := model.NewRegistry("alpha", map[string]model.ModelConfig{
		"alpha": {ID: "prov/alpha"},
		"beta":  {ID: "prov/beta", DialectOptions: model.DialectOptions{CacheMarkers: &on, ReasoningField: "reasoning"}},
	})
	m := editorShell(t, reg)
	next, _ := m.openModelsEditor()
	m = next.(ShellModel)
	m = keyStr(m, "j")
	m = keyStr(m, "e")
	m = typeRunes(m, "x")
	m = key(m, tea.KeyEnter)
	mc, _ := reg.Resolve("beta")
	if mc.ID != "prov/betax" {
		t.Fatalf("edit did not apply: id = %q", mc.ID)
	}
	if mc.DialectOptions.CacheMarkers == nil || !*mc.DialectOptions.CacheMarkers || mc.DialectOptions.ReasoningField != "reasoning" {
		t.Errorf("dialect options lost on edit: %+v", mc.DialectOptions)
	}
}
