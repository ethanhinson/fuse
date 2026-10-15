package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/model"
)

func discoverShell(t *testing.T, lister ModelLister) ShellModel {
	t.Helper()
	m := configShell(t, configRegistry()).WithModelLister(lister)
	next, _ := m.handleSlash("/models")
	return next.(ShellModel)
}

// press sends a key and, when it yields a command, feeds the reply back in.
func pressAndSettle(m ShellModel, msg tea.KeyMsg) ShellModel {
	next, cmd := m.handleKey(msg)
	m = next.(ShellModel)
	if cmd != nil {
		if reply := cmd(); reply != nil {
			n2, _ := m.Update(reply)
			m = n2.(ShellModel)
		}
	}
	return m
}

func TestDiscoverListsNewModelsAndPrefillsForm(t *testing.T) {
	m := discoverShell(t, func(context.Context) ([]string, error) {
		// prov/alpha is already registered and must be hidden.
		return []string{"prov/alpha", "cloud/new-one", "cloud/new-two"}, nil
	})
	m = pressAndSettle(m, runeKey('n'))
	out := plainStr(m.View())
	if !strings.Contains(out, "cloud/new-one") || strings.Contains(out, "prov/alpha") {
		t.Fatalf("discover list wrong:\n%s", out)
	}
	m = pressAndSettle(m, tea.KeyMsg{Type: tea.KeyDown})
	m = pressAndSettle(m, tea.KeyMsg{Type: tea.KeyEnter})
	f := m.modelsEdit.form
	if f == nil || f.id != "cloud/new-two" || f.alias != "new-two" {
		t.Fatalf("form not prefilled: %+v", f)
	}
}

func TestDiscoverErrorAndEscBack(t *testing.T) {
	m := discoverShell(t, func(context.Context) ([]string, error) { return nil, errors.New("boom") })
	m = pressAndSettle(m, runeKey('n'))
	if !strings.Contains(plainStr(m.View()), "boom") {
		t.Fatal("error not shown")
	}
	m = pressAndSettle(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.modelsEdit == nil || m.modelsEdit.discover != nil {
		t.Fatal("esc should return to the list, not close the editor")
	}
}

func TestDiscoverWithoutGateway(t *testing.T) {
	m := discoverShell(t, nil)
	m = pressAndSettle(m, runeKey('n'))
	if !strings.Contains(plainStr(m.View()), "no gateway") {
		t.Fatal("missing no-gateway status")
	}
}

func catalogShell(t *testing.T) ShellModel {
	t.Helper()
	m := configShell(t, configRegistry()).
		WithModelLister(func(context.Context) ([]string, error) { return []string{"cloud/gw-only", "local/x"}, nil }).
		WithCatalogLister(func(context.Context) ([]model.CatalogModel, error) {
			return []model.CatalogModel{
				{ID: "acme/private-one", ContextLength: 131072, PromptPerM: 0.1, CompletionPerM: 0.4, ZDR: true},
				{ID: "acme/leaky-two", ContextLength: 8192, PromptPerM: 3, CompletionPerM: 15},
				{ID: "zeta/other", ContextLength: 1048576},
			}, nil
		})
	next, _ := m.handleSlash("/models")
	return next.(ShellModel)
}

func TestDiscoverTabSwitchesToCatalogWithPrivacyLabels(t *testing.T) {
	m := catalogShell(t)
	m = pressAndSettle(m, runeKey('n'))
	out := plainStr(m.View())
	if !strings.Contains(out, "cloud/gw-only") || !strings.Contains(out, "Tab: OpenRouter catalog") {
		t.Fatalf("gateway source first:\n%s", out)
	}
	if !strings.Contains(out, "local/x") || !strings.Contains(out, "local") {
		t.Errorf("local model should be labelled local:\n%s", out)
	}
	m = pressAndSettle(m, tea.KeyMsg{Type: tea.KeyTab})
	out = plainStr(m.View())
	for _, want := range []string{"private/acme/private-one", "zdr", "openrouter/acme/leaky-two", "may retain", "128k ctx", "$0.10/$0.40 per M"} {
		if !strings.Contains(out, want) {
			t.Errorf("catalog view missing %q:\n%s", want, out)
		}
	}
	// Tab again returns to the gateway list without refetching.
	m = pressAndSettle(m, tea.KeyMsg{Type: tea.KeyTab})
	if !strings.Contains(plainStr(m.View()), "cloud/gw-only") {
		t.Error("tab should cycle back to the gateway source")
	}
}

func TestDiscoverFilterAndPickRoutes(t *testing.T) {
	m := catalogShell(t)
	m = pressAndSettle(m, runeKey('n'))
	m = pressAndSettle(m, tea.KeyMsg{Type: tea.KeyTab})
	for _, r := range "leaky" {
		m = pressAndSettle(m, runeKey(r))
	}
	out := plainStr(m.View())
	if strings.Contains(out, "private-one") || !strings.Contains(out, "leaky-two") {
		t.Fatalf("filter did not narrow:\n%s", out)
	}
	// Esc clears the filter first, then leaves.
	m = pressAndSettle(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.modelsEdit.discover == nil || m.modelsEdit.discover.filter != "" {
		t.Fatal("first esc should clear the filter only")
	}
	for _, r := range "leaky" {
		m = pressAndSettle(m, runeKey(r))
	}
	m = pressAndSettle(m, tea.KeyMsg{Type: tea.KeyEnter})
	f := m.modelsEdit.form
	if f == nil || f.id != "openrouter/acme/leaky-two" || f.alias != "leaky-two" {
		t.Fatalf("form: %+v", f)
	}
	if !strings.Contains(plainStr(m.View()), "may keep or train") {
		t.Error("a may-retain pick should warn on the form")
	}
}

func TestDiscoverCatalogOnlyWhenNoGateway(t *testing.T) {
	m := configShell(t, configRegistry()).WithCatalogLister(func(context.Context) ([]model.CatalogModel, error) {
		return []model.CatalogModel{{ID: "a/b", ZDR: true}}, nil
	})
	next, _ := m.handleSlash("/models")
	m = pressAndSettle(next.(ShellModel), runeKey('n'))
	if out := plainStr(m.View()); !strings.Contains(out, "private/a/b") || strings.Contains(out, "Tab source") {
		t.Fatalf("catalog-only view:\n%s", out)
	}
}

func TestRegisteredListShowsPrivacyColumn(t *testing.T) {
	reg := model.NewRegistry("a", map[string]model.ModelConfig{
		"a": {ID: "local/qwen"}, "b": {ID: "private/x/y"}, "c": {ID: "openrouter/x/z"}, "d": {ID: "cloud/plain"},
	})
	m := configShell(t, reg)
	next, _ := m.handleSlash("/models")
	out := plainStr(next.(ShellModel).View())
	for _, want := range []string{"local", "zdr", "may retain"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}
