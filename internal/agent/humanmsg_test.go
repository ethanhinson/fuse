package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/model"
)

// turnScriptCompleter drives N turns: it returns a tool call on the first turn
// (so the loop continues to a second turn boundary) and a final answer after. It
// records every request so a test can assert a queued human message reached the
// model on the turn after it was enqueued. onTurn fires before each response so a
// test can enqueue a message between turns.
type turnScriptCompleter struct {
	calls    int
	requests []model.CompletionReq
	onTurn   func(turn int)
}

func (c *turnScriptCompleter) Complete(_ context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	c.requests = append(c.requests, req)
	turn := c.calls
	c.calls++
	if c.onTurn != nil {
		c.onTurn(turn)
	}
	if turn == 0 {
		// A tool call keeps the loop alive to a second turn boundary.
		return model.CompletionResp{ToolCalls: []model.ToolCall{{ID: "t1", Name: "noop", Arguments: "{}"}}}, nil
	}
	return model.CompletionResp{Content: "done"}, nil
}

// TestLoopInjectsQueuedMessageAtTurnBoundary proves a message enqueued while the
// agent is mid-run is delivered as a user turn at the NEXT turn boundary — the
// core ADR-0022 delivery guarantee, exercised through the real Run loop.
func TestLoopInjectsQueuedMessageAtTurnBoundary(t *testing.T) {
	bus := NewHumanBus(nil)
	comp := &turnScriptCompleter{}
	comp.onTurn = func(turn int) {
		if turn == 0 {
			// Human types a message during the first turn's tool execution.
			bus.Enqueue("node-x", ModeDirect, "@x", "switch to the streaming approach")
		}
	}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 5, 0)
	a.SetHumanInjector(NewHumanInjector("node-x", bus))

	if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "start"}}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(comp.requests) < 2 {
		t.Fatalf("expected at least 2 turns, got %d", len(comp.requests))
	}
	// The SECOND request must contain the injected human message.
	var found bool
	for _, m := range comp.requests[1].Messages {
		if m.Role == "user" && strings.Contains(m.Content, "switch to the streaming approach") {
			found = true
		}
	}
	if !found {
		t.Error("queued human message was not injected at the turn boundary")
	}
	// And it's marked delivered on the bus.
	if len(bus.Log()) != 1 || bus.Log()[0].Status != MsgDelivered {
		t.Errorf("message should be logged as delivered, got %+v", bus.Log())
	}
}

func TestHumanBus_EnqueueDrainOrder(t *testing.T) {
	bus := NewHumanBus(nil)
	bus.Enqueue("n1", ModeQueued, "@a", "first")
	bus.Enqueue("n1", ModeQueued, "@a", "second")
	bus.Enqueue("n1", ModeQueued, "@a", "third")

	inj := NewHumanInjector("n1", bus)
	msg, ok := inj.Poll()
	if !ok {
		t.Fatal("expected a batched message")
	}
	// One batched user message, segments in Seq order joined by ---.
	if msg.Role != "user" {
		t.Errorf("role = %q, want user", msg.Role)
	}
	for _, want := range []string{"first", "second", "third", "---"} {
		if !strings.Contains(msg.Content, want) {
			t.Errorf("batched content missing %q: %q", want, msg.Content)
		}
	}
	if strings.Index(msg.Content, "first") > strings.Index(msg.Content, "second") {
		t.Error("segments out of Seq order")
	}
	// Queue is drained; a second poll yields nothing.
	if _, ok := inj.Poll(); ok {
		t.Error("queue should be empty after drain")
	}
	// Delivered messages are logged.
	if len(bus.Log()) != 3 {
		t.Errorf("log len = %d, want 3", len(bus.Log()))
	}
}

func TestHumanBus_EditDeleteMove(t *testing.T) {
	bus := NewHumanBus(nil)
	a := bus.Enqueue("n1", ModeQueued, "", "alpha")
	b := bus.Enqueue("n1", ModeQueued, "", "bravo")
	c := bus.Enqueue("n1", ModeQueued, "", "charlie")

	// Edit b.
	if !bus.Edit("n1", b.ID, "BRAVO") {
		t.Fatal("edit failed")
	}
	// Delete a.
	if !bus.Delete("n1", a.ID) {
		t.Fatal("delete failed")
	}
	// Move c to the front.
	if !bus.Move("n1", c.ID, 0) {
		t.Fatal("move failed")
	}
	pend := bus.Pending("n1")
	if len(pend) != 2 {
		t.Fatalf("pending len = %d, want 2", len(pend))
	}
	if pend[0].Text != "charlie" || pend[1].Text != "BRAVO" {
		t.Errorf("order/edit wrong: %q, %q", pend[0].Text, pend[1].Text)
	}
	// Editing a delivered/nonexistent message fails.
	if bus.Edit("n1", "nope", "x") {
		t.Error("edit of unknown id should fail")
	}
}

func TestHumanBus_BroadcastFansOut(t *testing.T) {
	bus := NewHumanBus(nil)
	msgs := bus.Broadcast([]string{"n1", "n2", "n3"}, "pause and review")
	if len(msgs) != 3 {
		t.Fatalf("broadcast created %d, want 3", len(msgs))
	}
	for _, id := range []string{"n1", "n2", "n3"} {
		p := bus.Pending(id)
		if len(p) != 1 || p[0].Text != "pause and review" || p[0].Mode != ModeBroadcast {
			t.Errorf("node %s did not receive the broadcast: %+v", id, p)
		}
	}
}

func TestHumanBus_StrandedBubblesToParent(t *testing.T) {
	// Build a real tree: root -> child.
	tree := NewAgentTree("root", "test")
	rootID := tree.RootID()
	child := &AgentNode{ID: "child-1", ParentID: rootID, Label: "coder", Status: StatusRunning}
	tree.addNode(child)

	bus := NewHumanBus(tree)
	bus.Enqueue("child-1", ModeDirect, "@coder", "use streaming")

	// Child finishes before draining → message must bubble to the parent (root).
	bubbled := bus.OnNodeComplete("child-1")
	if len(bubbled) != 1 || bubbled[0].Status != MsgStranded {
		t.Fatalf("expected 1 stranded message, got %+v", bubbled)
	}
	// It now lives on the parent's queue.
	if p := bus.Pending("child-1"); len(p) != 0 {
		t.Errorf("child queue should be emptied, got %d", len(p))
	}
	if p := bus.Pending(rootID); len(p) != 1 || p[0].Text != "use streaming" {
		t.Errorf("parent should hold the bubbled message, got %+v", p)
	}
}

func TestHumanBus_RootResidueUndeliverable(t *testing.T) {
	tree := NewAgentTree("root", "test")
	rootID := tree.RootID()
	bus := NewHumanBus(tree)
	bus.Enqueue(rootID, ModeQueued, "", "late message")

	residue := bus.OnNodeComplete(rootID)
	if len(residue) != 1 || residue[0].Status != MsgUndeliverable {
		t.Fatalf("root residue should be undeliverable, got %+v", residue)
	}
}

func TestHumanInjector_NilBusNoop(t *testing.T) {
	var inj *HumanInjector
	if _, ok := inj.Poll(); ok {
		t.Error("nil injector should be a no-op")
	}
	inj2 := NewHumanInjector("n", nil)
	if _, ok := inj2.Poll(); ok {
		t.Error("nil-bus injector should be a no-op")
	}
}
