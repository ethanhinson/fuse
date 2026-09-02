package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
)

// editorShell builds a sized ShellModel whose registry is isolated per test and
// whose config writes land in a temp HOME.
func editorShell(t *testing.T, reg *model.Registry) ShellModel {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	m := sized(NewShellModel("alpha", false, "dark", reg, nil, nilBuilder,
		permissions.NewSessionMode(permissions.ModeSmart), true))
	return m
}

func typeRunes(m ShellModel, s string) ShellModel {
	for _, r := range s {
		handled, next, _ := m.handleModelsEditorKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		if !handled {
			panic("key not handled")
		}
		m = next.(ShellModel)
	}
	return m
}

func key(m ShellModel, t tea.KeyType) ShellModel {
	_, next, _ := m.handleModelsEditorKey(tea.KeyMsg{Type: t})
	return next.(ShellModel)
}

func keyStr(m ShellModel, s string) ShellModel {
	_, next, _ := m.handleModelsEditorKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
	return next.(ShellModel)
}

func TestModelsEditorOpenAndClose(t *testing.T) {
	m := editorShell(t, model.NewRegistry("alpha", map[string]model.ModelConfig{"alpha": {ID: "prov/alpha"}}))
	next, _ := m.openModelsEditor()
	m = next.(ShellModel)
	if m.modelsEdit == nil {
		t.Fatal("editor not open")
	}
	if len(m.modelsEdit.rows) != 1 {
		t.Errorf("want 1 row, got %d", len(m.modelsEdit.rows))
	}
	// Esc closes.
	m = key(m, tea.KeyEsc)
	if m.modelsEdit != nil {
		t.Error("editor should be closed after Esc")
	}
}

func TestModelsEditorAddPersistsAndMutatesRegistry(t *testing.T) {
	reg := model.NewRegistry("alpha", map[string]model.ModelConfig{"alpha": {ID: "prov/alpha"}})
	m := editorShell(t, reg)
	next, _ := m.openModelsEditor()
	m = next.(ShellModel)

	// 'a' opens the add form; fill alias, id, persona, tokens.
	m = keyStr(m, "a")
	if m.modelsEdit.form == nil {
		t.Fatal("add form not open")
	}
	m = typeRunes(m, "shiny")     // alias (starts on fieldAlias for add)
	m = key(m, tea.KeyTab)        // -> id
	m = typeRunes(m, "cloud/new") // id
	m = key(m, tea.KeyTab)        // -> persona
	m = typeRunes(m, "coding")    // persona
	m = key(m, tea.KeyTab)        // -> max tokens
	m = typeRunes(m, "8192")      // max tokens
	m = key(m, tea.KeyEnter)      // save

	// Live registry updated.
	mc, err := reg.Resolve("shiny")
	if err != nil {
		t.Fatalf("alias not in live registry: %v", err)
	}
	if mc.ID != "cloud/new" || mc.MaxTokens != 8192 || mc.Persona != "coding" {
		t.Errorf("live model = %+v", mc)
	}
	// Persisted to config.
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Models.Entries["shiny"]; got.ID != "cloud/new" || got.MaxTokens != 8192 {
		t.Errorf("persisted model = %+v", got)
	}
	// Form closed, status set.
	if m.modelsEdit.form != nil {
		t.Error("form should close after save")
	}
}

func TestModelsEditorAddValidationErrors(t *testing.T) {
	reg := model.NewRegistry("alpha", map[string]model.ModelConfig{"alpha": {ID: "prov/alpha"}})
	m := editorShell(t, reg)
	next, _ := m.openModelsEditor()
	m = next.(ShellModel)

	m = keyStr(m, "a")
	// Empty alias -> error, form stays open.
	m = key(m, tea.KeyEnter)
	if m.modelsEdit.form == nil {
		t.Fatal("form should stay open on validation error")
	}
	if m.modelsEdit.status == "" {
		t.Error("expected an error status for empty alias")
	}

	// Alias but no id -> still an error.
	m = typeRunes(m, "x")
	m = key(m, tea.KeyEnter)
	if m.modelsEdit.form == nil {
		t.Error("form should stay open when id is empty")
	}
}

func TestModelsEditorAddDuplicateRejected(t *testing.T) {
	reg := model.NewRegistry("alpha", map[string]model.ModelConfig{"alpha": {ID: "prov/alpha"}})
	m := editorShell(t, reg)
	next, _ := m.openModelsEditor()
	m = next.(ShellModel)

	m = keyStr(m, "a")
	m = typeRunes(m, "alpha") // already exists
	m = key(m, tea.KeyTab)
	m = typeRunes(m, "cloud/dup")
	m = key(m, tea.KeyEnter)
	if m.modelsEdit.form == nil {
		t.Fatal("duplicate add should keep the form open")
	}
	if m.modelsEdit.status == "" {
		t.Error("expected a duplicate-alias error")
	}
}

func TestModelsEditorEditPreservesAliasAndUpdates(t *testing.T) {
	reg := model.NewRegistry("alpha", map[string]model.ModelConfig{
		"alpha": {ID: "prov/alpha"},
		"beta":  {ID: "prov/beta", MaxTokens: 100, SystemPrefix: "/no_think"},
	})
	m := editorShell(t, reg)
	next, _ := m.openModelsEditor()
	m = next.(ShellModel)

	// Cursor to 'beta' (entries are alias-sorted: alpha, beta).
	m = keyStr(m, "j")
	if m.modelsEdit.rows[m.modelsEdit.cursor].Alias != "beta" {
		t.Fatalf("cursor on %q, want beta", m.modelsEdit.rows[m.modelsEdit.cursor].Alias)
	}
	// 'e' opens edit; alias is locked, so field starts on id.
	m = keyStr(m, "e")
	if m.modelsEdit.form == nil || !m.modelsEdit.form.editing {
		t.Fatal("edit form not open")
	}
	if m.modelsEdit.form.field != fieldID {
		t.Errorf("edit should start on id field, got %d", m.modelsEdit.form.field)
	}
	// Replace the id (clear then type).
	for range "prov/beta" {
		m = key(m, tea.KeyBackspace)
	}
	m = typeRunes(m, "cloud/beta2")
	m = key(m, tea.KeyEnter)

	mc, _ := reg.Resolve("beta")
	if mc.ID != "cloud/beta2" {
		t.Errorf("edited id = %q, want cloud/beta2", mc.ID)
	}
	// SystemPrefix, which the form does not expose, is preserved.
	if mc.SystemPrefix != "/no_think" {
		t.Errorf("system prefix lost on edit: %q", mc.SystemPrefix)
	}
}

func TestModelsEditorDeleteNonDefault(t *testing.T) {
	reg := model.NewRegistry("alpha", map[string]model.ModelConfig{
		"alpha": {ID: "prov/alpha"},
		"beta":  {ID: "prov/beta"},
	})
	m := editorShell(t, reg)
	// Seed config so removal has something to remove.
	if err := config.SetModel("beta", config.ModelConfig{ID: "prov/beta"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	next, _ := m.openModelsEditor()
	m = next.(ShellModel)

	m = keyStr(m, "j") // -> beta
	m = keyStr(m, "d") // delete

	if reg.Has("beta") {
		t.Error("beta should be gone from live registry")
	}
	cfg, _ := config.Load()
	if _, ok := cfg.Models.Entries["beta"]; ok {
		t.Error("beta should be gone from config")
	}
}

func TestModelsEditorDeleteDefaultRefused(t *testing.T) {
	reg := model.NewRegistry("alpha", map[string]model.ModelConfig{"alpha": {ID: "prov/alpha"}})
	m := editorShell(t, reg)
	next, _ := m.openModelsEditor()
	m = next.(ShellModel)

	// Cursor on 'alpha' (the default). Delete must refuse.
	m = keyStr(m, "d")
	if !reg.Has("alpha") {
		t.Error("default alias must not be deletable")
	}
	if m.modelsEdit.status == "" {
		t.Error("expected a refusal status when deleting the default")
	}
}

func TestModelFormFieldNavigationSkipsLockedAlias(t *testing.T) {
	f := &modelForm{editing: true, field: fieldID}
	// prevField from id should wrap to context window, not the locked alias.
	f.prevField()
	if f.field != fieldContextWindow {
		t.Errorf("prevField from id = %d, want fieldContextWindow", f.field)
	}
	// nextField cycling around must never land on fieldAlias while editing.
	f.field = fieldContextWindow
	f.nextField()
	if f.field == fieldAlias {
		t.Error("nextField landed on locked alias during edit")
	}
}

// TestModelsEditorOverlayClampsToWidth is the regression guard for the editor
// opting out of RenderTable's render-width clamp (it passed width 0, meaning
// unbounded). All three columns are padded to their GLOBAL max, so one
// over-wide alias widened every row, and the only remaining guard —
// fitLine(ol, width) — truncates from the RIGHT: it ate the persona column on
// every row to pay for the one wide entry.
//
// The witness is deliberately NOT "the line is <= width": change 0066's
// learning is that a width assertion cannot see suffix-eating truncation,
// because the fit produces exactly the width being asserted. The real witness
// is that the TRAILING column survives verbatim. The width check is kept only
// as a secondary sanity assertion.
func TestModelsEditorOverlayClampsToWidth(t *testing.T) {
	cases := []struct {
		name      string
		longAlias string
	}{
		// Pure ASCII: a multibyte-only fixture can pass for the wrong reason,
		// because a byte budget and a cell budget agree on neither direction.
		{"ascii", strings.Repeat("z", 60)},
		{"wide-runes", strings.Repeat("宽", 30)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := model.NewRegistry("alpha", map[string]model.ModelConfig{
				"alpha":      {ID: "prov/alpha", Persona: "coding"},
				tc.longAlias: {ID: "prov/long", Persona: "research"},
			})
			m := editorShell(t, reg)
			next, _ := m.openModelsEditor()
			m = next.(ShellModel)

			base := strings.TrimSuffix(strings.Repeat("\n", 40), "\n")
			for _, width := range []int{40, 60, 80} {
				out := renderModelsEditorOverlay(base, m.modelsEdit, width)
				lines := strings.Split(out, "\n")
				for i, ln := range lines {
					if w := lipgloss.Width(ln); w > width {
						t.Errorf("width %d: line %d is %d cells wide: %q", width, i, w, stripANSIString(ln))
					}
				}
				// The persona column is last; it must survive as the trailing
				// content of its row rather than being clipped away.
				for _, persona := range []string{"coding", "research"} {
					found := false
					for _, ln := range lines {
						if strings.HasSuffix(strings.TrimRight(stripANSIString(ln), " "), persona) {
							found = true
							break
						}
					}
					if !found {
						t.Errorf("width %d: persona %q did not survive as trailing row content; rows:\n%s",
							width, persona, stripANSIString(out))
					}
				}
			}
		})
	}
}
