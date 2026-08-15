package runtime

import (
	"encoding/json"
	"fmt"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
)

// reconstructMessages folds a durable event slice back into the model-facing
// []model.Message transcript (change 0054, D5). It is the correctness core of
// resume: a loop whose in-memory transcript was discarded on disconnect is rebuilt
// purely from its durable #47 event stream — the single source of truth (D1) — so a
// re-parked agent continues with the same history the live loop held.
//
// The mapping is pinned by the spec's event-kind → message-role table; every other
// kind (turn boundaries, model.call.start, park/keepalive/summarize/gap markers) is
// model-internal and explicitly SKIPPED. Events must arrive in Seq order (Replay's
// contract), which is also transcript order, so the fold is a single forward pass:
//
//   - KindUserInput   → {Role:"user", Content}. User turns are otherwise absent from
//     the stream; change 0054 added this kind precisely so the fold is lossless.
//   - KindModelCallEnd → {Role:"assistant", Content, ToolCalls}. The payload's
//     ToolCalls are refs (ID+Name only); the raw Arguments live in the following
//     KindToolCall events of the SAME turn, so the assistant message is held pending
//     and its tool-call Arguments are filled in as those events arrive, then flushed.
//   - KindToolCall    → fills Arguments (by call ID) into the pending assistant msg.
//   - KindToolResult  → {Role:"tool", ToolCallID, Name, Content}, flushing any pending
//     assistant message first (the assistant turn precedes its tool results).
//
// The result is byte-equal to the live in-memory transcript (guarded by the D6
// round-trip test), because each reconstructed message is built to the exact shape
// the loop appends: CompletionResp.AsMessage for assistant turns, the executeTools
// tool-message shape for results, and the injector's user message for input.
func reconstructMessages(events []event.Event) ([]model.Message, error) {
	var out []model.Message

	// pending holds the assistant message for the current turn while its tool-call
	// Arguments are still arriving (tool.call events follow model.call.end). It is
	// flushed before the next user/assistant/tool message, or at the end.
	var pending *model.Message
	// callIdx maps a tool-call ID to its slot in pending.ToolCalls so a tool.call
	// event fills the Arguments the model.call.end ref left empty.
	callIdx := map[string]int{}

	flush := func() {
		if pending != nil {
			out = append(out, *pending)
			pending = nil
			callIdx = map[string]int{}
		}
	}

	for _, e := range events {
		switch e.Kind {
		case event.KindUserInput:
			flush()
			var p event.UserInputPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, fmt.Errorf("reconstruct: user.input payload (seq %d): %w", e.Seq, err)
			}
			out = append(out, model.Message{Role: "user", Content: p.Content})

		case event.KindModelCallEnd:
			flush()
			var p event.ModelCallEndPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, fmt.Errorf("reconstruct: model.call.end payload (seq %d): %w", e.Seq, err)
			}
			msg := model.Message{Role: "assistant", Content: p.Content}
			for _, ref := range p.ToolCalls {
				callIdx[ref.ID] = len(msg.ToolCalls)
				// Arguments are backfilled from the matching tool.call event below.
				msg.ToolCalls = append(msg.ToolCalls, model.ToolCall{ID: ref.ID, Name: ref.Name})
			}
			pending = &msg

		case event.KindToolCall:
			var p event.ToolCallPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, fmt.Errorf("reconstruct: tool.call payload (seq %d): %w", e.Seq, err)
			}
			// Backfill the raw Arguments into the pending assistant message's matching
			// tool call. If there is no pending assistant (malformed/partial stream), the
			// call is dropped rather than fabricating an orphan.
			if pending != nil {
				if i, ok := callIdx[p.ID]; ok {
					pending.ToolCalls[i].Arguments = string(p.Args)
				}
			}

		case event.KindToolResult:
			flush()
			var p event.ToolResultPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, fmt.Errorf("reconstruct: tool.result payload (seq %d): %w", e.Seq, err)
			}
			out = append(out, model.Message{
				Role:       "tool",
				ToolCallID: p.ID,
				Name:       p.Name,
				Content:    p.Result,
			})

		default:
			// turn.start / turn.end / model.call.start / model.delta / spawn.* /
			// context.summarize / loop.detector.trip / error / loop.parked / user
			// keepalive-style markers: not model-facing transcript content. Skipped.
		}
	}
	flush()
	return out, nil
}
