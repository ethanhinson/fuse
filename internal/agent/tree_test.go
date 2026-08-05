package agent

import (
	"sync"
	"testing"
	"time"
)

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
