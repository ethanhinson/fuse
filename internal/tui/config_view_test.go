package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/tools"
)

// configShell builds a sized ShellModel with an isolated HOME, so any config
// write a mis-routed key triggers lands in a temp dir rather than the user's
// real ~/.fuse/config.yml.
func configShell(t *testing.T, reg *model.Registry) ShellModel {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	return sized(NewShellModel("alpha", false, "dark", reg, nil, nilBuilder,
		permissions.NewSessionMode(permissions.ModeSmart), true))
}

func configRegistry() *model.Registry {
	return model.NewRegistry("alpha", map[string]model.ModelConfig{
		"alpha": {ID: "prov/alpha", Persona: "coding"},
		"beta":  {ID: "prov/beta"},
	})
}

// openConfigVia drives the real typed-slash path, not the constructor.
func openConfigVia(t *testing.T, m ShellModel) ShellModel {
	t.Helper()
	next, _ := m.handleSlash("/config")
	return next.(ShellModel)
}

// configKey drives a key through the real ShellModel key path.
func configKey(m ShellModel, msg tea.KeyMsg) ShellModel {
	next, _ := m.handleKey(msg)
	return next.(ShellModel)
}

func runeKey(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

// TestConfigOpensAndCloses: /config opens the overlay and esc closes it,
// both through the real key path.
func TestConfigOpensAndCloses(t *testing.T) {
	m := openConfigVia(t, configShell(t, configRegistry()))
	if m.config == nil {
		t.Fatal("/config did not open the overlay")
	}
	if m.modelsEdit == nil {
		t.Fatal("/config must share the models editor state so both routes use one set of handlers")
	}
	m = configKey(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.config != nil {
		t.Error("esc should close the /config overlay")
	}
	if m.modelsEdit != nil {
		t.Error("closing /config must release the shared models editor state")
	}
}

// TestConfigRegisteredInSlashRegistry: /config completes in the slash menu
// alongside the other builtins.
func TestConfigRegisteredInSlashRegistry(t *testing.T) {
	for _, e := range NewBuiltinProvider().Commands() {
		if e.Command == "/config" {
			if e.Kind != KindBuiltin {
				t.Errorf("/config kind = %q, want builtin", e.Kind)
			}
			return
		}
	}
	t.Fatal("/config is not registered as a builtin slash command")
}

// TestConfigTabsRenderWithoutPanicking covers every tab against both a
// populated session and the degenerate one: empty registry, no MCP servers.
func TestConfigTabsRenderWithoutPanicking(t *testing.T) {
	cases := map[string]*model.Registry{
		"populated": configRegistry(),
		"empty":     model.NewRegistry("", map[string]model.ModelConfig{}),
	}
	for name, reg := range cases {
		t.Run(name, func(t *testing.T) {
			m := openConfigVia(t, configShell(t, reg))
			for tab := 0; tab < m.config.tabs.Len(); tab++ {
				m.config.tabs.SetActive(tab)
				lines := configOverlayLines(m.config, 80, configOverlayHeight)
				if len(lines) == 0 {
					t.Fatalf("tab %d rendered nothing", tab)
				}
				if !strings.Contains(plainStr(strings.Join(lines, "\n")), "Config") {
					t.Errorf("tab %d render lost the overlay title", tab)
				}
			}
		})
	}
}

// TestConfigOverlayLinesFitWidth: every rendered line stays inside the budget
// it was given, on every tab, at narrow widths where the tab bar itself has to
// shrink (change 0041 — separator glyphs spend the same cells).
func TestConfigOverlayLinesFitWidth(t *testing.T) {
	m := openConfigVia(t, configShell(t, configRegistry()))
	for _, width := range []int{20, 30, 40, 60, 80, 120} {
		for tab := 0; tab < m.config.tabs.Len(); tab++ {
			m.config.tabs.SetActive(tab)
			for i, line := range configOverlayLines(m.config, width, configOverlayHeight) {
				if w := lipgloss.Width(line); w > width {
					t.Errorf("width=%d tab=%d line %d is %d cells: %q", width, tab, i, w, line)
				}
			}
		}
	}
}

// TestConfigTabSwitchingThroughShellModel drives tab/shift+tab/direct-index
// through the REAL ShellModel key path, not by calling Tabs directly.
func TestConfigTabSwitchingThroughShellModel(t *testing.T) {
	m := openConfigVia(t, configShell(t, configRegistry()))
	n := m.config.tabs.Len()
	if n != 3 {
		t.Fatalf("want 3 tabs, got %d", n)
	}
	m = configKey(m, tea.KeyMsg{Type: tea.KeyTab})
	if got := m.config.tabs.Active(); got != 1 {
		t.Fatalf("after tab, active = %d, want 1", got)
	}
	m = configKey(m, tea.KeyMsg{Type: tea.KeyShiftTab})
	if got := m.config.tabs.Active(); got != 0 {
		t.Fatalf("after shift+tab, active = %d, want 0", got)
	}
	// shift+tab from the first tab wraps to the last rather than cycling the
	// permission mode (the shell's usual shift+tab binding).
	m = configKey(m, tea.KeyMsg{Type: tea.KeyShiftTab})
	if got := m.config.tabs.Active(); got != n-1 {
		t.Fatalf("shift+tab should wrap to %d, got %d", n-1, got)
	}
	if mode := m.sessionMode.Get(); mode != permissions.ModeSmart {
		t.Errorf("shift+tab inside /config must not cycle the permission mode; got %v", mode)
	}
	m = configKey(m, runeKey('2'))
	if got := m.config.tabs.Active(); got != 1 {
		t.Fatalf("direct index '2' -> active %d, want 1", got)
	}
}

// TestConfigModelsTabReachesEditorHandlers: the Models tab's add/edit/remove
// land in the same handlers /models edit drives, acting on the selected ROW
// OBJECT rather than a re-parsed command string.
func TestConfigModelsTabReachesEditorHandlers(t *testing.T) {
	t.Run("add", func(t *testing.T) {
		m := openConfigVia(t, configShell(t, configRegistry()))
		m = configKey(m, runeKey('a'))
		if m.modelsEdit.form == nil {
			t.Fatal("'a' should open the add form via handleModelsEditorKey")
		}
		if m.modelsEdit.form.editing {
			t.Error("add form must not be in editing mode")
		}
		// The form owns every key while open, including tab — the tab container
		// must not steal it to switch panes.
		before := m.config.tabs.Active()
		m = configKey(m, tea.KeyMsg{Type: tea.KeyTab})
		if m.config.tabs.Active() != before {
			t.Error("tab inside the model form must move fields, not tabs")
		}
		if m.modelsEdit.form.field != fieldID {
			t.Errorf("tab should advance the form field, got %v", m.modelsEdit.form.field)
		}
		// Esc dismisses the form but leaves /config open.
		m = configKey(m, tea.KeyMsg{Type: tea.KeyEsc})
		if m.modelsEdit.form != nil {
			t.Error("esc should close the form")
		}
		if m.config == nil {
			t.Error("esc on the form must not close the whole /config overlay")
		}
	})

	t.Run("edit dispatches on the selected row object", func(t *testing.T) {
		m := openConfigVia(t, configShell(t, configRegistry()))
		m = configKey(m, runeKey('j')) // move to the second row
		want := m.modelsEdit.rows[m.modelsEdit.cursor]
		m = configKey(m, runeKey('e'))
		f := m.modelsEdit.form
		if f == nil || !f.editing {
			t.Fatal("'e' should open the edit form via handleModelsEditorKey")
		}
		if f.origAlias != want.Alias || f.id != want.Config.ID {
			t.Errorf("form built from %q/%q, want the selected row %q/%q",
				f.origAlias, f.id, want.Alias, want.Config.ID)
		}
	})

	t.Run("remove", func(t *testing.T) {
		m := openConfigVia(t, configShell(t, configRegistry()))
		// The cursor sits on the default alias, which deleteSelectedModel
		// refuses with a status banner — a handler-identifying result that
		// touches no config file.
		m = configKey(m, runeKey('d'))
		if !strings.Contains(plainStr(m.modelsEdit.status), "default model") {
			t.Errorf("'d' should reach deleteSelectedModel; status = %q", m.modelsEdit.status)
		}
	})
}

// TestConfigModelsEditStillOpensDirectly: /models edit keeps working as its own
// entry point, with no /config overlay attached.
func TestConfigModelsEditStillOpensDirectly(t *testing.T) {
	m := configShell(t, configRegistry())
	next, _ := m.handleSlash("/models edit")
	m = next.(ShellModel)
	if m.modelsEdit == nil {
		t.Fatal("/models edit should still open the editor")
	}
	if m.config != nil {
		t.Fatal("/models edit must not open the /config overlay")
	}
	m = configKey(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.modelsEdit != nil {
		t.Error("esc should still close the standalone editor")
	}
}

// TestConfigApprovalsAndAsksKeepPriority: /config sits BELOW approvals and asks
// in the guard order — it changes nothing about that precedence.
func TestConfigApprovalsAndAsksKeepPriority(t *testing.T) {
	t.Run("approval", func(t *testing.T) {
		m := openConfigVia(t, configShell(t, configRegistry()))
		respCh := make(chan approvalResponse, 1)
		m.approvals = append(m.approvals, approvalState{req: PermissionRequestMsg{
			Request: permissions.ApprovalRequest{ToolName: "bash", Preview: "bash: ls"},
			RespCh:  respCh,
		}})
		// 'a' would open the add form if /config had priority.
		m = configKey(m, runeKey('y'))
		if len(m.approvals) != 0 {
			t.Fatal("the pending approval should have consumed the key")
		}
		if resp := <-respCh; !resp.Approved {
			t.Error("approval answer did not reach the gate")
		}
		if m.config == nil {
			t.Error("/config should still be open behind the approval")
		}
		if m.modelsEdit != nil && m.modelsEdit.form != nil {
			t.Error("the key must not have reached the models editor")
		}
	})

	t.Run("ask", func(t *testing.T) {
		m := openConfigVia(t, configShell(t, configRegistry()))
		respCh := make(chan tools.Answer, 1)
		next, _ := m.Update(AskQuestionMsg{Question: sampleQuestion(), RespCh: respCh})
		m = next.(ShellModel)
		if len(m.asks) != 1 {
			t.Fatalf("asks len = %d, want 1", len(m.asks))
		}
		m = configKey(m, tea.KeyMsg{Type: tea.KeyEnter})
		if len(m.asks) != 0 {
			t.Fatal("the pending question should have consumed the key")
		}
		if m.config == nil {
			t.Error("/config should still be open behind the question")
		}
	})
}

// TestConfigMCPTabListsServers: the MCP pane is a read-only projection of the
// configured servers, and says so when there are none.
func TestConfigMCPTabListsServers(t *testing.T) {
	reg := NewSlashRegistry(&staticMCPProvider{entries: []SlashEntry{
		{Command: "/mcp:files/read", Kind: KindMCP, Server: "files"},
		{Command: "/mcp:files/write", Kind: KindMCP, Server: "files"},
		{Command: "/mcp:web/fetch", Kind: KindMCP, Server: "web"},
	}})
	defer reg.Close()

	st := newConfigState(&modelsEditState{}, permissions.NewSessionMode(permissions.ModeSmart), reg)
	st.tabs.SetActive(configTabMCP)
	out := plainStr(strings.Join(configOverlayLines(st, 80, configOverlayHeight), "\n"))
	for _, want := range []string{"files", "web"} {
		if !strings.Contains(out, want) {
			t.Errorf("MCP tab missing server %q in:\n%s", want, out)
		}
	}

	empty := newConfigState(&modelsEditState{}, permissions.NewSessionMode(permissions.ModeSmart), nil)
	empty.tabs.SetActive(configTabMCP)
	out = plainStr(strings.Join(configOverlayLines(empty, 80, configOverlayHeight), "\n"))
	if !strings.Contains(out, "no MCP servers") {
		t.Errorf("empty MCP tab should say so, got:\n%s", out)
	}
}

// TestConfigPermissionsTabIsReadOnly: the Permissions pane reports the session
// mode and does not mutate it.
func TestConfigPermissionsTabIsReadOnly(t *testing.T) {
	m := openConfigVia(t, configShell(t, configRegistry()))
	m.config.tabs.SetActive(configTabPermissions)
	out := plainStr(strings.Join(configOverlayLines(m.config, 80, configOverlayHeight), "\n"))
	if !strings.Contains(out, permissions.ModeSmart.String()) {
		t.Errorf("permissions tab should report the current mode, got:\n%s", out)
	}
	// Every key the pane does not use is swallowed, and none of them change mode.
	for _, r := range []rune{'a', 'd', 'e', 'x'} {
		m = configKey(m, runeKey(r))
	}
	if got := m.sessionMode.Get(); got != permissions.ModeSmart {
		t.Errorf("permissions tab is read-only; mode changed to %v", got)
	}
	if m.modelsEdit.form != nil {
		t.Error("keys on the permissions tab must not reach the models editor form")
	}
}

// staticMCPProvider is a fixed CommandProvider standing in for MCPProvider.
type staticMCPProvider struct{ entries []SlashEntry }

func (p *staticMCPProvider) Commands() []SlashEntry   { return p.entries }
func (p *staticMCPProvider) Changes() <-chan struct{} { return nil }
func (p *staticMCPProvider) Close()                   {}

func plainStr(s string) string { return ansiRE.ReplaceAllString(s, "") }
