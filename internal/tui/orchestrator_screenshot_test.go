package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/permissions"
)

// Visual scenes for the orchestrator and /interrupt UX, captured through the same
// harness as the other screenshot tests (FUSE_SCREENSHOT_DIR collects .ansi/.txt/.png).

func orchScene(t *testing.T, width, height int, style string) (ShellModel, *fakeOrch, *agent.HumanBus, *agent.AgentTree) {
	t.Helper()
	// Transcript lines are styled when they are appended, not when the frame is drawn, so
	// the colour profile must be on for the whole scene or the lines come out monochrome.
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
	tree := agent.NewAgentTree("alpha", "test")
	reg := agent.NewHandleRegistry()
	reg.Register(tree.RootID(), "alpha")
	bus := agent.NewHumanBus(tree)
	slashReg := NewSlashRegistry(NewBuiltinProvider())
	o := &fakeOrch{}
	m := NewShellModel("minimax", false, style, testRegistry(), slashReg, nilBuilder, permissions.NewSessionMode(permissions.ModeAuto), true)
	next, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	m = next.(ShellModel)
	m = m.WithTree(tree).WithBlackboard(agent.NewBlackboard(tree)).WithHumanMessaging(bus, reg, nil).WithOrchestrator(o)
	return m, o, bus, tree
}

func update(m ShellModel, msg tea.Msg) ShellModel {
	next, _ := m.Update(msg)
	return next.(ShellModel)
}

// busyConversation drives a realistic mid-turn conversation: the agent is running a
// long command, the person talks to the orchestrator, which looks, steers, and reports.
func busyConversation(t *testing.T, m ShellModel) ShellModel {
	t.Helper()
	m.lines = nil
	m = typeAndEnter(m, "Run the migration, then run the full test suite and report failures.")
	m = update(m, AssistantMsg{Text: "I'll start with the migration."})
	m = update(m, ToolCallMsg{Name: "bash", Args: `{"command":"go run ./cmd/migrate --apply && sleep 40"}`})
	m = typeAndEnter(m, "/orchestrator what is the agent doing right now?")
	m = update(m, OrchReplyMsg{Text: "It is **running the migration** (`go run ./cmd/migrate --apply`) and is waiting on a 40 second sleep.\n\nNothing has failed so far."})
	m = typeAndEnter(m, "/orchestrator tell it to skip the sleep and run the tests with -race. interrupt it.")
	m = update(m, OrchActionMsg{Text: "interrupting the agent: Skip the sleep. Run `go test -race ./...` now and report failures."})
	m = update(m, OrchReplyMsg{Text: "Done. I interrupted the agent and told it to skip the sleep and run the tests with `-race`."})
	m = typeAndEnter(m, "/interrupt actually, also run go vet first")
	m = update(m, OrchErrMsg{Err: "gateway timeout (the orchestrator will retry on your next message)"})
	return m
}

// typeOnly types a line rune by rune through the real key handler, without Enter.
func typeOnly(m ShellModel, line string) ShellModel {
	for _, r := range line {
		m = update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return m
}

func TestScreenshot_OrchSlashMenu(t *testing.T) {
	m, _, _, _ := orchScene(t, 100, 30, "dark")
	m = typeOnly(m, "/o")
	f := captureModelFrame(t, m, "orch-01-menu-o")
	if !strings.Contains(f, "/orchestrator") {
		t.Errorf("menu for /o missing /orchestrator:\n%s", f)
	}

	// The full list pages; the top shows a "more" hint, and the arrow keys scroll it.
	m2, _, _, _ := orchScene(t, 100, 30, "dark")
	m2 = typeOnly(m2, "/")
	f = captureModelFrame(t, m2, "orch-02-menu-all-top")
	if !strings.Contains(f, "more") {
		t.Errorf("full menu should hint that more entries follow:\n%s", f)
	}
	for i := 0; i < 13; i++ {
		m2 = update(m2, tea.KeyMsg{Type: tea.KeyDown})
	}
	f = captureModelFrame(t, m2, "orch-02c-menu-all-scrolled")
	for _, want := range []string{"/orchestrator", "/interrupt"} {
		if !strings.Contains(f, want) {
			t.Errorf("scrolled menu missing %s:\n%s", want, f)
		}
	}

	m3, _, _, _ := orchScene(t, 100, 30, "dark")
	m3 = typeOnly(m3, "/i")
	f = captureModelFrame(t, m3, "orch-02b-menu-i")
	if !strings.Contains(f, "/interrupt") {
		t.Errorf("menu for /i missing /interrupt:\n%s", f)
	}
}

func TestScreenshot_OrchModeOnAndOff(t *testing.T) {
	m, _, _, _ := orchScene(t, 100, 24, "dark")
	m = typeAndEnter(m, "/orchestrator")
	f := captureModelFrame(t, m, "orch-03-mode-on")
	for _, want := range []string{"orchestrator mode on", "[orchestrator] >", "ask the orchestrator"} {
		if !strings.Contains(f, want) {
			t.Errorf("mode-on frame missing %q:\n%s", want, f)
		}
	}
	m = typeAndEnter(m, "/orchestrator off")
	f = captureModelFrame(t, m, "orch-04-mode-off")
	if !strings.Contains(f, "[minimax] >") || !strings.Contains(f, "type a task") {
		t.Errorf("mode-off frame did not restore the prompt:\n%s", f)
	}
}

func TestScreenshot_OrchConversation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		w, h  int
		style string
	}{
		{"orch-05-conversation-dark-100", 100, 36, "dark"},
		{"orch-06-conversation-dark-60", 60, 40, "dark"},
		{"orch-07-conversation-light-100", 100, 36, "light"},
		{"orch-08-conversation-dark-140", 140, 36, "dark"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, o, bus, tree := orchScene(t, tc.w, tc.h, tc.style)
			m.running = true
			m.runStart = time.Now().Add(-16 * time.Second)
			m = busyConversation(t, m)
			f := captureModelFrame(t, m, tc.name)
			for _, want := range []string{"you → orchestrator", "interrupting the agent", "⚡ interrupt", "gateway timeout"} {
				if !strings.Contains(f, want) {
					t.Errorf("%s: frame missing %q", tc.name, want)
				}
			}
			// Each orchestrator exchange and the interrupt echo start after a blank line, so
			// they never run into the agent's output above.
			lines := strings.Split(f, "\n")
			for i, l := range lines {
				if (strings.HasPrefix(strings.TrimSpace(l), "you → orchestrator") || strings.HasPrefix(strings.TrimSpace(l), "⚡ interrupt")) && i > 0 && strings.TrimSpace(lines[i-1]) != "" {
					t.Errorf("%s: no blank line before %q (after %q)", tc.name, strings.TrimSpace(l), strings.TrimSpace(lines[i-1]))
				}
			}
			if got := o.got(); len(got) != 2 {
				t.Errorf("orchestrator heard %d messages, want 2: %v", len(got), got)
			}
			// The scene's opening task queued (the model is marked running) and the
			// /interrupt line queued behind it with the interrupt flag.
			p := bus.Pending(tree.RootID())
			if len(p) != 2 || p[0].Interrupt || !p[1].Interrupt || p[1].Text != "actually, also run go vet first" {
				t.Errorf("unexpected agent queue: %+v", p)
			}
		})
	}
}

func TestScreenshot_OrchStickyModeBusy(t *testing.T) {
	m, o, _, _ := orchScene(t, 100, 30, "dark")
	m.running = true
	m.runStart = time.Now().Add(-9 * time.Second)
	m.lines = nil
	m = typeAndEnter(m, "Refactor the auth package.")
	m = update(m, ToolCallMsg{Name: "bash", Args: `{"command":"go test ./internal/auth/..."}`})
	m = typeAndEnter(m, "/orchestrator")
	m = typeAndEnter(m, "is it stuck? if so nudge it")
	m = update(m, OrchReplyMsg{Text: "Not stuck. It started `go test ./internal/auth/...` a few seconds ago."})
	f := captureModelFrame(t, m, "orch-09-sticky-while-busy")
	if !strings.Contains(f, "[orchestrator] >") || len(o.got()) != 1 {
		t.Errorf("sticky busy frame wrong (heard %v):\n%s", o.got(), f)
	}
}

func TestScreenshot_OrchUsageAndUnavailable(t *testing.T) {
	m, _, _, _ := orchScene(t, 100, 20, "dark")
	m.lines = nil
	m = typeAndEnter(m, "/interrupt")
	f := captureModelFrame(t, m, "orch-10-interrupt-usage")
	if !strings.Contains(f, "usage: /interrupt") {
		t.Errorf("usage missing:\n%s", f)
	}

	m2, _, _, _ := orchScene(t, 100, 20, "dark")
	m2.orch = nil
	m2.lines = nil
	m2 = typeAndEnter(m2, "/orchestrator")
	f = captureModelFrame(t, m2, "orch-11-unavailable")
	if !strings.Contains(f, "not available") {
		t.Errorf("unavailable note missing:\n%s", f)
	}
}

func TestScreenshot_OrchWithApprovalPending(t *testing.T) {
	m, o, _, _ := orchScene(t, 100, 30, "dark")
	m.running = true
	m.lines = nil
	m = typeAndEnter(m, "/orchestrator")
	m = update(m, OrchReplyMsg{Text: "Watching the agent."})
	// A permission prompt arrives while the orchestrator is on.
	resp := make(chan approvalResponse, 1)
	m = update(m, PermissionRequestMsg{Request: permissions.ApprovalRequest{ToolName: "bash", Args: `{"command":"rm -rf build/"}`, Preview: "rm -rf build/"}, RespCh: resp})
	f := captureModelFrame(t, m, "orch-12-approval-pending")
	if !strings.Contains(f, "press y") {
		t.Errorf("approval prompt missing:\n%s", f)
	}
	// Typing while an approval is pending must not leak to the orchestrator: the key
	// belongs to the approval prompt.
	m = update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if len(o.got()) != 0 {
		t.Errorf("approval keystroke reached the orchestrator: %v", o.got())
	}
}
