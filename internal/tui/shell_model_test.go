package tui

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/version"
)

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// flattenLines renders the transcript store back into the flat pre-concatenated
// strings the transcript used to hold — first + text per line — so content
// assertions can inspect the store independent of wrap decisions.
func flattenLines(m ShellModel) []string {
	out := make([]string, len(m.lines))
	for i, l := range m.lines {
		out[i] = l.first + l.text
	}
	return out
}

// plainLines strips ANSI escape codes and joins model lines for content checks.
func plainLines(m ShellModel) string {
	return ansiRE.ReplaceAllString(strings.Join(flattenLines(m), "\n"), "")
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

func (p *staticProvider) Commands() []SlashEntry   { return p.entries }
func (p *staticProvider) Changes() <-chan struct{} { return nil }
func (p *staticProvider) Close()                   {}

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

func TestNewShellModel_ShowsBanner(t *testing.T) {
	m := NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder)
	got := plainLines(m)
	for _, want := range []string{`'########:'##`, version.Version, "alpha"} {
		if !strings.Contains(got, want) {
			t.Errorf("scrollback missing %q\nscrollback:\n%s", want, got)
		}
	}
	// The banner replaces the old two-line welcome — its text must be gone.
	for _, gone := range []string{"Fuse  alpha", "Type a task"} {
		if strings.Contains(got, gone) {
			t.Errorf("scrollback still contains old welcome text %q\nscrollback:\n%s", gone, got)
		}
	}
}

// TestAppendLineStoresTranscriptLines verifies appendLine splits on newlines
// into one zero-prefix transcriptLine per row (pre:false, no first/cont prefix)
// and that the flattened render is behavior-preserving at a wide viewport.
func TestAppendLineStoresTranscriptLines(t *testing.T) {
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder))
	m.lines = nil // drop banner lines so we inspect only what we append
	m.appendLine("a\nb")
	if len(m.lines) != 2 {
		t.Fatalf("appendLine(\"a\\nb\") -> %d lines, want 2", len(m.lines))
	}
	for i, want := range []string{"a", "b"} {
		l := m.lines[i]
		if l.first != "" || l.cont != "" {
			t.Errorf("line %d: want zero prefix, got first=%q cont=%q", i, l.first, l.cont)
		}
		if l.pre {
			t.Errorf("line %d: want pre=false", i)
		}
		if l.text != want {
			t.Errorf("line %d: text = %q, want %q", i, l.text, want)
		}
	}
	// Behavior-preserving at wide viewport: flattened content matches the raw text.
	if got := strings.Join(flattenLines(m), "\n"); got != "a\nb" {
		t.Errorf("flattened = %q, want %q", got, "a\nb")
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
	if !strings.Contains(strings.Join(flattenLines(m), "\n"), "verbose = true") {
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
	if !strings.Contains(strings.Join(flattenLines(m), "\n"), `unknown model "nope"`) {
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
	if !strings.Contains(strings.Join(flattenLines(m), "\n"), "unknown command /bogus") {
		t.Error("expected unknown command line")
	}
}

func TestAssistantMsgAppends(t *testing.T) {
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder))
	next, cmd := m.Update(AssistantMsg{Text: "hello world"})
	m = next.(ShellModel)
	if !strings.Contains(plainLines(m), "hello world") {
		t.Error("assistant text not appended")
	}
	// Program.Send delivery: channel messages need no re-armed subscription cmd.
	if cmd != nil {
		t.Error("channel messages should not return a subscription cmd")
	}
}

func TestToolCallTruncation(t *testing.T) {
	long := strings.Repeat("x", previewLimit+50)

	// Tool call text queues until its result settles it.
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder))
	next, _ := m.Update(ToolCallMsg{Name: "bash", Args: long})
	m = next.(ShellModel)
	if len(m.pendingCalls) != 1 {
		t.Fatalf("pendingCalls len = %d, want 1", len(m.pendingCalls))
	}
	if !strings.Contains(m.pendingCalls[0].text, "bash(") || !strings.Contains(m.pendingCalls[0].text, "…") {
		t.Errorf("expected truncated pending call, got: %q", m.pendingCalls[0].text)
	}

	mv := sized(NewShellModel("alpha", true, "dark", testRegistry(), nil, nilBuilder))
	nv, _ := mv.Update(ToolCallMsg{Name: "bash", Args: long})
	mv = nv.(ShellModel)
	if strings.Contains(mv.pendingCalls[0].text, "…") {
		t.Error("verbose tool call should not be truncated")
	}
}

// TestBatchedCallsPairWithResults verifies a batch of announced calls renders
// as call+result pairs, not one bullet followed by unbroken result soup.
func TestBatchedCallsPairWithResults(t *testing.T) {
	m := sized(NewShellModel("alpha", true, "dark", testRegistry(), nil, nilBuilder))
	for _, p := range []string{"a.go", "b.go", "c.go"} {
		next, _ := m.Update(ToolCallMsg{Name: "read_file", Args: p})
		m = next.(ShellModel)
	}
	if len(m.pendingCalls) != 3 {
		t.Fatalf("pendingCalls len = %d, want 3", len(m.pendingCalls))
	}
	for _, out := range []string{"content-a", "content-b", "content-c"} {
		next, _ := m.Update(ToolResultMsg{Name: "read_file", Output: out})
		m = next.(ShellModel)
	}
	if len(m.pendingCalls) != 0 {
		t.Fatalf("pendingCalls not drained: %d", len(m.pendingCalls))
	}
	joined := plainLines(m)
	posCallB := strings.Index(joined, "b.go")
	posResA := strings.Index(joined, "content-a")
	posResB := strings.Index(joined, "content-b")
	if posResA == -1 || posResB == -1 || posCallB == -1 {
		t.Fatalf("missing content in transcript:\n%s", joined)
	}
	// Each result must appear after its own call's bullet: a-result, then
	// b-call, then b-result.
	if !(posResA < posCallB && posCallB < posResB) {
		t.Errorf("expected call/result interleaving; got:\n%s", joined)
	}
	if got := strings.Count(joined, "read_file("); got != 3 {
		t.Errorf("expected 3 call bullets, got %d", got)
	}
}

func TestToolResultErrorPrefix(t *testing.T) {
	m := sized(NewShellModel("alpha", true, "dark", testRegistry(), nil, nilBuilder))
	// Send a ToolCallMsg first so pendingCall is set before the result arrives.
	next, _ := m.Update(ToolCallMsg{Name: "bash", Args: "x"})
	m = next.(ShellModel)
	next, _ = m.Update(ToolResultMsg{Name: "bash", IsError: true, Output: "boom"})
	m = next.(ShellModel)
	if !strings.Contains(strings.Join(flattenLines(m), "\n"), "✗ boom") {
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
	if cmd != nil {
		t.Error("channel messages should not return a subscription cmd")
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
	for _, line := range flattenLines(m) {
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
	// Reloads are pumped by StartBridges; no subscription cmd expected.
	if cmd != nil {
		t.Error("registryReloadMsg should not return a subscription cmd")
	}
}

// TestUserPromptEchoedInTranscript: the submitted prompt must remain visible
// after the input clears.
func TestUserPromptEchoedInTranscript(t *testing.T) {
	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder))
	m.input.SetValue("please audit the spawn path")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(ShellModel)
	if !strings.Contains(plainLines(m), "please audit the spawn path") {
		t.Error("user prompt not echoed into transcript")
	}
}

// TestPreviewResultKeepsLinesAndNotesElision: non-verbose results show real
// leading lines plus a pointer to the full output, not a 120-char chop.
func TestPreviewResultKeepsLinesAndNotesElision(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&sb, "result line %02d\n", i)
	}
	got := previewResult(sb.String())
	if !strings.Contains(got, "result line 00") || !strings.Contains(got, "result line 07") {
		t.Errorf("first lines missing: %q", got)
	}
	if strings.Contains(got, "result line 08") {
		t.Errorf("preview too long: %q", got)
	}
	if !strings.Contains(got, "+22 more lines") || !strings.Contains(got, "/verbose") {
		t.Errorf("elision note missing/wrong: %q", got)
	}
	short := "one\ntwo"
	if previewResult(short) != short {
		t.Error("short results must pass through unchanged")
	}
}

// TestStatusBarShowsAgentCounter: running/queued children must be visible in
// the shell status bar before the user opens the agents overlay.
func TestStatusBarShowsAgentCounter(t *testing.T) {
	tree := agent.NewAgentTree("root", "alpha")
	root := tree.Node(tree.RootID())
	block := make(chan struct{})
	s := agent.NewSpawner(agent.WithTree(tree), agent.WithNode(root),
		agent.WithChildBuilder(func(ctx context.Context, _ agent.SpawnOpts, _ *agent.AgentNode, _ *agent.AgentTree) (string, error) {
			select {
			case <-block:
			case <-ctx.Done():
			}
			return "", nil
		}))
	handles := make([]agent.AgentHandle, 0, agent.MaxConcurrentSpawns+2)
	for i := 0; i < agent.MaxConcurrentSpawns+2; i++ {
		h, err := s.Spawn(context.Background(), agent.SpawnOpts{Label: "w"})
		if err != nil {
			t.Fatal(err)
		}
		handles = append(handles, h)
	}
	// Wait until the cap's worth of children report running.
	deadline := time.After(2 * time.Second)
	for {
		if r, _ := tree.ActiveCounts(); r >= agent.MaxConcurrentSpawns {
			break
		}
		select {
		case <-deadline:
			t.Fatal("children never started")
		case <-time.After(5 * time.Millisecond):
		}
	}

	m := sized(NewShellModel("alpha", false, "dark", testRegistry(), nil, nilBuilder))
	m = m.WithTree(tree)
	view := ansiRE.ReplaceAllString(m.View(), "")
	if !strings.Contains(view, fmt.Sprintf("%d running", agent.MaxConcurrentSpawns)) {
		t.Errorf("status bar missing running counter: %q", view)
	}
	if !strings.Contains(view, "2 queued") {
		t.Errorf("status bar missing queued counter: %q", view)
	}

	close(block)
	for _, h := range handles {
		h.Wait()
	}
	view = ansiRE.ReplaceAllString(m.View(), "")
	if !strings.Contains(view, "Tab → agents") || strings.Contains(view, "running") {
		t.Errorf("idle status bar should fall back to plain hint: %q", view)
	}
}
