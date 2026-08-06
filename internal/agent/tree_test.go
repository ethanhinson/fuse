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
	tr := NewAgentTreeWithConcurrency("root", "m", 3)
	if cap(tr.spawnSem) != 3 {
		t.Fatalf("spawnSem cap = %d, want 3", cap(tr.spawnSem))
	}
}

func TestNewAgentTreeWithConcurrencyFallsBackOnZero(t *testing.T) {
	tr := NewAgentTreeWithConcurrency("root", "m", 0)
	if cap(tr.spawnSem) != MaxConcurrentSpawns {
		t.Fatalf("spawnSem cap = %d, want %d", cap(tr.spawnSem), MaxConcurrentSpawns)
	}
}

func TestNewAgentTreeUsesDefaultConcurrency(t *testing.T) {
	tr := NewAgentTree("root", "m")
	if cap(tr.spawnSem) != MaxConcurrentSpawns {
		t.Fatalf("spawnSem cap = %d, want %d", cap(tr.spawnSem), MaxConcurrentSpawns)
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
