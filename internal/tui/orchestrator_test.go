package tui

import (
	"strings"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
)

type fakeOrch struct {
	mu   sync.Mutex
	said []string
}

func (f *fakeOrch) Say(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.said = append(f.said, text)
}

func (f *fakeOrch) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.said...)
}

func orchShell(t *testing.T) (ShellModel, *fakeOrch, *agent.HumanBus, *agent.AgentTree) {
	t.Helper()
	m, bus, tree := shellWithHumanMessaging(t)
	o := &fakeOrch{}
	return m.WithOrchestrator(o), o, bus, tree
}

func TestOrchestratorUnavailableWithoutSession(t *testing.T) {
	m, _, _ := shellWithHumanMessaging(t)
	m.lines = nil
	m = typeAndEnter(m, "/orchestrator")
	if !strings.Contains(plainLines(m), "not available") {
		t.Fatalf("want an 'not available' note:\n%s", plainLines(m))
	}
}

// One-shot form works while the agent is busy and does not change modes.
func TestOrchestratorOneMessageMidTurn(t *testing.T) {
	m, o, bus, tree := orchShell(t)
	m = typeAndEnter(m, "/orchestrator how is it going?")
	if got := o.got(); len(got) != 1 || got[0] != "how is it going?" {
		t.Fatalf("orchestrator heard %v", got)
	}
	if m.orchMode {
		t.Fatal("a one-shot message must not turn on orchestrator mode")
	}
	if n := len(bus.Pending(tree.RootID())); n != 0 {
		t.Fatalf("the message leaked to the working agent's queue (%d)", n)
	}
	if !strings.Contains(plainLines(m), "how is it going?") {
		t.Fatalf("the message is not echoed:\n%s", plainLines(m))
	}
}

// Sticky mode: plain lines go to the orchestrator, even mid-turn, until turned off.
func TestOrchestratorModeRoutesPlainLines(t *testing.T) {
	m, o, bus, tree := orchShell(t)
	m = typeAndEnter(m, "/orchestrator")
	if !m.orchMode {
		t.Fatal("bare /orchestrator should turn the mode on")
	}
	if !strings.Contains(m.View(), "orchestrator") {
		t.Fatal("the prompt must show the orchestrator is the one listening")
	}
	if !strings.Contains(m.input.Placeholder, "orchestrator") {
		t.Fatalf("placeholder does not say who is listening: %q", m.input.Placeholder)
	}
	m = typeAndEnter(m, "tell it to run the tests first")
	if got := o.got(); len(got) != 1 || got[0] != "tell it to run the tests first" {
		t.Fatalf("orchestrator heard %v", got)
	}
	if n := len(bus.Pending(tree.RootID())); n != 0 {
		t.Fatalf("mode leaked a line to the working agent (%d)", n)
	}

	m = typeAndEnter(m, "/orchestrator off")
	if m.orchMode {
		t.Fatal("/orchestrator off should turn the mode off")
	}
	if m.input.Placeholder != defaultPlaceholder {
		t.Fatalf("placeholder not restored: %q", m.input.Placeholder)
	}
	m = typeAndEnter(m, "plain note for the agent")
	if n := len(bus.Pending(tree.RootID())); n != 1 {
		t.Fatalf("after off, a busy line should queue for the agent, got %d", n)
	}
	if got := o.got(); len(got) != 1 {
		t.Fatalf("orchestrator heard a line after off: %v", got)
	}
}

func TestInterruptCommandSteersRunningAgent(t *testing.T) {
	m, _, bus, tree := orchShell(t)
	fired := 0
	bus.SetInterrupt(tree.RootID(), func() { fired++ })
	m = typeAndEnter(m, "/interrupt stop and use plan B")
	p := bus.Pending(tree.RootID())
	if len(p) != 1 || !p[0].Interrupt || p[0].Text != "stop and use plan B" || p[0].Source != agent.SourceHuman {
		t.Fatalf("pending = %+v", p)
	}
	if fired != 1 {
		t.Fatalf("interrupt hook fired %d times, want 1", fired)
	}
	if !strings.Contains(plainLines(m), "stop and use plan B") {
		t.Fatalf("not echoed:\n%s", plainLines(m))
	}
}

func TestOrchestratorOutputRenders(t *testing.T) {
	m, _, _, _ := orchShell(t)
	m.lines = nil
	next, _ := m.Update(OrchReplyMsg{Text: "it is running the linter"})
	next, _ = next.(ShellModel).Update(OrchActionMsg{Text: "told the agent to run the tests"})
	next, _ = next.(ShellModel).Update(OrchErrMsg{Err: "gateway timeout"})
	out := plainLines(next.(ShellModel))
	for _, want := range []string{"orchestrator", "it is running the linter", "told the agent to run the tests", "gateway timeout"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestOrchestratorCommandsAreInTheSlashMenu(t *testing.T) {
	var names []string
	for _, e := range NewBuiltinProvider().Commands() {
		names = append(names, e.Command)
	}
	joined := strings.Join(names, " ")
	for _, want := range []string{"/orchestrator", "/interrupt"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%s missing from the slash menu: %s", want, joined)
		}
	}
}

// Picking /orchestrator from the slash menu reaches handleSlash, not the typed-line path.
func TestOrchestratorFromTheSlashMenu(t *testing.T) {
	m, _, _, _ := orchShell(t)
	m.running = false
	next, _ := m.handleSlash("/orchestrator")
	if !next.(ShellModel).orchMode {
		t.Fatal("the slash-menu route did not turn orchestrator mode on")
	}
	next, _ = next.(ShellModel).handleSlash("/orchestrator off")
	if next.(ShellModel).orchMode {
		t.Fatal("/orchestrator off via handleSlash did not turn the mode off")
	}
}
