package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
)

// models_harness_test.go drives the model UI end-to-end through the real
// teatest harness (a live bubbletea program), not by poking Update/View
// directly. Each test types the actual keystrokes a user would and captures the
// rendered terminal frame via the harness snapshot path, so the assertions run
// against what the terminal truly shows.

// richModelRegistry is a registry with a few aliases of varied widths so the
// listing/editor frames exercise column alignment and markers.
func richModelRegistry() *model.Registry {
	return model.NewRegistry("glm", map[string]model.ModelConfig{
		"glm":            {ID: "cloud/glm-5.2", MaxTokens: 16384, ContextWindow: 131072, Persona: "general"},
		"deepseek-flash": {ID: "cloud/deepseek-v4-flash", MaxTokens: 16384, Persona: "coding"},
		"qwen-local":     {ID: "local/qwen-7b", MaxTokens: 4096, Persona: "general"},
	})
}

// modelsHarness boots a harness whose ShellModel carries a real builtin slash
// registry (so /models and /model dispatch) and the given model registry, with
// HOME pointed at a temp dir so the editor's config writes are hermetic.
func modelsHarness(t *testing.T, reg *model.Registry) *harness {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	slashReg := NewSlashRegistry(NewBuiltinProvider())
	t.Cleanup(slashReg.Close)

	build := func(alias string, r agent.Renderer, _ permissions.ApprovalFunc) (*agent.Agent, error) {
		return nil, nil // no agent needed; these tests only render UI
	}
	m := NewShellModel("glm", false, "", reg, slashReg, build,
		permissions.NewSessionMode(permissions.ModeSmart), true)
	return startHarnessWithModelSized(t, m, 100, 24)
}

func TestHarness_ModelsListing(t *testing.T) {
	h := modelsHarness(t, richModelRegistry())
	h.typeAndSubmit("/models")

	// The listing prints synchronously into the transcript on Enter.
	h.waitForOutput("cloud/glm-5.2", 2*time.Second)

	frame := h.snapshot("models-listing")
	// The listing is renderModelsListing's format: a header line plus one row
	// per alias, with the default/active tag on the active row.
	for _, want := range []string{"Available models:", "glm", "deepseek-flash", "cloud/glm-5.2", "(default, active)"} {
		if !strings.Contains(frame, want) {
			t.Errorf("listing frame missing %q; frame:\n%s", want, frame)
		}
	}
}

func TestHarness_ModelArgAutocomplete(t *testing.T) {
	h := modelsHarness(t, richModelRegistry())

	// Type "/model " (note trailing space) WITHOUT submitting: the completer
	// overlay should switch to alias completion. The completer's command column
	// may truncate the "/model <alias>" label, but the description column
	// carries the un-truncated gateway id, so assert on that.
	for _, r := range "/model deep" {
		h.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	h.waitForOutput("cloud/deepseek-v4-flash", 2*time.Second)

	// The overlay swallows keys, so tear down via Quit and capture the overlay.
	frame := captureOverlayFrame(t, h.tm, "model-arg-autocomplete")
	if !strings.Contains(frame, "cloud/deepseek-v4-flash") {
		t.Errorf("autocomplete frame missing deepseek entry; frame:\n%s", frame)
	}
	// The unrelated alias must be filtered out by the "deep" prefix.
	if strings.Contains(frame, "qwen-local") || strings.Contains(frame, "local/qwen-7b") {
		t.Errorf("autocomplete should have filtered out qwen-local; frame:\n%s", frame)
	}
}

func TestHarness_ModelsEditorOverlay(t *testing.T) {
	h := modelsHarness(t, richModelRegistry())

	h.typeAndSubmit("/models edit")
	h.waitForOutput("Model mappings", 2*time.Second)

	frame := captureOverlayFrame(t, h.tm, "models-editor-list")
	for _, want := range []string{"Model mappings", "glm", "deepseek-flash", "add", "edit", "delete"} {
		if !strings.Contains(frame, want) {
			t.Errorf("editor list frame missing %q; frame:\n%s", want, frame)
		}
	}
}

func TestHarness_ModelsEditorAddForm(t *testing.T) {
	h := modelsHarness(t, richModelRegistry())

	h.typeAndSubmit("/models edit")
	h.waitForOutput("Model mappings", 2*time.Second)

	// Open the add form and type an alias + partial id.
	h.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	for _, r := range "shiny" {
		h.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	h.tm.Send(tea.KeyMsg{Type: tea.KeyTab})
	for _, r := range "cloud/shiny-1" {
		h.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	h.waitForOutput("Add model", 2*time.Second)

	frame := captureOverlayFrame(t, h.tm, "models-editor-add-form")
	for _, want := range []string{"Add model", "alias", "model id", "persona", "shiny", "cloud/shiny-1", "Enter save"} {
		if !strings.Contains(frame, want) {
			t.Errorf("add-form frame missing %q; frame:\n%s", want, frame)
		}
	}
}
