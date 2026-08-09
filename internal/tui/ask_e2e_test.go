package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/tools"
)

// registryToolExec adapts a tools.Registry to the agent's ToolExecutor seam so
// the harness can run a REAL ask_user tool (wired to the model channel) instead
// of the noop executor. This is what closes the loop: the agent goroutine calls
// ask_user, which blocks on the TUI, the user answers, and the answer returns as
// the tool result the agent threads back into context.
type registryToolExec struct{ reg *tools.Registry }

func (e registryToolExec) Execute(ctx context.Context, name, args string) tools.Result {
	return e.reg.Execute(ctx, name, args)
}

func (e registryToolExec) Schemas() []model.ToolSchema { return e.reg.Schemas() }

// TestAsk_E2E_ToolRoundTrip drives the whole path a real question travels:
// scripted model → ask_user tool call → NewTeaAskFunc blocks on the channel →
// overlay renders → user picks an option → answer flows back as the tool result
// → the model's follow-up turn confirms the choice landed in context.
func TestAsk_E2E_ToolRoundTrip(t *testing.T) {
	// The model's first turn calls ask_user; its second turn (after seeing the
	// tool result) replies with prose echoing the recorded answer.
	askArgs := `{"header":"DB driver","question":"Which database driver should we use?",` +
		`"options":[{"label":"pgx (recommended)","description":"modern fast Postgres driver"},` +
		`{"label":"lib/pq","description":"older, widely used"}]}`
	cmp := &scriptedCompleter{responses: []model.CompletionResp{
		{ToolCalls: []model.ToolCall{{ID: "c1", Name: "ask_user", Arguments: askArgs}}},
		{Content: "Recorded: DECISION-WAS-pgx"},
	}}

	var h *harness
	build := func(alias string, r agent.Renderer, _ permissions.ApprovalFunc) (*agent.Agent, error) {
		reg := tools.NewRegistry()
		reg.Register(tools.NewAskUserTool(NewTeaAskFunc(h.m.Channel())))
		return agent.New(cmp, registryToolExec{reg: reg}, r, "test/model", "", 25, 0), nil
	}

	m := NewShellModel("alpha", false, "", testRegistry(), nil, build, permissions.NewSessionMode(permissions.ModeSmart), true)
	h = startHarnessWithModel(t, m)

	h.typeAndSubmit("set up the database")

	// The overlay should appear once the agent calls ask_user. (The per-option
	// labels carry cursor/color styling that can split across ANSI boundaries in
	// the raw output stream, so we assert on the plain question text here and on
	// the settled View() frame below.)
	h.waitForOutput("Which database driver", 5*time.Second)

	// Answer with the first (recommended) option.
	h.tm.Send(tea.KeyMsg{Type: tea.KeyEnter})

	// The follow-up model turn proves the answer reached context: the model only
	// emits this after seeing the ask_user tool result carrying the choice. This
	// is the whole point — the human's selection is now recorded in the
	// conversation the agent reasons over.
	h.waitForOutput("DECISION-WAS-pgx", 5*time.Second)

	// The scripted model's second request must include the tool result carrying
	// the chosen label, confirming the answer was threaded back into context
	// rather than merely rendered.
	if len(cmp.requests) < 2 {
		t.Fatalf("expected a follow-up turn after the answer; got %d requests", len(cmp.requests))
	}
	sent := renderRequest(cmp.requests[1])
	if !strings.Contains(sent, "pgx (recommended)") {
		t.Errorf("the answer was not threaded into the agent's next request:\n%s", sent)
	}
}

// startAskScreenshotHarness boots a harness whose scripted model immediately
// calls ask_user with askArgs, then waits until the question overlay is on
// screen. Shared by the screenshot scenarios below.
func startAskScreenshotHarness(t *testing.T, askArgs, wantOnScreen string) *harness {
	t.Helper()
	cmp := &scriptedCompleter{responses: []model.CompletionResp{
		{ToolCalls: []model.ToolCall{{ID: "c1", Name: "ask_user", Arguments: askArgs}}},
		{Content: "done"},
	}}
	var h *harness
	build := func(alias string, r agent.Renderer, _ permissions.ApprovalFunc) (*agent.Agent, error) {
		reg := tools.NewRegistry()
		reg.Register(tools.NewAskUserTool(NewTeaAskFunc(h.m.Channel())))
		return agent.New(cmp, registryToolExec{reg: reg}, r, "test/model", "", 25, 0), nil
	}
	m := NewShellModel("alpha", false, "", testRegistry(), nil, build, permissions.NewSessionMode(permissions.ModeSmart), true)
	h = startHarnessWithModel(t, m)
	h.typeAndSubmit("go")
	h.waitForOutput(wantOnScreen, 5*time.Second)
	return h
}

// TestAsk_E2E_Screenshot captures the single-select overlay mid-question as a
// visual artifact (reports/screenshots/ask-* when FUSE_SCREENSHOT_DIR is set).
func TestAsk_E2E_Screenshot(t *testing.T) {
	askArgs := `{"header":"Auth","question":"How should users authenticate?",` +
		`"options":[{"label":"OAuth (recommended)","description":"delegate to Google/GitHub"},` +
		`{"label":"Email + password","description":"self-managed credentials"},` +
		`{"label":"Magic links","description":"passwordless email tokens"}]}`
	h := startAskScreenshotHarness(t, askArgs, "How should users authenticate")

	// Move the cursor down one so the screenshot shows a mid-list highlight.
	h.tm.Send(tea.KeyMsg{Type: tea.KeyDown})
	time.Sleep(150 * time.Millisecond)

	frame := captureOverlayFrame(t, h.tm, "ask-question")
	if !strings.Contains(frame, "Magic links") {
		t.Errorf("captured overlay frame missing an option:\n%s", frame)
	}
}

// TestAsk_E2E_ScreenshotMultiSelect captures the multiSelect variant with two
// options toggled ([x] checkboxes).
func TestAsk_E2E_ScreenshotMultiSelect(t *testing.T) {
	askArgs := `{"header":"Features","multiSelect":true,` +
		`"question":"Which features should the new service ship with?",` +
		`"options":[{"label":"Rate limiting","description":"per-client token buckets on every route"},` +
		`{"label":"Structured logging","description":"JSON logs with request IDs"},` +
		`{"label":"Metrics/Prometheus","description":"a /metrics endpoint with RED metrics"},` +
		`{"label":"Tracing","description":"OpenTelemetry spans across handlers"}]}`
	h := startAskScreenshotHarness(t, askArgs, "Which features should the new service")

	// Toggle option 0, move to option 2, toggle it, then park the cursor there so
	// the frame shows two [x] boxes and a highlighted third row.
	h.tm.Send(tea.KeyMsg{Type: tea.KeySpace}) // toggle 0
	h.tm.Send(tea.KeyMsg{Type: tea.KeyDown})
	h.tm.Send(tea.KeyMsg{Type: tea.KeyDown})
	h.tm.Send(tea.KeyMsg{Type: tea.KeySpace}) // toggle 2
	time.Sleep(150 * time.Millisecond)

	frame := captureOverlayFrame(t, h.tm, "ask-multiselect")
	if !strings.Contains(frame, "[x]") {
		t.Errorf("multi-select frame missing a checked box:\n%s", frame)
	}
}

// TestAsk_E2E_ScreenshotFullFlow captures the settled transcript AFTER a full
// multi-select round-trip: the user's prompt, the answered-question record, the
// ask_user tool result carrying both selections, and the model's follow-up that
// acts on them. One frame that tells the whole story a reviewer wants to see.
func TestAsk_E2E_ScreenshotFullFlow(t *testing.T) {
	askArgs := `{"header":"API Features","multiSelect":true,` +
		`"question":"Which API features would you like to enable?",` +
		`"options":[{"label":"Rate limiting (recommended)","description":"throttle requests per client"},` +
		`{"label":"Structured logging","description":"JSON logs with request IDs"},` +
		`{"label":"Prometheus metrics","description":"a /metrics endpoint with RED metrics"},` +
		`{"label":"Tracing","description":"OpenTelemetry spans across handlers"}]}`
	// Turn 1 calls ask_user; turn 2 (after seeing the selections) confirms them.
	cmp := &scriptedCompleter{responses: []model.CompletionResp{
		{ToolCalls: []model.ToolCall{{ID: "c1", Name: "ask_user", Arguments: askArgs}}},
		{Content: "Enabling Rate limiting and Prometheus metrics for your API."},
	}}
	var h *harness
	build := func(alias string, r agent.Renderer, _ permissions.ApprovalFunc) (*agent.Agent, error) {
		reg := tools.NewRegistry()
		reg.Register(tools.NewAskUserTool(NewTeaAskFunc(h.m.Channel())))
		return agent.New(cmp, registryToolExec{reg: reg}, r, "test/model", "", 25, 0), nil
	}
	m := NewShellModel("alpha", false, "", testRegistry(), nil, build, permissions.NewSessionMode(permissions.ModeSmart), true)
	h = startHarnessWithModel(t, m)

	h.typeAndSubmit("set up my API")
	h.waitForOutput("Which API features", 5*time.Second)

	// Toggle option 0 and option 2, then submit both.
	h.tm.Send(tea.KeyMsg{Type: tea.KeySpace}) // Rate limiting
	h.tm.Send(tea.KeyMsg{Type: tea.KeyDown})
	h.tm.Send(tea.KeyMsg{Type: tea.KeyDown})
	h.tm.Send(tea.KeyMsg{Type: tea.KeySpace}) // Prometheus metrics
	h.tm.Send(tea.KeyMsg{Type: tea.KeyEnter})

	// Wait for the tool result to land in the transcript (the selections are
	// rendered unstyled in the result line, so they survive the raw stream),
	// which means the answer reached context and the follow-up turn will render.
	h.waitForOutput(`"selected"`, 5*time.Second)
	time.Sleep(400 * time.Millisecond) // let the follow-up prose settle

	// Quit via tm.Quit() (not a swallowed Ctrl+C) and render the settled final
	// model's transcript — the whole flow as one scrollback frame.
	frame := captureOverlayFrame(t, h.tm, "ask-fullflow")
	for _, want := range []string{"set up my API", "Rate limiting", "Prometheus metrics", "Enabling Rate limiting"} {
		if !strings.Contains(frame, want) {
			t.Errorf("full-flow frame missing %q:\n%s", want, frame)
		}
	}
}

// TestAsk_E2E_ScreenshotFreeText captures the "Type something." field open with
// a partially-typed custom answer.
func TestAsk_E2E_ScreenshotFreeText(t *testing.T) {
	askArgs := `{"header":"DB driver","question":"Which database driver should we use?",` +
		`"options":[{"label":"pgx (recommended)","description":"modern, fast native Postgres driver"},` +
		`{"label":"lib/pq","description":"older, in maintenance mode"}]}`
	h := startAskScreenshotHarness(t, askArgs, "Which database driver")

	// Navigate down to the "Type something." row (index 2 = after the 2 options),
	// open it with Enter, then type a custom answer.
	h.tm.Send(tea.KeyMsg{Type: tea.KeyDown})
	h.tm.Send(tea.KeyMsg{Type: tea.KeyDown})
	h.tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	for _, r := range "sqlx + pgx under the hood" {
		h.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	time.Sleep(150 * time.Millisecond)

	frame := captureOverlayFrame(t, h.tm, "ask-freetext")
	if !strings.Contains(frame, "sqlx") {
		t.Errorf("free-text frame missing the typed answer:\n%s", frame)
	}
}
