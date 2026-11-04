package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/event"
)

// TestReconstructLoopStateGolden reads golden .jsonl streams from
// testdata/streams/ and asserts that reconstructLoopState produces the
// expected loop state for each case.
func TestReconstructLoopStateGolden(t *testing.T) {
	// Helper: read a .jsonl file and parse events.
	parse := func(path string) ([]event.Event, error) {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		var events []event.Event
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if line == "" {
				continue
			}
			var ev event.Event
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				return nil, fmt.Errorf("parse event in %s: %w\n  line: %s", path, err, line)
			}
			events = append(events, ev)
		}
		return events, nil
	}

	// ---- root_only ----
	{
		evs, err := parse("testdata/streams/root_only.jsonl")
		if err != nil {
			t.Fatal(err)
		}
		state, err := reconstructLoopState(evs, "root")
		if err != nil {
			t.Fatalf("root_only: %v", err)
		}

		if len(state.messages) != 2 {
			t.Fatalf("root_only: expected 2 messages, got %d: %#v", len(state.messages), state.messages)
		}
		if state.messages[0].Role != "user" || state.messages[0].Content != "Hello" {
			t.Fatalf("root_only: msg[0] unexpected: %#v", state.messages[0])
		}
		if state.messages[1].Role != "assistant" || state.messages[1].Content != "Hi there!" {
			t.Fatalf("root_only: msg[1] unexpected: %#v", state.messages[1])
		}
		if len(state.tree.nodes) != 0 {
			t.Fatalf("root_only: expected 0 tree nodes, got %d", len(state.tree.nodes))
		}
		if state.budgets.spawnsUsed != 0 {
			t.Fatalf("root_only: expected spawnsUsed=0, got %d", state.budgets.spawnsUsed)
		}
		if state.budgets.sessionTokens != 30 {
			t.Fatalf("root_only: expected sessionTokens=30 (10+20), got %d", state.budgets.sessionTokens)
		}
		if len(state.summaries) != 0 {
			t.Fatalf("root_only: expected 0 summaries, got %d", len(state.summaries))
		}
	}

	// ---- root_child_completed ----
	{
		evs, err := parse("testdata/streams/root_child_completed.jsonl")
		if err != nil {
			t.Fatal(err)
		}
		state, err := reconstructLoopState(evs, "root")
		if err != nil {
			t.Fatalf("root_child_completed: %v", err)
		}

		// Root transcript: user, assistant (with tool call), tool result. NO child model.call.end.
		if len(state.messages) != 3 {
			t.Fatalf("root_child_completed: expected 3 messages, got %d: %#v", len(state.messages), state.messages)
		}
		if state.messages[0].Role != "user" || state.messages[0].Content != "Hello" {
			t.Fatalf("root_child_completed: msg[0] unexpected: %#v", state.messages[0])
		}
		if state.messages[1].Role != "assistant" {
			t.Fatalf("root_child_completed: msg[1] role %q, want assistant", state.messages[1].Role)
		}
		if state.messages[2].Role != "tool" {
			t.Fatalf("root_child_completed: msg[2] role %q, want tool", state.messages[2].Role)
		}

		// Tree: 1 node, terminal=true, not orphaned.
		if len(state.tree.nodes) != 1 {
			t.Fatalf("root_child_completed: expected 1 tree node, got %d", len(state.tree.nodes))
		}
		if state.tree.nodes[0].id != "child1" {
			t.Fatalf("root_child_completed: node id %q, want child1", state.tree.nodes[0].id)
		}
		if !state.tree.nodes[0].terminal {
			t.Fatalf("root_child_completed: node should be terminal")
		}
		if state.tree.nodes[0].orphaned {
			t.Fatalf("root_child_completed: node should NOT be orphaned")
		}

		if state.budgets.spawnsUsed != 1 {
			t.Fatalf("root_child_completed: expected spawnsUsed=1, got %d", state.budgets.spawnsUsed)
		}
		// Session tokens: root model.call.end (10+20=30) + child model.call.end (5+10=15) = 45
		if state.budgets.sessionTokens != 45 {
			t.Fatalf("root_child_completed: expected sessionTokens=45, got %d", state.budgets.sessionTokens)
		}
		// Pool tokens: pool "default" should have child1's tokens (15)
		if state.budgets.poolTokens["default"] != 15 {
			t.Fatalf("root_child_completed: expected poolTokens[default]=15, got %d", state.budgets.poolTokens["default"])
		}
	}

	// ---- root_orphan_child ----
	{
		evs, err := parse("testdata/streams/root_orphan_child.jsonl")
		if err != nil {
			t.Fatal(err)
		}
		state, err := reconstructLoopState(evs, "root")
		if err != nil {
			t.Fatalf("root_orphan_child: %v", err)
		}

		// Root transcript: user, assistant (with tool call). NO tool result (orphaned).
		if len(state.messages) != 2 {
			t.Fatalf("root_orphan_child: expected 2 messages, got %d: %#v", len(state.messages), state.messages)
		}
		if state.messages[0].Role != "user" {
			t.Fatalf("root_orphan_child: msg[0] role %q, want user", state.messages[0].Role)
		}
		if state.messages[1].Role != "assistant" {
			t.Fatalf("root_orphan_child: msg[1] role %q, want assistant", state.messages[1].Role)
		}

		// Tree: 1 node, orphaned=true.
		if len(state.tree.nodes) != 1 {
			t.Fatalf("root_orphan_child: expected 1 tree node, got %d", len(state.tree.nodes))
		}
		if !state.tree.nodes[0].orphaned {
			t.Fatalf("root_orphan_child: node should be orphaned")
		}
		if state.tree.nodes[0].terminal {
			t.Fatalf("root_orphan_child: node should NOT be terminal")
		}

		if state.budgets.spawnsUsed != 1 {
			t.Fatalf("root_orphan_child: expected spawnsUsed=1, got %d", state.budgets.spawnsUsed)
		}
		// Session tokens: root model.call.end (10+20=30) + child model.call.end (5+10=15) = 45
		if state.budgets.sessionTokens != 45 {
			t.Fatalf("root_orphan_child: expected sessionTokens=45, got %d", state.budgets.sessionTokens)
		}
		// Pool tokens: pool "default" should have child1's tokens (15)
		if state.budgets.poolTokens["default"] != 15 {
			t.Fatalf("root_orphan_child: expected poolTokens[default]=15, got %d", state.budgets.poolTokens["default"])
		}
	}

	// ---- summarized_then_resumed ----
	{
		evs, err := parse("testdata/streams/summarized_then_resumed.jsonl")
		if err != nil {
			t.Fatal(err)
		}
		state, err := reconstructLoopState(evs, "root")
		if err != nil {
			t.Fatalf("summarized_then_resumed: %v", err)
		}

		// Should have 1 summary anchor.
		if len(state.summaries) != 1 {
			t.Fatalf("summarized_then_resumed: expected 1 summary, got %d", len(state.summaries))
		}
		if state.summaries[0].summary != "Summary text" {
			t.Fatalf("summarized_then_resumed: summary text %q, want 'Summary text'", state.summaries[0].summary)
		}
		if state.summaries[0].insertAt != 2 {
			t.Fatalf("summarized_then_resumed: insertAt %d, want 2", state.summaries[0].insertAt)
		}

		// Also verify transcript is present (contains root events).
		if len(state.messages) != 4 {
			t.Fatalf("summarized_then_resumed: expected 4 messages, got %d: %#v", len(state.messages), state.messages)
		}
		if state.messages[0].Role != "user" || state.messages[0].Content != "Tell me a story" {
			t.Fatalf("summarized_then_resumed: msg[0] unexpected: %#v", state.messages[0])
		}
		if state.messages[1].Role != "assistant" || state.messages[1].Content != "Once upon a time..." {
			t.Fatalf("summarized_then_resumed: msg[1] unexpected: %#v", state.messages[1])
		}
		if state.messages[2].Role != "user" || state.messages[2].Content != "Continue the story" {
			t.Fatalf("summarized_then_resumed: msg[2] unexpected: %#v", state.messages[2])
		}
		if state.messages[3].Role != "assistant" || state.messages[3].Content != "The story continues..." {
			t.Fatalf("summarized_then_resumed: msg[3] unexpected: %#v", state.messages[3])
		}
	}
}