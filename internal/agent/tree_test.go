package agent

import (
	"sync"
	"testing"
	"time"
)

func TestSpawnBudgetCountsChildrenNotRoot(t *testing.T) {
	tree := NewAgentTree("root", "m")
	tree.SetMaxSpawns(16)

	used, max := tree.SpawnBudget()
	if used != 0 || max != 16 {
		t.Fatalf("fresh tree budget = %d/%d, want 0/16 (root excluded)", used, max)
	}

	rootID := tree.RootID()
	tree.addNode(&AgentNode{ID: newNodeID(), ParentID: rootID, Label: "c1"})
	tree.addNode(&AgentNode{ID: newNodeID(), ParentID: rootID, Label: "c2"})

	used, max = tree.SpawnBudget()
	if used != 2 || max != 16 {
		t.Errorf("after 2 spawns budget = %d/%d, want 2/16", used, max)
	}
}

func TestSpawnBudgetZeroMaxMeansUnset(t *testing.T) {
	// A tree whose max was never set reports max 0 — the spawner treats that as
	// "no budget configured" and does not enforce one.
	tree := NewAgentTree("root", "m")
	if _, max := tree.SpawnBudget(); max != 0 {
		t.Errorf("unset max = %d, want 0", max)
	}
}

func TestMaxConcurrentSpawnsDefaultIs16(t *testing.T) {
	if MaxConcurrentSpawns != 16 {
		t.Fatalf("MaxConcurrentSpawns = %d, want 16", MaxConcurrentSpawns)
	}
}

func TestNewAgentTreeWithConcurrencySizesSemaphore(t *testing.T) {
	// The concurrency figure sizes the scheduler's slot pool (change 0036 moved
	// the semaphore off the tree onto the scheduler it owns).
	tr := NewAgentTreeWithConcurrency("root", "m", 3)
	if tr.sched.slotCap != 3 {
		t.Fatalf("scheduler sem cap = %d, want 3", tr.sched.slotCap)
	}
}

func TestNewAgentTreeWithConcurrencyFallsBackOnZero(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 0)
	if tr.sched.slotCap != MaxConcurrentSpawns {
		t.Fatalf("scheduler sem cap = %d, want %d", tr.sched.slotCap, MaxConcurrentSpawns)
	}
}

func TestNewAgentTreeUsesDefaultConcurrency(t *testing.T) {
	tr := NewAgentTree("root", "m")
	if tr.sched.slotCap != MaxConcurrentSpawns {
		t.Fatalf("scheduler sem cap = %d, want %d", tr.sched.slotCap, MaxConcurrentSpawns)
	}
}

func TestNewAgentTree(t *testing.T) {
	tree := NewAgentTree("root", "claude-3-5-sonnet")
	if tree.RootID() == "" {
		t.Fatal("RootID must be non-empty")
	}
	node := tree.Node(tree.RootID())
	if node == nil {
		t.Fatal("Node(RootID()) must return the root node")
	}
	if node.Label != "root" {
		t.Errorf("root label = %q, want %q", node.Label, "root")
	}
	if node.Model != "claude-3-5-sonnet" {
		t.Errorf("root model = %q, want %q", node.Model, "claude-3-5-sonnet")
	}
}

func TestNewAgentTreeRootStartsIdle(t *testing.T) {
	// The root node must not carry a running clock before the first turn: the
	// agents tab renders idle (no elapsed time) for a zero StartedAt. BeginTurn
	// starts the clock at prompt submit.
	tree := NewAgentTree("root", "m")
	root := tree.Node(tree.RootID())
	if !root.StartedAt.IsZero() {
		t.Errorf("fresh root StartedAt = %v, want zero (idle before first prompt)", root.StartedAt)
	}

	tree.BeginTurn()
	root = tree.Node(tree.RootID())
	if root.StartedAt.IsZero() {
		t.Error("after BeginTurn root StartedAt is still zero, want the turn clock started")
	}
}

func TestSetRootModelUpdatesLabelAndModel(t *testing.T) {
	// /model switches the session model; the tree root's rendered label/model must
	// follow so the agents tab shows the current selection, not the startup default.
	tree := NewAgentTree("alpha", "alpha")

	// Drain the construction update, then assert SetRootModel emits one.
	select {
	case <-tree.Updates():
	default:
	}

	tree.SetRootModel("beta")

	root := tree.Node(tree.RootID())
	if root.Model != "beta" {
		t.Errorf("root Model = %q, want %q", root.Model, "beta")
	}
	if root.Label != "beta" {
		t.Errorf("root Label = %q, want %q", root.Label, "beta")
	}

	select {
	case up := <-tree.Updates():
		if up.NodeID != tree.RootID() {
			t.Errorf("emitted update NodeID = %q, want root %q", up.NodeID, tree.RootID())
		}
	default:
		t.Error("SetRootModel did not emit a tree update for the root")
	}
}

func TestAgentNodeAddEvent(t *testing.T) {
	t.Parallel()
	node := &AgentNode{ID: "n1"}

	const goroutines = 10
	const eventsEach = 20
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < eventsEach; j++ {
				node.AddEvent(AgentEvent{Kind: KindAssistant, Name: "msg"})
			}
		}()
	}
	wg.Wait()

	node.mu.Lock()
	total := len(node.Events)
	node.mu.Unlock()

	if total != goroutines*eventsEach {
		t.Fatalf("expected %d events, got %d", goroutines*eventsEach, total)
	}
}

func TestAgentNodeFinish(t *testing.T) {
	t.Run("done_no_error", func(t *testing.T) {
		node := &AgentNode{ID: "n2"}
		before := time.Now()
		node.Finish(StatusDone, "")
		after := time.Now()

		if node.Status != StatusDone {
			t.Errorf("status = %v, want %v", node.Status, StatusDone)
		}
		if node.EndedAt.Before(before) || node.EndedAt.After(after) {
			t.Error("EndedAt is outside the expected time range")
		}
		if len(node.Events) != 0 {
			t.Errorf("expected no events for empty errMsg, got %d", len(node.Events))
		}
	})

	t.Run("error_appends_event", func(t *testing.T) {
		node := &AgentNode{ID: "n3"}
		node.Finish(StatusError, "something went wrong")

		if node.Status != StatusError {
			t.Errorf("status = %v, want %v", node.Status, StatusError)
		}
		if len(node.Events) != 1 {
			t.Fatalf("expected 1 event, got %d", len(node.Events))
		}
		evt := node.Events[0]
		if evt.Kind != KindError {
			t.Errorf("event kind = %v, want %v", evt.Kind, KindError)
		}
		if msg, _ := evt.Payload["error"].(string); msg != "something went wrong" {
			t.Errorf("event error payload = %q, want %q", msg, "something went wrong")
		}
	})
}

func TestAgentTreeEmitNonBlocking(t *testing.T) {
	tree := NewAgentTree("root", "m")
	// Fill the channel (buffered 256).
	for i := 0; i < 256; i++ {
		tree.Emit(TreeUpdate{NodeID: "n"})
	}

	// A 257th Emit must not block.
	done := make(chan struct{})
	go func() {
		tree.Emit(TreeUpdate{NodeID: "n"})
		close(done)
	}()

	select {
	case <-done:
		// success
	case <-time.After(200 * time.Millisecond):
		t.Fatal("257th Emit blocked; expected non-blocking drop to dirty set")
	}
}

func TestAgentTreeSnapshotAllOrder(t *testing.T) {
	tree := NewAgentTree("root", "m")
	rootID := tree.RootID()

	child := &AgentNode{ID: newNodeID(), ParentID: rootID, Label: "child1"}
	tree.addNode(child)

	grandchild := &AgentNode{ID: newNodeID(), ParentID: child.ID, Label: "grandchild"}
	tree.addNode(grandchild)

	nodes := tree.SnapshotAll()

	if len(nodes) != 3 {
		t.Fatalf("expected 3 nodes, got %d", len(nodes))
	}
	// Depth-first: root first
	if nodes[0].ID != rootID {
		t.Errorf("first node should be root, got %q", nodes[0].Label)
	}
	if nodes[1].ID != child.ID {
		t.Errorf("second node should be child, got %q", nodes[1].Label)
	}
	if nodes[2].ID != grandchild.ID {
		t.Errorf("third node should be grandchild, got %q", nodes[2].Label)
	}
}

func TestNodeStatus_String(t *testing.T) {
	cases := []struct {
		status NodeStatus
		want   string
	}{
		{StatusPending, "pending"},
		{StatusRunning, "running"},
		{StatusDone, "done"},
		{StatusError, "error"},
		{StatusCancelled, "cancelled"},
		{NodeStatus(99), "unknown"},
	}
	for _, tc := range cases {
		if got := tc.status.String(); got != tc.want {
			t.Errorf("NodeStatus(%d).String() = %q, want %q", tc.status, got, tc.want)
		}
	}
}

func TestEventKind_String(t *testing.T) {
	cases := []struct {
		kind EventKind
		want string
	}{
		{KindAssistant, "assistant"},
		{KindToolCall, "tool_call"},
		{KindToolResult, "tool_result"},
		{KindTokens, "tokens"},
		{KindDone, "done"},
		{KindError, "error"},
		{EventKind(99), "unknown"},
	}
	for _, tc := range cases {
		if got := tc.kind.String(); got != tc.want {
			t.Errorf("EventKind(%d).String() = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

// --- change 0066: per-turn marks on the root node -------------------------

func TestBeginTurnRecordsSequentialTurnMarks(t *testing.T) {
	tree := NewAgentTree("root", "m")

	tree.BeginTurn()
	time.Sleep(2 * time.Millisecond)
	tree.BeginTurn()

	turns := tree.Node(tree.RootID()).Snapshot().Turns
	if len(turns) != 2 {
		t.Fatalf("len(Turns) = %d, want 2", len(turns))
	}
	if turns[0].Turn != 1 || turns[1].Turn != 2 {
		t.Errorf("turn ordinals = %d, %d; want 1, 2", turns[0].Turn, turns[1].Turn)
	}
	if !turns[1].StartedAt.After(turns[0].StartedAt) {
		t.Errorf("turn 2 StartedAt %v is not after turn 1 %v", turns[1].StartedAt, turns[0].StartedAt)
	}
	if !turns[0].EndedAt.IsZero() || !turns[1].EndedAt.IsZero() {
		t.Error("in-flight turn marks must have a zero EndedAt")
	}
}

// TestBeginTurnDoesNotRewriteRootStartedAt is the regression guard for the
// reported bug (change 0066): a second BeginTurn used to clobber
// root.StartedAt, so every prior-turn event rendered a NEGATIVE offset
// ("[-24013.7s]") because offsets are computed against root.StartedAt.
// StartedAt is set once, by the first BeginTurn, and never rewritten.
func TestBeginTurnDoesNotRewriteRootStartedAt(t *testing.T) {
	tree := NewAgentTree("root", "m")
	if !tree.Node(tree.RootID()).Snapshot().StartedAt.IsZero() {
		t.Fatal("fresh root StartedAt must be zero (idle before the first prompt)")
	}

	tree.BeginTurn()
	first := tree.Node(tree.RootID()).Snapshot().StartedAt
	if first.IsZero() {
		t.Fatal("first BeginTurn must set root StartedAt")
	}

	time.Sleep(2 * time.Millisecond)
	tree.BeginTurn()

	after := tree.Node(tree.RootID()).Snapshot().StartedAt
	if !after.Equal(first) {
		t.Errorf("root StartedAt = %v after second BeginTurn, want it unchanged at %v", after, first)
	}
}

func TestEndTurnStampsOnlyTheLastTurnMark(t *testing.T) {
	tree := NewAgentTree("root", "m")

	tree.BeginTurn()
	tree.EndTurn(false)
	firstEnded := tree.Node(tree.RootID()).Snapshot().Turns[0].EndedAt
	if firstEnded.IsZero() {
		t.Fatal("EndTurn must stamp EndedAt on the in-flight mark")
	}

	time.Sleep(2 * time.Millisecond)
	tree.BeginTurn()
	tree.EndTurn(true)

	turns := tree.Node(tree.RootID()).Snapshot().Turns
	if len(turns) != 2 {
		t.Fatalf("len(Turns) = %d, want 2", len(turns))
	}
	if !turns[0].EndedAt.Equal(firstEnded) {
		t.Errorf("turn 1 EndedAt = %v, want it untouched at %v", turns[0].EndedAt, firstEnded)
	}
	if turns[1].EndedAt.IsZero() {
		t.Error("turn 2 EndedAt is zero, want it stamped by EndTurn")
	}
	if turns[1].EndedAt.Before(turns[1].StartedAt) {
		t.Error("turn 2 EndedAt precedes its StartedAt")
	}
	// A second EndTurn with no intervening BeginTurn must not re-stamp.
	stamped := turns[1].EndedAt
	time.Sleep(2 * time.Millisecond)
	tree.EndTurn(false)
	if got := tree.Node(tree.RootID()).Snapshot().Turns[1].EndedAt; !got.Equal(stamped) {
		t.Errorf("redundant EndTurn re-stamped EndedAt: %v, want %v", got, stamped)
	}
}

func TestSnapshotTurnsIsDefensiveCopy(t *testing.T) {
	tree := NewAgentTree("root", "m")
	tree.BeginTurn()
	root := tree.Node(tree.RootID())

	view := root.Snapshot()
	if len(view.Turns) != 1 {
		t.Fatalf("len(Turns) = %d, want 1", len(view.Turns))
	}
	want := view.Turns[0]

	view.Turns[0].Turn = 999
	view.Turns[0].StartedAt = time.Unix(0, 0)
	view.Turns[0].EndedAt = time.Unix(1, 0)
	view.Turns = append(view.Turns, TurnMark{Turn: 42})

	again := root.Snapshot()
	if len(again.Turns) != 1 {
		t.Fatalf("len(Turns) after consumer mutation = %d, want 1", len(again.Turns))
	}
	if again.Turns[0] != want {
		t.Errorf("node state mutated through the snapshot: %+v, want %+v", again.Turns[0], want)
	}
}

func TestNodeWithoutBeginTurnHasNoTurnMarks(t *testing.T) {
	tree := NewAgentTree("root", "m")
	if turns := tree.Node(tree.RootID()).Snapshot().Turns; len(turns) != 0 {
		t.Errorf("fresh root Turns = %v, want empty", turns)
	}

	// Child nodes never call BeginTurn — they always take the legacy path.
	child := &AgentNode{ID: newNodeID(), ParentID: tree.RootID(), Label: "child", Depth: 1, Status: StatusRunning}
	tree.addNode(child)
	tree.BeginTurn()
	if turns := child.Snapshot().Turns; len(turns) != 0 {
		t.Errorf("child Turns = %v, want empty", turns)
	}
}

func TestTurnMarksRaceCleanUnderConcurrentSnapshot(t *testing.T) {
	tree := NewAgentTree("root", "m")
	root := tree.Node(tree.RootID())

	var wg sync.WaitGroup
	done := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			tree.BeginTurn()
			tree.EndTurn(i%2 == 0)
		}
		close(done)
	}()

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				v := root.Snapshot()
				for _, tm := range v.Turns {
					_ = tm.Turn
				}
			}
		}()
	}
	wg.Wait()

	turns := root.Snapshot().Turns
	if len(turns) != 200 {
		t.Fatalf("len(Turns) = %d, want 200", len(turns))
	}
	for i, tm := range turns {
		if tm.Turn != i+1 {
			t.Fatalf("Turns[%d].Turn = %d, want %d", i, tm.Turn, i+1)
		}
	}
}

// --- change 0066: the prompt rides on the turn mark -----------------------

// TestBeginTurnWithPromptStoresPromptOnItsOwnMark pins the source of the turn
// group header's preview: the conversational prompt never enters the event
// stream (there is no user-input EventKind), so the mark is the only carrier.
func TestBeginTurnWithPromptStoresPromptOnItsOwnMark(t *testing.T) {
	tree := NewAgentTree("root", "m")

	tree.BeginTurnWithPrompt("first prompt")
	tree.EndTurn(false)
	tree.BeginTurnWithPrompt("second prompt")

	turns := tree.Node(tree.RootID()).Snapshot().Turns
	if len(turns) != 2 {
		t.Fatalf("len(Turns) = %d, want 2", len(turns))
	}
	if turns[0].Prompt != "first prompt" {
		t.Errorf("Turns[0].Prompt = %q, want %q", turns[0].Prompt, "first prompt")
	}
	if turns[1].Prompt != "second prompt" {
		t.Errorf("Turns[1].Prompt = %q, want %q", turns[1].Prompt, "second prompt")
	}
}

// TestBeginTurnWithPromptLeavesEarlierMarksUntouched: opening a new turn must
// not rewrite settled history, the same invariant EndTurn already honors.
func TestBeginTurnWithPromptLeavesEarlierMarksUntouched(t *testing.T) {
	tree := NewAgentTree("root", "m")
	tree.BeginTurnWithPrompt("keep me")
	before := tree.Node(tree.RootID()).Snapshot().Turns[0]

	tree.BeginTurnWithPrompt("a much later prompt")
	tree.BeginTurnWithPrompt("later still")

	after := tree.Node(tree.RootID()).Snapshot().Turns[0]
	if after.Prompt != before.Prompt || after.Turn != before.Turn || !after.StartedAt.Equal(before.StartedAt) {
		t.Errorf("turn 1 mark changed: %+v -> %+v", before, after)
	}
}

// TestBeginTurnYieldsEmptyPrompt: the no-argument wrapper stays valid for the
// callers that have no prompt (research_probe), and records no preview.
func TestBeginTurnYieldsEmptyPrompt(t *testing.T) {
	tree := NewAgentTree("root", "m")
	tree.BeginTurn()

	turns := tree.Node(tree.RootID()).Snapshot().Turns
	if len(turns) != 1 {
		t.Fatalf("len(Turns) = %d, want 1", len(turns))
	}
	if turns[0].Prompt != "" {
		t.Errorf("BeginTurn() Prompt = %q, want empty", turns[0].Prompt)
	}
}

// TestBeginTurnWithPromptStoresRawUnsanitized: sanitization belongs to the
// renderer (learning sanitize-untrusted-bytes-fixed-width-tui) — the model must
// not store a pre-mangled prompt.
func TestBeginTurnWithPromptStoresRawUnsanitized(t *testing.T) {
	raw := "line1\n\x1b[31mred\x1b[0m\ttabbed\r"
	tree := NewAgentTree("root", "m")
	tree.BeginTurnWithPrompt(raw)

	if got := tree.Node(tree.RootID()).Snapshot().Turns[0].Prompt; got != raw {
		t.Errorf("stored Prompt = %q, want the raw bytes %q", got, raw)
	}
}
