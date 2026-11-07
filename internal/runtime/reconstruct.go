package runtime

import (
	"encoding/json"
	"fmt"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
)

// ---------------------------------------------------------------------------
// Types for reconstructLoopState
// ---------------------------------------------------------------------------

// loopState is the full reconstructed state of a loop from its durable event
// stream: the model-facing transcript, a tree of spawned child nodes, budgets
// (spawn/token counts), and summaries.
type loopState struct {
	messages  []model.Message
	tree      treeView
	budgets   budgets
	summaries []summaryAnchor
}

type treeView struct {
	nodes []treeNode
}

type treeNode struct {
	id, parent, label, role string
	depth                   int
	terminal                bool   // has matching spawn.done
	orphaned                bool   // spawn.start without spawn.done
}

type budgets struct {
	spawnsUsed    int
	sessionTokens int
	poolTokens    map[string]int // pool ID -> total tokens
}

type summaryAnchor struct {
	summary  string
	insertAt int
}

// ---------------------------------------------------------------------------
// reconstructLoopState — full loop state rebuild from a durable event stream
// ---------------------------------------------------------------------------
//
// reconstructLoopState folds a durable event slice back into the full loop
// state the agent loop needs to resume: the model-facing transcript, the spawn
// tree, budget counters, and summaries (change 0127).
//
// The transcript fold is the same logic as the original reconstructMessages
// (user.input → user message, model.call.end → assistant message, tool.call
// backfills arguments, tool.result → tool message). NodeID/Depth filtering
// (3a) skips child-node events when rootID is non-empty, so only root events
// contribute to the transcript.
//
// The tree view (3b) is built from spawn.start and spawn.done events: every
// spawn.start produces a treeNode, and the matching spawn.done marks it
// terminal. Any spawn.start without a matching spawn.done is orphaned.
//
// Budgets (3c) count spawn.start events, accumulate model token usage across
// all nodes, and track per-pool token sums.
//
// Summaries (3d) collect context.summarize events with non-empty Summary into
// summaryAnchor records.
func reconstructLoopState(events []event.Event, rootID string) (loopState, error) {
	var state loopState
	state.tree = treeView{nodes: []treeNode{}}
	state.budgets = budgets{spawnsUsed: 0, sessionTokens: 0, poolTokens: map[string]int{}}
	state.summaries = []summaryAnchor{}

	// pending holds the assistant message for the current turn while its tool-call
	// Arguments are still arriving (tool.call events follow model.call.end). It is
	// flushed before the next user/assistant/tool message, or at the end.
	var pending *model.Message
	// callIdx maps a tool-call ID to its slot in pending.ToolCalls so a tool.call
	// event fills the Arguments the model.call.end ref left empty.
	callIdx := map[string]int{}

	// Per-node token totals for pool attribution (3c).
	nodeTokens := map[string]int{}

	flush := func() {
		if pending != nil {
			state.messages = append(state.messages, *pending)
			pending = nil
			callIdx = map[string]int{}
		}
	}

	// isRootEvent returns true when the event should be included in the transcript
	// fold. When rootID is "", all events pass (backward-compatible). When rootID is
	// non-empty, only events whose NodeID is empty (older streams) or equals rootID.
	isRootEvent := func(e event.Event) bool {
		return rootID == "" || e.NodeID == "" || e.NodeID == rootID
	}

	for _, e := range events {
		// --- 3a: Transcript fold — root events only ---
		if isRootEvent(e) {
			switch e.Kind {
			case event.KindUserInput:
				flush()
				var p event.UserInputPayload
				if err := json.Unmarshal(e.Payload, &p); err != nil {
					return loopState{}, fmt.Errorf("reconstruct: user.input payload (seq %d): %w", e.Seq, err)
				}
				state.messages = append(state.messages, model.Message{Role: "user", Content: p.Content})

			case event.KindModelCallEnd:
				flush()
				var p event.ModelCallEndPayload
				if err := json.Unmarshal(e.Payload, &p); err != nil {
					return loopState{}, fmt.Errorf("reconstruct: model.call.end payload (seq %d): %w", e.Seq, err)
				}
				// A discarded reply never entered the live transcript. Streams from
				// before that marker existed can still hold an empty cut-off reply,
				// which is equally not a turn.
				if p.Discarded || (p.FinishReason == "length" && p.Content == "" && len(p.ToolCalls) == 0) {
					break // leaves this switch only: its tokens are still counted below
				}
				msg := model.Message{Role: "assistant", Content: p.Content}
				for _, ref := range p.ToolCalls {
					callIdx[ref.ID] = len(msg.ToolCalls)
					msg.ToolCalls = append(msg.ToolCalls, model.ToolCall{ID: ref.ID, Name: ref.Name})
				}
				pending = &msg

			case event.KindToolCall:
				var p event.ToolCallPayload
				if err := json.Unmarshal(e.Payload, &p); err != nil {
					return loopState{}, fmt.Errorf("reconstruct: tool.call payload (seq %d): %w", e.Seq, err)
				}
				if pending != nil {
					if i, ok := callIdx[p.ID]; ok {
						pending.ToolCalls[i].Arguments = string(p.Args)
					}
				}

			case event.KindToolResult:
				flush()
				var p event.ToolResultPayload
				if err := json.Unmarshal(e.Payload, &p); err != nil {
					return loopState{}, fmt.Errorf("reconstruct: tool.result payload (seq %d): %w", e.Seq, err)
				}
				state.messages = append(state.messages, model.Message{
					Role:       "tool",
					ToolCallID: p.ID,
					Name:       p.Name,
					Content:    p.Result,
				})

			default:
				// Other kinds are not transcript content; skip.
			}
		}

		// --- 3b/3c/3d: Non-transcript processing (ALL events, regardless of NodeID) ---
		switch e.Kind {
		case event.KindSpawnStart:
			state.budgets.spawnsUsed++
			var p event.SpawnStartPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return loopState{}, fmt.Errorf("reconstruct: spawn.start payload (seq %d): %w", e.Seq, err)
			}
			state.tree.nodes = append(state.tree.nodes, treeNode{
				id:       p.ChildNodeID,
				parent:   e.ParentID,
				label:    p.Label,
				depth:    e.Depth,
				role:     p.Role,
				terminal: false,
				orphaned: false,
			})

		case event.KindSpawnDone:
			var p event.SpawnDonePayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return loopState{}, fmt.Errorf("reconstruct: spawn.done payload (seq %d): %w", e.Seq, err)
			}
			// Mark the matching node terminal.
			for i := 0; i < len(state.tree.nodes); i++ {
				if state.tree.nodes[i].id == p.ChildNodeID {
					state.tree.nodes[i].terminal = true
					break
				}
			}

		case event.KindModelCallEnd:
			var p event.ModelCallEndPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return loopState{}, fmt.Errorf("reconstruct: model.call.end payload (seq %d): %w", e.Seq, err)
			}
			state.budgets.sessionTokens += p.InputTokens + p.OutputTokens
			nodeTokens[e.NodeID] = nodeTokens[e.NodeID] + p.InputTokens + p.OutputTokens

		case event.KindSummarize:
			var p event.SummarizePayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return loopState{}, fmt.Errorf("reconstruct: summarize payload (seq %d): %w", e.Seq, err)
			}
			if p.Summary != "" {
				state.summaries = append(state.summaries, summaryAnchor{
					summary:  p.Summary,
					insertAt: p.InsertAt,
				})
			}

		default:
			// Other kinds: skip.
		}
	}
	flush()

	// --- Post-processing ---

	// 3b — Mark orphaned nodes: any treeNode that is not terminal is orphaned.
	for i := 0; i < len(state.tree.nodes); i++ {
		if !state.tree.nodes[i].terminal {
			state.tree.nodes[i].orphaned = true
		}
	}

	// 3c — Pool token resolution: attribute each child's token total to its pool.
	for _, e := range events {
		if e.Kind == event.KindSpawnStart {
			var p event.SpawnStartPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return loopState{}, fmt.Errorf("reconstruct: spawn.start payload (seq %d): %w", e.Seq, err)
			}
			if p.Pool != "" {
				state.budgets.poolTokens[p.Pool] = state.budgets.poolTokens[p.Pool] + nodeTokens[p.ChildNodeID]
			}
		}
	}

	return state, nil
}

// ---------------------------------------------------------------------------
// reconstructMessages — backward-compatible wrapper
// ---------------------------------------------------------------------------
//
// reconstructMessages folds events into just the model-facing transcript,
// with NO NodeID filtering (rootID=""). This preserves the original behaviour
// for existing callers and tests that do not pass a rootID.
func reconstructMessages(events []event.Event) ([]model.Message, error) {
	state, err := reconstructLoopState(events, "")
	if err != nil {
		return nil, err
	}
	return state.messages, nil
}
