package tui

import (
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/permissions"
)

// TestModelsCommand_ListsHeaderAndActiveRow asserts `/models` prints the
// header and the active model's row with its marker, and does not change
// the active alias.
func TestModelsCommand_ListsHeaderAndActiveRow(t *testing.T) {
	sm := permissions.NewSessionMode(permissions.ModeAuto)
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder, sm, true))
	m.lines = nil // drop banner/welcome lines so we inspect only what /models appends.

	next, _ := m.handleSlash("/models")
	out := plainLines(next.(ShellModel))

	if !strings.Contains(out, "Available models:") {
		t.Errorf("/models should print the header; got:\n%s", out)
	}
	if !strings.Contains(out, modelsActiveMarker+"alpha") {
		t.Errorf("/models should print the active model's row with its marker; got:\n%s", out)
	}
	if got := next.(ShellModel).alias; got != "alpha" {
		t.Errorf("/models must not change the active alias; got %q, want %q", got, "alpha")
	}
}

// TestModelsCommand_DoesNotAffectModelSwitch asserts `/model NAME` still
// switches the active model as before, unaffected by the presence of /models.
func TestModelsCommand_DoesNotAffectModelSwitch(t *testing.T) {
	sm := permissions.NewSessionMode(permissions.ModeAuto)
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder, sm, true))
	m.lines = nil

	next, _ := m.handleSlash("/model beta")
	if got := next.(ShellModel).alias; got != "beta" {
		t.Fatalf("/model beta should switch the active alias; got %q, want %q", got, "beta")
	}
}
