package tui

// Opt-in LIVE end-to-end test: a REAL model (via the OpenAI-compatible gateway,
// e.g. OpenRouter) drives the actual ShellModel and DECIDES ON ITS OWN to call
// the blackboard tools in response to a natural dev prompt — nothing about the
// tool calls is scripted. It then opens the /blackboard tab and screenshots the
// board the model populated. This is the "organic" proof: no scriptedCompleter,
// real network, real tool-use decision.
//
// Skipped unless FUSE_LIVE_MODEL=1 and the gateway env is set, so it never runs
// in normal CI:
//
//	LLM_GATEWAY_URL=https://openrouter.ai/api/v1 \
//	LLM_GATEWAY_KEY=$OPENROUTER_API_KEY \
//	FUSE_LIVE_MODEL=1 FUSE_LIVE_MODEL_ID=openai/gpt-4o-mini \
//	FUSE_SCREENSHOT_DIR=$PWD/reports/screenshots \
//	go test ./internal/tui/ -run TestLive_BlackboardOrganic -count=1 -v

import (
	"bytes"
	"context"
	"net/http"
	"os"
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

func TestLive_BlackboardOrganic(t *testing.T) {
	if os.Getenv("FUSE_LIVE_MODEL") != "1" {
		t.Skip("set FUSE_LIVE_MODEL=1 (+ LLM_GATEWAY_URL/KEY) to run the live-model blackboard test")
	}
	url, key := os.Getenv("LLM_GATEWAY_URL"), os.Getenv("LLM_GATEWAY_KEY")
	if url == "" || key == "" {
		t.Skip("LLM_GATEWAY_URL and LLM_GATEWAY_KEY required for the live-model test")
	}
	modelID := os.Getenv("FUSE_LIVE_MODEL_ID")
	if modelID == "" {
		modelID = "openai/gpt-4o-mini"
	}

	// Real agent tree + real session blackboard, wired to the real tools exactly
	// as cmd/fuse does. A real gateway adapter is the completer — the model, not
	// a script, chooses whether to call the tools.
	tree := agent.NewAgentTree("alpha", modelID)
	root := tree.Node(tree.RootID())
	bb := agent.NewBlackboard(tree)

	reg := tools.NewRegistry()
	for _, tl := range tools.NewBlackboardTools(bb.ForNode(root)) {
		reg.Register(tl)
	}
	exec := regExec{reg: reg}

	adapter := model.NewAdapter(url, key, &http.Client{Timeout: 90 * time.Second})
	build := func(_ string, r agent.Renderer, _ permissions.ApprovalFunc) (*agent.Agent, error) {
		return agent.New(adapter, exec, r, modelID, "", 12, 1024), nil
	}
	m := NewShellModel("alpha", true, "", testRegistry(), nil, build, permissions.NewSessionMode(permissions.ModeOff), true).
		WithTree(tree).
		WithBlackboard(bb)

	bridgeCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(120, 34))
	StartBridges(bridgeCtx, tm.GetProgram(), m.Channel(), nil, nil)
	t.Cleanup(func() { tm.Quit() })

	// A genuine dev prompt — no mention of exact tool names as commands, just the
	// intent. The model must decide to use the blackboard tools it was given.
	prompt := "You are coordinating a code review of the fuse project. Use the shared " +
		"blackboard as your working memory: record a brief review plan under the key " +
		"plan/review, and record one concern about internal/tui/agents_model.go under " +
		"review/agents_model. Then list the blackboard keys to confirm they persisted."
	for _, r := range prompt {
		tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})

	// Wait until the model has actually written the plan key to the real store.
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := bb.Get("plan/review"); ok {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if _, ok := bb.Get("plan/review"); !ok {
		// Dump the transcript to explain what the model did instead.
		tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
		frame := captureFrame(t, tm, "blackboard-live-FAILED")
		t.Fatalf("model never wrote plan/review to the blackboard; transcript:\n%s", frame)
	}

	// The model's tool calls must be visible in the transcript...
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(stripANSI(b), []byte("blackboard_write"))
	}, teatest.WithDuration(20*time.Second), teatest.WithCheckInterval(50*time.Millisecond))

	// ...and the TURN MUST BE FULLY DONE before we open the overlay: while the
	// agent is running, ShellModel drops slash commands (the KeyEnter handler
	// returns early on m.running), so /blackboard would be swallowed and the
	// capture would grab the still-"Thinking…" main screen. After both keys land
	// the model does one short summary turn and then stops (no more tool calls);
	// wait for the second key, then let that final turn settle before typing.
	for time.Now().Before(deadline) {
		if _, ok := bb.Get("review/agents_model"); ok {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	time.Sleep(10 * time.Second) // let the (tool-call-free) summary turn finish so m.running clears

	for _, r := range "/blackboard" {
		tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		s := stripANSI(b)
		return bytes.Contains(s, []byte("Blackboard")) && bytes.Contains(s, []byte("plan/review"))
	}, teatest.WithDuration(10*time.Second), teatest.WithCheckInterval(50*time.Millisecond))

	frame := captureOverlayFrame(t, tm, "blackboard-live")

	// Both keys the prompt asked for must be on the board, written by alpha (the
	// real root agent), proving the model organically used the store.
	for _, want := range []string{"plan/review", "review/agents_model", "⟨written by alpha⟩"} {
		if !strings.Contains(frame, want) {
			t.Errorf("live board screenshot missing %q\n---\n%s", want, frame)
		}
	}
	t.Logf("live board rendered:\n%s", frame)
}
