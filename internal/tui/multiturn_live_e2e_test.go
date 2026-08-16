package tui

// multiturn_live_e2e_test.go is change 0066's live verification: it drives the
// REAL ShellModel, through a real bubbletea program (teatest), against a real
// model.Adapter POSTing to a SCRIPTED OpenAI-compatible gateway double — the
// same seam cmd/fuse wires in production. Nothing about the UI is faked: the
// keystrokes, the agent loop, the tool executor, the tree renderer, the bridge
// pump and the /agents overlay all run for real.
//
// House policy: verification traffic never touches Anthropic/Claude. The gateway
// here is an httptest server on localhost — no network egress at all.
//
// It sends THREE prompts on one session (one loop_id / one AgentTree), with real
// tool calls in the first two, then opens /agents and inspects the root detail
// pane and the blackboard, asserting the six operator-visible properties change
// 0066 is responsible for.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/tools"
)

// recordingEventSink is a real event.EventStore that keeps every emitted event
// so the test can assert on the loop's telemetry. Only Append is exercised.
type recordingEventSink struct {
	event.NoopStore // Subscribe/Replay are unused here
	mu              sync.Mutex
	events          []event.Event
}

func (s *recordingEventSink) Append(e event.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
	return nil
}

func (s *recordingEventSink) kinds() []event.Kind {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]event.Kind, len(s.events))
	for i, e := range s.events {
		out[i] = e.Kind
	}
	return out
}

// negativeOffsetRE matches the reported defect: a detail-pane / blackboard
// offset rendered as a negative number, e.g. "[-24013.7s]".
var negativeOffsetRE = regexp.MustCompile(`-\d+\.\d+s`)

// scriptedGatewayTurn describes one scripted conversational turn: an optional
// tool call the model "decides" to make, then the closing assistant text.
type scriptedGatewayTurn struct {
	toolName string
	toolArgs string
	reply    string
}

// newScriptedGateway stands up an OpenAI-compatible gateway double that plays
// the given turns. It picks its response from the conversation it is sent — the
// number of user messages is the conversational turn, and a trailing tool
// message means the tool already ran, so the turn should close. That keeps the
// double honest: it reacts to real request state rather than a call counter that
// could drift out of step with the agent loop.
func newScriptedGateway(t *testing.T, turns []scriptedGatewayTurn, pause time.Duration) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("gateway: read body: %v", err)
			return
		}
		var req struct {
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("gateway: unmarshal body: %v", err)
			return
		}
		mu.Lock()
		defer mu.Unlock()

		users := 0
		for _, m := range req.Messages {
			if m.Role == "user" {
				users++
			}
		}
		idx := users - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(turns) {
			idx = len(turns) - 1
		}
		spec := turns[idx]
		toolAlreadyRan := len(req.Messages) > 0 && req.Messages[len(req.Messages)-1].Role == "tool"

		// A deliberate pause makes each turn occupy real wall-clock time, so the
		// rendered per-turn offsets and durations are non-trivial (a zero-length
		// turn would make "no negative offsets" vacuous).
		time.Sleep(pause)

		w.Header().Set("Content-Type", "application/json")
		if spec.toolName == "" || toolAlreadyRan {
			fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q}}]}`, spec.reply)
			return
		}
		fmt.Fprintf(w,
			`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c%d","type":"function","function":{"name":%q,"arguments":%q}}]}}]}`,
			idx+1, spec.toolName, spec.toolArgs)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// liveStream mirrors the program's rendered output into a buffer a test can
// re-read at will. teatest.WaitFor consumes tm.Output(), so a test that needs to
// assert over EVERY frame the session ever painted (here: the whole scrolled
// transcript, not just the last window) must own the reader itself.
type liveStream struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	stop chan struct{}
}

// newLiveStream starts draining the program's output into the buffer. teatest's
// output is a bytes.Buffer, so a read on a momentarily-empty buffer returns
// io.EOF rather than blocking — the loop must poll through EOF, not exit on it,
// or it stops after the very first frame. It exits when the test signals stop.
func newLiveStream(tm *teatest.TestModel) *liveStream {
	ls := &liveStream{stop: make(chan struct{})}
	go func() {
		b := make([]byte, 32*1024)
		out := tm.Output()
		for {
			n, err := out.Read(b)
			if n > 0 {
				ls.mu.Lock()
				ls.buf.Write(b[:n])
				ls.mu.Unlock()
				continue
			}
			if err != nil && err != io.EOF {
				return
			}
			select {
			case <-ls.stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	return ls
}

func (ls *liveStream) close() { close(ls.stop) }

func (ls *liveStream) text() string {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return string(stripANSI(ls.buf.Bytes()))
}

// waitFor blocks until want appears in anything the program has rendered so far.
func (ls *liveStream) waitFor(t *testing.T, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(ls.text(), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in rendered output; last frame:\n%s", want, tailLines(ls.text(), 40))
}

// forceRepaint makes the program emit a COMPLETE frame. bubbletea's renderer
// normally writes only the lines that changed, so the most recent full frame in
// the output stream can be many keystrokes stale — reading current UI state off
// it silently lies (it made a working `enter` toggle look broken during this
// change's verification). A window-size change forces a full repaint.
func forceRepaint(tm *teatest.TestModel) {
	tm.Send(tea.WindowSizeMsg{Width: 121, Height: 34})
	time.Sleep(60 * time.Millisecond)
	tm.Send(tea.WindowSizeMsg{Width: 120, Height: 34})
	time.Sleep(60 * time.Millisecond)
}

// lastPaneFrame extracts the most recently painted overlay frame (from its
// top border to its bottom border) so a log line shows one coherent screen
// rather than an arbitrary tail across several repaints.
func lastPaneFrame(s string) string {
	i := strings.LastIndex(s, "┌─ agents")
	if i < 0 {
		return tailLines(s, 40)
	}
	rest := s[i:]
	if j := strings.Index(rest, "└"); j >= 0 {
		if k := strings.Index(rest[j:], "\n"); k >= 0 {
			return rest[:j+k]
		}
	}
	return rest
}

// waitForFrame blocks until the CURRENTLY painted overlay frame contains want.
// waitFor searches everything ever rendered, which is right for "did this ever
// appear" but useless for a state TOGGLE — an earlier frame keeps satisfying it
// forever. Anything asserting current UI state must go through this.
func (ls *liveStream) waitForFrame(t *testing.T, tm *teatest.TestModel, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		forceRepaint(tm)
		last = lastPaneFrame(ls.text())
		if strings.Contains(last, want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in the CURRENT frame; frame was:\n%s", want, last)
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// TestLiveMultiTurn_TurnGroupsAndOffsets is change 0066's end-to-end proof: a
// genuine three-prompt session on one tree, driven by keystrokes through a real
// agent loop against a scripted gateway, then inspected through the real
// /agents overlay.
func TestLiveMultiTurn_TurnGroupsAndOffsets(t *testing.T) {
	gw := newScriptedGateway(t, []scriptedGatewayTurn{
		{toolName: "blackboard_write", toolArgs: `{"key":"plan/turn-one","value":"{\"step\":1}"}`, reply: "turn-one-complete"},
		{toolName: "blackboard_write", toolArgs: `{"key":"plan/turn-two","value":"{\"step\":2}"}`, reply: "turn-two-complete"},
		{reply: "turn-three-complete"},
	}, 400*time.Millisecond)

	// Real tree, real blackboard, real tool registry — exactly cmd/fuse's wiring.
	tree := agent.NewAgentTree("alpha", "test/scripted")
	root := tree.Node(tree.RootID())
	bb := agent.NewBlackboard(tree)

	reg := tools.NewRegistry()
	for _, tl := range tools.NewBlackboardTools(bb.ForNode(root)) {
		reg.Register(tl)
	}

	// A real event sink on every turn's agent: change 0066 CONSUMES the event
	// stream and must not perturb it, so the session's telemetry is recorded and
	// asserted rather than assumed.
	sink := &recordingEventSink{}
	build := func(_ string, r agent.Renderer, _ permissions.ApprovalFunc) (*agent.Agent, error) {
		a := agent.New(
			model.NewAdapter(gw.URL, "tkn", gw.Client()),
			reg, r, "test/scripted", "", 8, 256,
		)
		a.SetEventSink(sink)
		return a, nil
	}
	m := NewShellModel("alpha", false, "", testRegistry(), nil, build,
		permissions.NewSessionMode(permissions.ModeOff), true).
		WithTree(tree).
		WithBlackboard(bb)

	bridgeCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(120, 34))
	StartBridges(bridgeCtx, tm.GetProgram(), m.Channel(), nil, nil)
	ls := newLiveStream(tm)
	t.Cleanup(func() { ls.close(); tm.Quit() }) //nolint:errcheck // best-effort teardown

	send := func(s string) {
		for _, r := range s {
			tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		}
	}
	prompt := func(s, done string) {
		send(s)
		tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
		ls.waitFor(t, done, 30*time.Second)
	}

	sessionStart := time.Now()
	prompt("record the first plan step on the board", "turn-one-complete")
	prompt("now record the second plan step", "turn-two-complete")
	prompt("summarize what you recorded", "turn-three-complete")
	sessionElapsed := time.Since(sessionStart)

	// The session must genuinely be three turns on ONE root node with real events.
	rv := root.Snapshot()
	if len(rv.Turns) != 3 {
		t.Fatalf("want 3 turn marks on the root, got %d (%+v)", len(rv.Turns), rv.Turns)
	}
	if got := len(root.CopyEvents()); got < 6 {
		t.Fatalf("want a real event burst across turns, got %d root events", got)
	}
	if _, ok := bb.Get("plan/turn-one"); !ok {
		t.Fatal("turn 1's tool call never reached the real blackboard store")
	}
	if _, ok := bb.Get("plan/turn-two"); !ok {
		t.Fatal("turn 2's tool call never reached the real blackboard store")
	}

	// --- Observation 6: telemetry is untouched -------------------------------
	// The loop emits turn.start/turn.end per INNER tool-loop iteration; the two
	// tool-using turns run two iterations each and the last runs one, so a
	// three-prompt session yields five of each. The UI's TurnMark ordinal is a
	// separate, UI-local concept and must not have leaked into the stream.
	var turnStarts, turnEnds int
	for _, k := range sink.kinds() {
		switch k {
		case event.KindTurnStart:
			turnStarts++
		case event.KindTurnEnd:
			turnEnds++
		}
	}
	if turnStarts != 5 || turnEnds != 5 {
		t.Errorf("turn.start/turn.end across the session = %d/%d, want 5/5 (kinds: %v)", turnStarts, turnEnds, sink.kinds())
	}
	// The loop's Turn field is its 0-based INNER iteration counter and restarts
	// at each prompt: 0,1 / 0,1 / 0. If the UI's conversational ordinal had leaked
	// into the stream this sequence would be monotonic instead.
	var seq []int
	sink.mu.Lock()
	for _, e := range sink.events {
		if e.Kind == event.KindTurnStart {
			seq = append(seq, e.Turn)
		}
	}
	sink.mu.Unlock()
	if want := []int{0, 1, 0, 1, 0}; fmt.Sprint(seq) != fmt.Sprint(want) {
		t.Errorf("turn.start Turn sequence = %v, want %v (the loop counter must be untouched)", seq, want)
	}

	// --- Observation 4: the headline timer tracks the CURRENT turn -----------
	// Read it exactly as the overlay does, off the live snapshot.
	turn3 := rv.Turns[2].EndedAt.Sub(rv.Turns[2].StartedAt)
	if turn3 <= 0 || turn3 >= sessionElapsed*3/4 {
		t.Errorf("headline timer does not track the current turn: turn3=%v session=%v", turn3, sessionElapsed)
	}
	if headline := nodeElapsed(rv); headline != formatNodeElapsed(turn3) {
		t.Errorf("nodeElapsed = %q, want turn 3's duration %q", headline, formatNodeElapsed(turn3))
	}

	// --- Drive the real overlay ---------------------------------------------
	send("/agents")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	ls.waitFor(t, "j/k", 10*time.Second)
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter}) // enter the root node's detail pane
	ls.waitFor(t, "turn 3", 10*time.Second)

	// --- Observation 2: headers, ordinals, previews, collapse defaults -------
	frame := ls.text()
	for _, want := range []string{
		`▸ turn 1 · "record the first plan step on the board"`,
		`▸ turn 2 · "now record the second plan step"`,
		`▾ turn 3 · "summarize what you recorded"`,
	} {
		if !strings.Contains(frame, want) {
			t.Errorf("detail pane missing turn header %q\n%s", want, tailLines(frame, 40))
		}
	}

	// --- Observation 3: enter toggles a prior turn's group -------------------
	// Walk the cursor to turn 1's header (top of the pane) and toggle it twice,
	// re-rendering between toggles, then leave it expanded.
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	ls.waitForFrame(t, tm, `▸ turn 1`, 5*time.Second)
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter}) // expand turn 1
	ls.waitForFrame(t, tm, `▾ turn 1`, 5*time.Second)
	ls.waitForFrame(t, tm, "blackboard_write", 5*time.Second) // its events are really there
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})                   // collapse it again
	ls.waitForFrame(t, tm, `▸ turn 1`, 5*time.Second)
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter}) // expand once more and stay there
	ls.waitForFrame(t, tm, `▾ turn 1`, 5*time.Second)

	// The cursor must still be on turn 1's header after three toggles — selection
	// coherence is the thing the row model most easily gets wrong.
	if !strings.Contains(lastPaneFrame(ls.text()), `▾ turn 1`) {
		t.Error("turn 1 did not stay expanded after the third toggle")
	}
	toggledFrame := lastPaneFrame(ls.text())

	// Expand turn 2 as well, then scroll the WHOLE transcript so every row of
	// every turn is painted at least once — observation 1 is only meaningful
	// over the full transcript, not the visible window.
	for i := 0; i < 4; i++ {
		tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	}
	ls.waitForFrame(t, tm, `▸ turn 2`, 5*time.Second)
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter}) // expand turn 2
	ls.waitForFrame(t, tm, `▾ turn 2`, 5*time.Second)
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}})
	forceRepaint(tm)
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	forceRepaint(tm)
	topFrame := lastPaneFrame(ls.text())
	for i := 0; i < 30; i++ {
		tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
		time.Sleep(15 * time.Millisecond)
	}
	forceRepaint(tm)

	// With all three turns expanded, every turn's events must have been painted.
	all := ls.text()
	for _, want := range []string{"turn-one-complete", "turn-two-complete", "turn-three-complete"} {
		if !strings.Contains(all, want) {
			t.Errorf("expanded transcript never rendered %q — the scroll did not cover every turn", want)
		}
	}

	// --- Observation 1: NO negative offsets anywhere, ever -------------------
	if hits := negativeOffsetRE.FindAllString(ls.text(), -1); len(hits) > 0 {
		t.Errorf("negative offsets rendered in the live session (the 0066 defect): %v", hits)
	}

	detailFrame := ls.text()

	// --- Observation 5: the blackboard ---------------------------------------
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	ls.waitFor(t, "plan/turn-one", 10*time.Second)
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	forceRepaint(tm)
	boardFrame := ls.text()
	if !strings.Contains(boardFrame, "── turn 1 ──") || !strings.Contains(boardFrame, "── turn 2 ──") {
		t.Errorf("blackboard is missing per-turn sub-dividers:\n%s", tailLines(boardFrame, 40))
	}
	if !strings.Contains(boardFrame, "written by alpha · +") {
		t.Errorf("blackboard entry meta is missing the turn-relative offset:\n%s", tailLines(boardFrame, 40))
	}
	// The offset is inline prose here: zero padding ("+000.4s") reads as a
	// rendering fault to an operator, so the board uses the unpadded form.
	if regexp.MustCompile(`written by [^⟩]*· \+0\d`).MatchString(boardFrame) {
		t.Errorf("zero-padded offset rendered on the live board:\n%s", tailLines(boardFrame, 40))
	}
	if hits := negativeOffsetRE.FindAllString(boardFrame, -1); len(hits) > 0 {
		t.Errorf("negative offsets on the blackboard: %v", hits)
	}
	// n/p must still land exactly on writer-group starts with dividers present.
	// Assert that against the render path itself, which is what scroll uses.
	am := NewAgentsModel(tree, nil).WithBlackboard(bb)
	am.width, am.height = 120, 34
	body, starts := am.blackboardBody(bb.Snapshot(), am.blackboardContentWidth())
	if len(starts) == 0 {
		t.Fatal("blackboard reported no writer-group starts")
	}
	if got := am.blackboardGroupStarts(); len(got) != len(starts) {
		t.Errorf("blackboardGroupStarts()=%v diverged from the render path %v", got, starts)
	}
	for _, s := range starts {
		if s < 0 || s >= len(body) {
			t.Fatalf("group start %d out of range (body=%d)", s, len(body))
		}
		if !strings.Contains(string(stripANSI([]byte(body[s]))), "▌ ") {
			t.Errorf("group start line %d is not a writer header: %q", s, body[s])
		}
	}

	// Capture the board as the operator sees it, then the detail pane text.
	boardShot := captureOverlayFrame(t, tm, "0066-live-blackboard-turns")
	t.Logf("live blackboard frame:\n%s", boardShot)
	t.Logf("live detail pane, turn 1 re-expanded by enter:\n%s", toggledFrame)
	t.Logf("live detail pane, all three turns expanded, scrolled to the top:\n%s", topFrame)
	t.Logf("live detail pane, bottom of the scrolled transcript:\n%s", lastPaneFrame(detailFrame))
}
