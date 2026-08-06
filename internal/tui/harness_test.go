package tui

// harness_test.go provides a reusable, end-to-end TUI harness that drives a
// real ShellModel through an actual bubbletea program (via teatest), including
// the channel bridge that carries agent-renderer output back into the Update
// loop. It closes the gap the unit tests leave open: those poke Update/View
// directly and short-circuit the agent by feeding AgentDoneMsg by hand, so they
// never exercise the live path a keystroke actually travels —
//
//	keystrokes → completer → handleSlash/handleSlashEntry → startPrompt →
//	AgentBuilder → agent.Run → TeaRenderer → m.ch → StartBridges → Update → View
//
// The harness wires that whole path with a scripted fake agent, so a test can
// type "/research foo", press Enter, and assert on what the terminal shows —
// which is exactly what one-shot `fuse "<task>"` mode cannot reach, because it
// registers no skill tool and never starts the TUI.

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/tools"
)

// scriptedCompleter is a fake agent.Completer that returns a fixed sequence of
// responses, one per Complete call. It records every request it receives so a
// test can assert on what the agent was actually asked (e.g. that the injected
// skill body plus "ARGUMENTS:" reached the model). The final response must not
// carry tool calls, so the agent loop terminates.
type scriptedCompleter struct {
	responses []model.CompletionResp
	calls     int
	requests  []model.CompletionReq
}

func (c *scriptedCompleter) Complete(_ context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	c.requests = append(c.requests, req)
	i := c.calls
	c.calls++
	if i >= len(c.responses) {
		// Safety net: an unexpected extra turn ends the loop cleanly rather
		// than panicking, so a scripting mistake surfaces as a failed content
		// assertion instead of a crash inside the program goroutine.
		return model.CompletionResp{Content: ""}, nil
	}
	return c.responses[i], nil
}

// assistantOnce builds a scriptedCompleter that answers with a single plain
// assistant message and then stops — the common "the model just replied" case.
func assistantOnce(text string) *scriptedCompleter {
	return &scriptedCompleter{responses: []model.CompletionResp{{Content: text}}}
}

// harness drives a real ShellModel end-to-end through a teatest program with the
// agent-event bridge running, mirroring cmd/fuse/shell.go's wiring.
type harness struct {
	t   *testing.T
	tm  *teatest.TestModel
	m   ShellModel
	cmp *scriptedCompleter

	cancelBridge context.CancelFunc
}

// harnessOpts configures a harness. A nil SlashReg means no slash commands (all
// dispatch to "unknown"); Completer defaults to a single empty reply.
type harnessOpts struct {
	SlashReg  *SlashRegistry
	Completer *scriptedCompleter
	Alias     string
	Width     int
	Height    int
}

// newHarness constructs the model, starts the bridge that pumps m.Channel()
// into the program, and boots teatest. The bridge is the load-bearing detail:
// without it the fake agent's TeaRenderer output would sit unread on m.ch and
// never reach Update, so the transcript would never show the reply — the exact
// failure mode a hand-rolled test silently hides by sending AgentDoneMsg itself.
func newHarness(t *testing.T, opts harnessOpts) *harness {
	t.Helper()

	if opts.Alias == "" {
		opts.Alias = "alpha"
	}
	if opts.Width == 0 {
		opts.Width = 80
	}
	if opts.Height == 0 {
		opts.Height = 24
	}
	cmp := opts.Completer
	if cmp == nil {
		cmp = assistantOnce("")
	}

	// The AgentBuilder is the injection seam: it hands the model a real
	// agent.Agent backed by our scripted completer and a no-op tool executor,
	// rendering through the TeaRenderer the model wires onto its channel.
	build := func(alias string, r agent.Renderer, _ permissions.ApprovalFunc) (*agent.Agent, error) {
		return agent.New(cmp, noopToolExec{}, r, "test/model", "", 25, 0), nil
	}

	m := NewShellModel(opts.Alias, false, "", testRegistry(), opts.SlashReg, build, permissions.NewSessionMode(permissions.ModeSmart), true)

	bridgeCtx, cancel := context.WithCancel(context.Background())

	tm := teatest.NewTestModel(t, m,
		teatest.WithInitialTermSize(opts.Width, opts.Height),
	)

	// Pump agent-renderer events (AssistantMsg, AgentDoneMsg, …) from the
	// model's channel into the program, exactly as StartBridges does in the
	// real shell. tree/slashReg are nil here: this harness targets the
	// single-agent transcript path, not the multi-agent drilldown view.
	StartBridges(bridgeCtx, tm.GetProgram(), m.Channel(), nil, nil)

	h := &harness{t: t, tm: tm, m: m, cmp: cmp, cancelBridge: cancel}
	t.Cleanup(h.close)
	return h
}

// close tears down the bridge and program. Quitting the program first stops
// Update from consuming further sends; cancelling the bridge stops the pump.
func (h *harness) close() {
	h.tm.Quit() //nolint:errcheck // best-effort teardown
	h.cancelBridge()
}

// typeAndSubmit sends each rune of s as a key press, then Enter — the same
// events a real terminal delivers, so the completer and key handling run for
// real rather than being bypassed by SetValue.
func (h *harness) typeAndSubmit(s string) {
	h.t.Helper()
	for _, r := range s {
		h.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	h.tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
}

// waitForOutput blocks until want appears in the rendered terminal output or the
// timeout elapses, failing the test on timeout. It reads the program's live
// output stream, so it observes exactly what a user's terminal would show.
func (h *harness) waitForOutput(want string, timeout time.Duration) {
	h.t.Helper()
	teatest.WaitFor(h.t, h.tm.Output(), func(b []byte) bool {
		return bytes.Contains(stripANSI(b), []byte(want))
	}, teatest.WithDuration(timeout), teatest.WithCheckInterval(10*time.Millisecond))
}

// finalOutput quits the program and returns the last rendered frame, ANSI
// stripped, for whole-frame assertions after a turn has settled.
func (h *harness) finalOutput() string {
	h.t.Helper()
	h.tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
	out, err := io.ReadAll(h.tm.FinalOutput(h.t, teatest.WithFinalTimeout(3*time.Second)))
	if err != nil {
		h.t.Fatalf("read final output: %v", err)
	}
	return string(stripANSI(out))
}

// stripANSI removes ANSI escape sequences so content assertions are stable
// across terminal styling. It reuses the package's ansiRE (defined in
// shell_model_test.go).
func stripANSI(b []byte) []byte {
	return ansiRE.ReplaceAll(b, nil)
}

// noopToolExec is a ToolExecutor that never runs a tool — the scripted
// completer under test returns no tool calls, so it is never invoked, but the
// agent constructor requires a non-nil executor.
type noopToolExec struct{}

func (noopToolExec) Execute(_ context.Context, _ string, _ string) tools.Result {
	return tools.Result{Output: ""}
}

func (noopToolExec) Schemas() []model.ToolSchema { return nil }

// researchSkillEntry returns a SlashEntry that stands in for the embedded
// /research skill: a KindSkill whose expand() yields the skill body the
// KindSkill dispatch path injects, verbatim, ahead of "ARGUMENTS: <args>".
func researchSkillEntry(body string) SlashEntry {
	return SlashEntry{
		Command:     "/research",
		Description: "Web research fan-out.",
		Kind:        KindSkill,
		expand:      func() string { return body },
	}
}

// --- Tests ---------------------------------------------------------------

// TestHarness_PlainPromptRoundTrip is the baseline: a non-slash prompt runs the
// agent and the reply appears in the transcript. Proves the harness wiring
// (keystrokes → build → agent.Run → renderer → bridge → View) is sound before
// the slash-specific tests lean on it.
func TestHarness_PlainPromptRoundTrip(t *testing.T) {
	h := newHarness(t, harnessOpts{
		Completer: assistantOnce("the-model-replied-42"),
	})
	h.typeAndSubmit("hello there")
	h.waitForOutput("the-model-replied-42", 5*time.Second)
}

// TestHarness_ResearchSlashDispatch is the coverage the e2e gap demanded: the
// /research command, driven as real keystrokes through the live program, must
// dispatch through the KindSkill path — injecting the skill body plus the typed
// ARGUMENTS into the agent — and surface the model's reply in the transcript.
// This is the flow one-shot `fuse "<task>"` mode structurally cannot reach.
func TestHarness_ResearchSlashDispatch(t *testing.T) {
	const skillBody = "RESEARCH-SKILL-BODY: diversify into facets and fan out."
	cmp := assistantOnce("research-report-done")
	h := newHarness(t, harnessOpts{
		SlashReg:  registryWith(researchSkillEntry(skillBody)),
		Completer: cmp,
	})

	h.typeAndSubmit("/research is SQLite good in 2026")
	h.waitForOutput("research-report-done", 5*time.Second)

	// The agent must have been asked with the skill body AND the typed
	// arguments — the whole point of KindSkill dispatch. Assert against the
	// recorded request rather than the rendered frame, which only shows the
	// echoed command line, not the injected body.
	if len(cmp.requests) == 0 {
		t.Fatal("agent Complete was never called — slash dispatch did not run the agent")
	}
	sent := renderRequest(cmp.requests[0])
	if !strings.Contains(sent, skillBody) {
		t.Errorf("skill body not injected into agent request; got:\n%s", sent)
	}
	if !strings.Contains(sent, "is SQLite good in 2026") {
		t.Errorf("typed ARGUMENTS not forwarded to agent; got:\n%s", sent)
	}
	if !strings.Contains(sent, "ARGUMENTS:") {
		t.Errorf("ARGUMENTS marker missing from agent request; got:\n%s", sent)
	}
}

// TestHarness_UnknownSlashReportsError proves an unrecognized slash command is
// reported in the transcript and does NOT start the agent — the negative path
// that keeps a typo from silently launching a turn.
func TestHarness_UnknownSlashReportsError(t *testing.T) {
	cmp := assistantOnce("should-not-appear")
	h := newHarness(t, harnessOpts{
		SlashReg:  registryWith(researchSkillEntry("body")),
		Completer: cmp,
	})

	h.typeAndSubmit("/definitely-not-a-command")
	h.waitForOutput("unknown", 5*time.Second)

	if cmp.calls != 0 {
		t.Errorf("agent ran for an unknown slash command (calls=%d)", cmp.calls)
	}
}

// renderRequest flattens a CompletionReq's messages into one searchable string.
func renderRequest(req model.CompletionReq) string {
	var b strings.Builder
	for _, msg := range req.Messages {
		b.WriteString(msg.Role)
		b.WriteString(": ")
		b.WriteString(msg.Content)
		b.WriteByte('\n')
	}
	return b.String()
}
