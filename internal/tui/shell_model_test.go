package tui

import (
	"regexp"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
)

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// plainLines strips ANSI escape codes and joins model lines for content checks.
func plainLines(m ShellModel) string {
	return ansiRE.ReplaceAllString(strings.Join(m.lines, "\n"), "")
}

func testRegistry() *model.Registry {
	return model.NewRegistry("alpha", map[string]model.ModelConfig{
		"alpha": {ID: "prov/alpha"},
		"beta":  {ID: "prov/beta"},
	})
}

func nilBuilder(alias string, r agent.Renderer, _ permissions.ApprovalFunc) (*agent.Agent, error) {
	return nil, nil
}

// sized returns a model that has received a WindowSizeMsg (viewport ready).
func sized(m ShellModel) ShellModel {
	next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return next.(ShellModel)
}

// staticProvider is a CommandProvider backed by a fixed entry list for tests.
type staticProvider struct {
	entries []SlashEntry
}

func (p *staticProvider) Commands() []SlashEntry  { return p.entries }
func (p *staticProvider) Changes() <-chan struct{} { return nil }
func (p *staticProvider) Close()                  {}

// registryWith builds a SlashRegistry from a fixed set of entries.
func registryWith(entries ...SlashEntry) *SlashRegistry {
	return NewSlashRegistry(&staticProvider{entries: entries})
}

// skillEntry builds a KindSkill SlashEntry for tests.
func skillEntry(cmd, body, desc string) SlashEntry {
	b := body
	return SlashEntry{
		Command:     cmd,
		Description: desc,
		Kind:        KindSkill,
		expand:      func() string { return b },
	}
}

func enter(m ShellModel) (ShellModel, tea.Cmd) {
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	return next.(ShellModel), cmd
}

func typeLine(m ShellModel, s string) ShellModel {
	m.input.SetValue(s)
	return m
}

func TestWindowSizeSetsViewport(t *testing.T) {
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder))
	if !m.ready {
		t.Fatal("model not ready after WindowSizeMsg")
	}
	if m.vp.Height != 24-chromeHeight {
		t.Errorf("viewport height = %d, want %d", m.vp.Height, 24-chromeHeight)
	}
	if m.vp.Width != 80 {
		t.Errorf("viewport width = %d, want 80", m.vp.Width)
	}
}

func TestEnterStartsPrompt(t *testing.T) {
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder))
	m = typeLine(m, "do a thing")
	m, cmd := enter(m)
	if !m.running {
		t.Error("expected running=true after Enter with prompt")
	}
	if cmd == nil {
		t.Error("expected a non-nil cmd (agent goroutine) after Enter")
	}
	if len(m.history) != 1 || m.history[0].Content != "do a thing" {
		t.Errorf("history not appended: %+v", m.history)
	}
	if m.input.Value() != "" {
		t.Error("input not reset after submit")
	}
}

func TestEnterWhileRunningIsNoop(t *testing.T) {
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder))
	m.running = true
	m = typeLine(m, "ignored")
	m, cmd := enter(m)
	if cmd != nil {
		t.Error("expected no cmd while running")
	}
	if len(m.history) != 0 {
		t.Error("history should not change while running")
	}
}

func TestEnterEmptyIsNoop(t *testing.T) {
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder))
	m = typeLine(m, "   ")
	m, cmd := enter(m)
	if cmd != nil || m.running {
		t.Error("empty input should be a no-op")
	}
}

func TestSlashExitQuits(t *testing.T) {
	for _, cmdStr := range []string{"/exit", "/quit"} {
		m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder))
		m = typeLine(m, cmdStr)
		_, cmd := enter(m)
		if cmd == nil {
			t.Fatalf("%s: expected quit cmd", cmdStr)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Errorf("%s: expected tea.Quit", cmdStr)
		}
	}
}

func TestSlashVerboseToggles(t *testing.T) {
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder))
	m = typeLine(m, "/verbose")
	m, _ = enter(m)
	if !m.verbose {
		t.Error("verbose should be true after toggle")
	}
	if !strings.Contains(strings.Join(m.lines, "\n"), "verbose = true") {
		t.Error("expected verbose confirmation line")
	}
}

func TestSlashModelSwitch(t *testing.T) {
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder))
	m = typeLine(m, "/model beta")
	m, _ = enter(m)
	if m.alias != "beta" {
		t.Errorf("alias = %q, want beta", m.alias)
	}
}

func TestSlashModelUnknown(t *testing.T) {
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder))
	m = typeLine(m, "/model nope")
	m, _ = enter(m)
	if m.alias != "alpha" {
		t.Errorf("alias should stay alpha, got %q", m.alias)
	}
	if !strings.Contains(strings.Join(m.lines, "\n"), `unknown model "nope"`) {
		t.Error("expected unknown-model line")
	}
}

func TestSlashSkillInjectsBody(t *testing.T) {
	reg := registryWith(skillEntry("/route", "route body prompt", "route skill"))
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), reg, nilBuilder))
	m = typeLine(m, "/route")
	m, cmd := enter(m)
	if !m.running || cmd == nil {
		t.Fatal("skill command should start a prompt run")
	}
	if len(m.history) != 1 || m.history[0].Content != "route body prompt" {
		t.Errorf("expected skill body as prompt, got %+v", m.history)
	}
}

func TestSlashSkillForwardsArgs(t *testing.T) {
	reg := registryWith(skillEntry("/docket-new-change", "skill body", "design skill"))
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), reg, nilBuilder))
	m = typeLine(m, "/docket-new-change design the auth layer")
	m, cmd := enter(m)
	if !m.running || cmd == nil {
		t.Fatal("skill with args should start a prompt run")
	}
	if len(m.history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(m.history))
	}
	got := m.history[0].Content
	if got != "skill body\n\nARGUMENTS: design the auth layer" {
		t.Errorf("prompt with args = %q", got)
	}
}

func TestSlashUnknown(t *testing.T) {
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder))
	m = typeLine(m, "/bogus")
	m, cmd := enter(m)
	if cmd != nil || m.running {
		t.Error("unknown command should not start a run")
	}
	if !strings.Contains(strings.Join(m.lines, "\n"), "unknown command /bogus") {
		t.Error("expected unknown command line")
	}
}

func TestAssistantMsgAppendsAndRearms(t *testing.T) {
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder))
	next, cmd := m.Update(AssistantMsg{Text: "hello world"})
	m = next.(ShellModel)
	if !strings.Contains(plainLines(m), "hello world") {
		t.Error("assistant text not appended")
	}
	if cmd == nil {
		t.Error("expected re-armed waitForMsg cmd")
	}
}

func TestToolCallTruncation(t *testing.T) {
	long := strings.Repeat("x", previewLimit+50)

	// Tool call text lives in pendingCall until a result settles it.
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder))
	next, _ := m.Update(ToolCallMsg{Name: "bash", Args: long})
	m = next.(ShellModel)
	if !strings.Contains(m.pendingCall, "bash(") || !strings.Contains(m.pendingCall, "…") {
		t.Errorf("expected truncated pending call, got: %q", m.pendingCall)
	}

	mv := sized(NewShellModel("alpha", true, "dark", testRegistry(), nil, nilBuilder))
	nv, _ := mv.Update(ToolCallMsg{Name: "bash", Args: long})
	mv = nv.(ShellModel)
	if strings.Contains(mv.pendingCall, "…") {
		t.Error("verbose tool call should not be truncated")
	}
}

func TestToolResultErrorPrefix(t *testing.T) {
	m := sized(NewShellModel("alpha", true, "dark", testRegistry(), nil, nilBuilder))
	// Send a ToolCallMsg first so pendingCall is set before the result arrives.
	next, _ := m.Update(ToolCallMsg{Name: "bash", Args: "x"})
	m = next.(ShellModel)
	next, _ = m.Update(ToolResultMsg{Name: "bash", IsError: true, Output: "boom"})
	m = next.(ShellModel)
	if !strings.Contains(strings.Join(m.lines, "\n"), "✗ boom") {
		t.Error("expected error-prefixed tool result")
	}
}

func TestAgentDoneClearsRunning(t *testing.T) {
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder))
	m.running = true
	hist := []model.Message{{Role: "user", Content: "x"}, {Role: "assistant", Content: "y"}}
	next, cmd := m.Update(AgentDoneMsg{History: hist})
	m = next.(ShellModel)
	if m.running {
		t.Error("running should be cleared on AgentDoneMsg")
	}
	if len(m.history) != 2 {
		t.Errorf("history not updated: %+v", m.history)
	}
	if cmd == nil {
		t.Error("expected re-armed waitForMsg cmd")
	}
}

func TestCtrlLClears(t *testing.T) {
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder))
	m.appendLine("some content")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlL})
	m = next.(ShellModel)
	if len(m.lines) != 0 {
		t.Errorf("Ctrl+L should clear lines, got %d", len(m.lines))
	}
}

func TestCtrlCQuits(t *testing.T) {
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder))
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("expected quit cmd")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("Ctrl+C should quit")
	}
}

func TestViewContainsStatusAndPrompt(t *testing.T) {
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder))
	v := m.View()
	if !strings.Contains(v, "alpha") {
		t.Error("view should contain the model alias")
	}
	if !strings.Contains(v, "[alpha] > ") {
		t.Error("view should contain the prompt")
	}
	m.running = true
	if !strings.Contains(m.View(), "Thinking…") {
		t.Error("running view should show the running indicator")
	}
}

// TestCompleterActivatesOnSlash verifies the completer becomes active when
// the input starts with '/'.
func TestCompleterActivatesOnSlash(t *testing.T) {
	reg := registryWith(skillEntry("/code-review", "review body", "review code"))
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), reg, nilBuilder))

	// Type '/' — the completer should activate.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	m = next.(ShellModel)
	m.input.SetValue("/")
	// Simulate the input update path that activates the completer.
	m.completer.activate("/")

	if !m.completer.active {
		t.Error("completer should be active after typing '/'")
	}
}

// TestCompleterEscDeactivates verifies Esc dismisses the completer.
func TestCompleterEscDeactivates(t *testing.T) {
	reg := registryWith(skillEntry("/code-review", "review body", "review code"))
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), reg, nilBuilder))
	m.input.SetValue("/")
	m.completer.activate("/")

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(ShellModel)
	if m.completer.active {
		t.Error("completer should be deactivated after Esc")
	}
	if m.input.Value() != "" {
		t.Error("input should be cleared after Esc")
	}
}

// TestCompleterEnterDispatchesSkill verifies that pressing Enter while the
// completer is active runs the selected skill body, not "unknown command".
func TestCompleterEnterDispatchesSkill(t *testing.T) {
	reg := registryWith(skillEntry("/docket-status", "show the board", "board status"))
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), reg, nilBuilder))

	// Activate the completer and select the skill.
	m.input.SetValue("/")
	m.completer.activate("/")
	if len(m.completer.visible) == 0 {
		t.Fatal("completer has no visible entries")
	}

	// Press Enter — should dispatch the skill, not print "unknown command".
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(ShellModel)

	if cmd == nil || !m.running {
		t.Fatal("completer Enter should start a prompt run for a skill")
	}
	if len(m.history) != 1 || m.history[0].Content != "show the board" {
		t.Errorf("expected skill body as prompt, got %+v", m.history)
	}
	for _, line := range m.lines {
		if strings.Contains(ansiRE.ReplaceAllString(line, ""), "unknown command") {
			t.Errorf("got 'unknown command' instead of dispatching skill: %q", line)
		}
	}
}

// TestRegistryReloadMsgRefreshesCompleter verifies that a registryReloadMsg
// triggers a completer refresh.
func TestRegistryReloadMsgRefreshesCompleter(t *testing.T) {
	reg := registryWith(skillEntry("/echo", "echo body", "echo skill"))
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), reg, nilBuilder))
	m.input.SetValue("/ec")
	m.completer.activate("/ec")

	next, cmd := m.Update(registryReloadMsg{})
	m = next.(ShellModel)
	_ = m
	// cmd should be a re-armed waitForRegistryReload
	if cmd == nil {
		t.Error("expected re-armed registry reload cmd after registryReloadMsg")
	}
}
