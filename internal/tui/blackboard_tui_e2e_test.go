package tui

// End-to-end TUI verification for change 0023 (shared agent blackboard),
// mirroring the MCP round-trip e2e (mcp_tui_e2e_test.go). Two live-program
// tests prove the two headline pieces of the feature reach the screen through
// the REAL ShellModel + bridge, and each writes a visual-confirmation
// screenshot (.ansi/.txt/.png via captureFrame → FUSE_SCREENSHOT_DIR):
//
//  1. TestTUI_BlackboardToolRoundTrip — a scripted model drives the REAL
//     blackboard tools (blackboard_write/keys/read) wired to a REAL
//     agent.Blackboard through the live TUI, proving the tool→store round trip
//     runs inside the actual program (the read value flows back to the model).
//  2. TestTUI_BlackboardTabScreenshot — the /agents overlay Blackboard section
//     (pressed live as "/agents" then "b") renders prepopulated keys, JSON
//     values, and per-entry writer provenance.
//
// Together they are the screenshot-backed proof that 0023's tool and tab work
// end-to-end, not just in direct-View unit tests.

import (
	"bytes"
	"context"
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

// TestTUI_BlackboardToolRoundTrip drives a turn whose scripted model calls the
// real blackboard tools — write a plan, list keys, read it back — against a REAL
// agent.Blackboard reached through the same tools.Registry → MCP-style
// regExec seam a live session uses. Turn 2 replies and stops. It proves the
// tool→store→tool path runs inside the live program: the value written in one
// tool call is read back in a later one and fed to the model, and the store
// actually holds it afterward. Captures the settled transcript screenshot.
func TestTUI_BlackboardToolRoundTrip(t *testing.T) {
	// A real session-scoped blackboard owned by a real agent tree, wired to the
	// tools exactly as cmd/fuse does: NewBlackboardTools(bb.ForNode(root)).
	tree := agent.NewAgentTree("alpha", "test/model")
	root := tree.Node(tree.RootID())
	bb := agent.NewBlackboard(tree)

	reg := tools.NewRegistry()
	for _, tl := range tools.NewBlackboardTools(bb.ForNode(root)) {
		reg.Register(tl)
	}
	for _, name := range []string{"blackboard_write", "blackboard_read", "blackboard_keys"} {
		if !reg.Has(name) {
			t.Fatalf("blackboard tool %s not registered", name)
		}
	}

	// Turn 1: write a plan value, list the keys, read the plan back.
	// Turn 2: reply and stop the loop.
	cmp := &scriptedCompleter{responses: []model.CompletionResp{
		{ToolCalls: []model.ToolCall{
			{ID: "1", Name: "blackboard_write", Arguments: `{"key":"plan/outline","value":"{\"steps\":3}"}`},
			{ID: "2", Name: "blackboard_keys", Arguments: `{"pattern":"plan/*"}`},
			{ID: "3", Name: "blackboard_read", Arguments: `{"key":"plan/outline"}`},
		}},
		{Content: "blackboard-round-trip-complete"},
	}}

	exec := regExec{reg: reg}
	build := func(_ string, r agent.Renderer, _ permissions.ApprovalFunc) (*agent.Agent, error) {
		return agent.New(cmp, exec, r, "test/model", "", 25, 0), nil
	}
	// verbose=true so full tool result descriptors render in the transcript.
	m := NewShellModel("alpha", true, "", testRegistry(), nil, build, permissions.NewSessionMode(permissions.ModeSmart), true)

	bridgeCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(96, 30))
	StartBridges(bridgeCtx, tm.GetProgram(), m.Channel(), nil, nil)
	t.Cleanup(func() { tm.Quit() })

	for _, r := range "use the blackboard" {
		tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})

	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(stripANSI(b), []byte("blackboard-round-trip-complete"))
	}, teatest.WithDuration(15*time.Second), teatest.WithCheckInterval(20*time.Millisecond))

	// The store must actually hold the value written through the live program.
	entry, ok := bb.Get("plan/outline")
	if !ok {
		t.Fatal("blackboard has no plan/outline after the live tool round trip")
	}
	if entry.WriterLabel != "alpha" {
		t.Errorf("plan/outline writer provenance = %q, want alpha", entry.WriterLabel)
	}

	// The read tool's result — the JSON value — must have been fed back to the
	// model on the SECOND request, proving write→read flowed through the store
	// inside the live TUI (not just that the tools were called).
	if len(cmp.requests) < 2 {
		t.Fatalf("expected >=2 completer requests (tool results never fed back), got %d", len(cmp.requests))
	}
	sent := renderRequest(cmp.requests[1])
	if !strings.Contains(sent, `{"steps":3}`) {
		t.Errorf("read-back blackboard value missing from agent request:\n%s", sent)
	}
	if !strings.Contains(sent, "plan/outline") {
		t.Errorf("blackboard_keys listing missing from agent request:\n%s", sent)
	}

	// Visual-confirmation screenshot of the settled round-trip transcript.
	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
	frame := captureFrame(t, tm, "blackboard-roundtrip")
	if !strings.Contains(frame, "blackboard_write") {
		t.Errorf("screenshot frame missing the blackboard_write call\n%s", frame)
	}
	if !strings.Contains(frame, "blackboard-round-trip-complete") {
		t.Errorf("screenshot frame missing the final reply\n%s", frame)
	}
}

// TestTUI_BlackboardTabScreenshot opens the /agents overlay's Blackboard section
// through the LIVE program — typing "/agents" then pressing "b" — against a
// ShellModel wired with a real tree and a prepopulated blackboard, and captures
// a screenshot of the rendered tab. It is the visual proof that 0023's tab shows
// each key, its JSON value, and per-entry writer provenance to a real user.
func TestTUI_BlackboardTabScreenshot(t *testing.T) {
	tree := agent.NewAgentTree("alpha", "test/model")
	root := tree.Node(tree.RootID())
	bb := agent.NewBlackboard(tree)
	// Two entries with distinct provenance: a structured plan written by root,
	// and a facet result written by a child agent.
	bb.Put("plan/outline", map[string]any{"steps": float64(3)}, root.ID, "alpha")
	bb.Put("facet/sqlite", "supported", "child-2", "research/facet-2")

	// No agent turn runs here; a completer that never returns tool calls is fine.
	build := func(_ string, r agent.Renderer, _ permissions.ApprovalFunc) (*agent.Agent, error) {
		return agent.New(assistantOnce(""), noopToolExec{}, r, "test/model", "", 25, 0), nil
	}
	m := NewShellModel("alpha", false, "", testRegistry(), nil, build, permissions.NewSessionMode(permissions.ModeSmart), true).
		WithTree(tree).
		WithBlackboard(bb)

	bridgeCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(110, 30))
	StartBridges(bridgeCtx, tm.GetProgram(), m.Channel(), nil, nil)
	t.Cleanup(func() { tm.Quit() })

	// Open the overlay via the real /agents slash command...
	for _, r := range "/agents" {
		tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	// ...then select the Blackboard section.
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})

	// Wait for the tab to render both keys.
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		s := stripANSI(b)
		return bytes.Contains(s, []byte("plan/outline")) && bytes.Contains(s, []byte("facet/sqlite"))
	}, teatest.WithDuration(10*time.Second), teatest.WithCheckInterval(20*time.Millisecond))

	// The overlay swallows key input (Ctrl+C included), so tear the program down
	// with tm.Quit() rather than a keystroke; the overlay state survives on the
	// final model, so its View() renders the Blackboard tab.
	frame := captureOverlayFrame(t, tm, "blackboard-tab")

	for _, want := range []string{
		"plan/outline",     // structured key
		`"steps": 3`,       // its JSON value (pretty-printed, change 0041 Task 6)
		"facet/sqlite",     // second key
		"research/facet-2", // child writer provenance
	} {
		if !strings.Contains(frame, want) {
			t.Errorf("blackboard-tab screenshot missing %q\n---\n%s", want, frame)
		}
	}
}

// boardShellModel builds a ShellModel wired with a real tree + a prepopulated
// blackboard, ready to drive through the live program. Shared by the discovery
// tests below.
func boardShellModel(t *testing.T) ShellModel {
	t.Helper()
	tree := agent.NewAgentTree("alpha", "test/model")
	root := tree.Node(tree.RootID())
	bb := agent.NewBlackboard(tree)
	bb.Put("plan/outline", map[string]any{"steps": float64(3)}, root.ID, "alpha")
	build := func(_ string, r agent.Renderer, _ permissions.ApprovalFunc) (*agent.Agent, error) {
		return agent.New(assistantOnce(""), noopToolExec{}, r, "test/model", "", 25, 0), nil
	}
	return NewShellModel("alpha", false, "", testRegistry(), nil, build, permissions.NewSessionMode(permissions.ModeSmart), true).
		WithTree(tree).
		WithBlackboard(bb)
}

// TestTUI_BlackboardSlashOpensBoard proves the /blackboard slash command lands
// the overlay DIRECTLY on the Blackboard section — no "b" press needed — driven
// as real keystrokes through the live program. This is the dedicated entry point
// the user asked for alongside the in-overlay "b" swap.
func TestTUI_BlackboardSlashOpensBoard(t *testing.T) {
	m := boardShellModel(t)

	bridgeCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(110, 30))
	StartBridges(bridgeCtx, tm.GetProgram(), m.Channel(), nil, nil)
	t.Cleanup(func() { tm.Quit() })

	for _, r := range "/blackboard" {
		tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})

	// The board header and the entry must appear WITHOUT any "b" keystroke.
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		s := stripANSI(b)
		return bytes.Contains(s, []byte("Blackboard")) && bytes.Contains(s, []byte("plan/outline"))
	}, teatest.WithDuration(10*time.Second), teatest.WithCheckInterval(20*time.Millisecond))

	frame := captureOverlayFrame(t, tm, "blackboard-slash")
	if !strings.Contains(frame, "plan/outline") || !strings.Contains(frame, `"steps": 3`) {
		t.Errorf("/blackboard did not open on the board\n%s", frame)
	}
}

// TestTUI_BlackboardReachableFromDetail proves the "b" swap into the board works
// from a node's DETAIL view, not just the top-level tree list — the "another
// swap" the user asked for. Flow: /agents → Enter (open detail of the root node)
// → b (board). The board must render even though we entered from detail.
func TestTUI_BlackboardReachableFromDetail(t *testing.T) {
	m := boardShellModel(t)

	bridgeCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(110, 30))
	StartBridges(bridgeCtx, tm.GetProgram(), m.Channel(), nil, nil)
	t.Cleanup(func() { tm.Quit() })

	// Open the agents overlay on the tree list.
	for _, r := range "/agents" {
		tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return bytes.Contains(stripANSI(b), []byte("j/k"))
	}, teatest.WithDuration(10*time.Second), teatest.WithCheckInterval(20*time.Millisecond))

	// Enter the selected node's detail view, THEN swap to the board with "b".
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})

	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		s := stripANSI(b)
		return bytes.Contains(s, []byte("Blackboard")) && bytes.Contains(s, []byte("plan/outline"))
	}, teatest.WithDuration(10*time.Second), teatest.WithCheckInterval(20*time.Millisecond))

	frame := captureOverlayFrame(t, tm, "blackboard-from-detail")
	if !strings.Contains(frame, "plan/outline") {
		t.Errorf("b did not reach the board from the detail view\n%s", frame)
	}
}
