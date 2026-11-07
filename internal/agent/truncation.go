package agent

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/ethanhinson/fuse/internal/model"
)

// ErrOutputTruncated ends a run whose model could not finish a reply inside the
// highest output cap it is allowed. It is an error, never a clean finish: a
// reply cut at max_tokens says nothing about whether the work is done.
var ErrOutputTruncated = errors.New("agent: model reply truncated at the output cap")

const (
	// defaultOutputCeilingFactor is how far a truncated reply may raise the
	// model's max_tokens when it configures no max_tokens_ceiling. The cap covers
	// reasoning as well as the visible reply, so a reasoning model can spend all
	// of it thinking and produce nothing. Doubling from the configured value gives
	// two retries before the run fails.
	defaultOutputCeilingFactor = 4
	// outputCapFloor is the smallest cap the context clamp will send, so a nearly
	// full window still gets a usable reply or a provider length error, which the
	// loop already recovers from, rather than a guaranteed truncation.
	outputCapFloor = 4096
)

// outputCeiling is the highest cap a retry may request: max_tokens_ceiling when
// set, otherwise defaultOutputCeilingFactor times max_tokens. A max_tokens of 0
// sends no cap, so the provider's own limit applies and there is nothing to raise.
func (a *Agent) outputCeiling() int {
	if a.maxTokens <= 0 {
		return 0
	}
	if a.outputCeilingTokens > 0 {
		return a.outputCeilingTokens
	}
	return a.maxTokens * defaultOutputCeilingFactor
}

// clampOutputCap keeps prompt + cap inside the context window, because some
// providers reject the request otherwise. It never lowers the cap below
// outputCapFloor, and a cap of 0 stays unset.
func clampOutputCap(limit, window, estimate int) int {
	if limit <= 0 {
		return limit
	}
	room := window - estimate
	if room < outputCapFloor {
		room = outputCapFloor
	}
	if limit > room {
		return room
	}
	return limit
}

// nextOutputCap doubles the cap for a retry, bounded by the ceiling and the
// context window. A result equal to current means no room is left to raise it.
func nextOutputCap(current, ceiling, window, estimate int) int {
	if current <= 0 {
		return current
	}
	next := current * 2
	if next > ceiling {
		next = ceiling
	}
	next = clampOutputCap(next, window, estimate)
	if next < current {
		return current
	}
	return next
}

// replyUnusable reports whether a truncated reply cannot be acted on: it has no
// tool call to run, or a tool call was cut mid-arguments. A truncated reply whose
// tool calls are all complete JSON is usable, because the model finished deciding
// what to do.
func replyUnusable(resp model.CompletionResp, limit int) bool {
	if !resp.Truncated(limit) {
		return false
	}
	if len(resp.ToolCalls) == 0 {
		return true
	}
	for _, tc := range resp.ToolCalls {
		if !json.Valid([]byte(tc.Arguments)) {
			return true
		}
	}
	return false
}

// isOutputCapErr reports a provider refusing the requested max_tokens itself, as
// opposed to the prompt being too long.
func isOutputCapErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, pat := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens", "maximum output"} {
		if strings.Contains(s, pat) {
			return true
		}
	}
	return false
}

const (
	// prunedArgMinBytes leaves small tool-call arguments alone: clearing them
	// frees little and they are cheap context about what the agent did.
	prunedArgMinBytes = 512
	// prunedArgFieldBytes is the largest argument value kept when pruning, so
	// identifying fields such as a path or a short command survive.
	prunedArgFieldBytes = 200
	// prunedArgsMarker records what was removed, in the arguments themselves.
	prunedArgsMarker = "_cleared_bytes_to_free_context"
)

// pruneOldToolArgs shrinks the arguments of tool calls whose results were already
// stubbed and returns the estimated tokens freed. A write_file body sits in the
// assistant message for the rest of the session otherwise, and repeated writes
// can fill the window with content the agent can re-read from disk. Arguments stay
// valid JSON objects, so provider tool pairing and validation still hold; anything
// that is not a JSON object is left untouched.
func pruneOldToolArgs(messages []model.Message) int {
	stubbed := make(map[string]bool)
	for _, m := range messages {
		if m.Role == "tool" && m.Content == prunedStub && m.ToolCallID != "" {
			stubbed[m.ToolCallID] = true
		}
	}
	if len(stubbed) == 0 {
		return 0
	}
	freed := 0
	for i := range messages {
		if messages[i].Role != "assistant" {
			continue
		}
		var calls []model.ToolCall
		for j, tc := range messages[i].ToolCalls {
			if !stubbed[tc.ID] {
				continue
			}
			trimmed, ok := trimToolArgs(tc.Arguments)
			if !ok {
				continue
			}
			// Copy on first write: the slice may be shared with the caller's history.
			if calls == nil {
				calls = append([]model.ToolCall(nil), messages[i].ToolCalls...)
			}
			freed += (len(tc.Arguments) - len(trimmed)) / bytesPerToken
			calls[j].Arguments = trimmed
		}
		if calls != nil {
			messages[i].ToolCalls = calls
		}
	}
	return freed
}

// trimToolArgs drops oversized values from a JSON-object argument string,
// reporting false when there is nothing worth removing.
func trimToolArgs(args string) (string, bool) {
	if len(args) < prunedArgMinBytes {
		return "", false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(args), &fields); err != nil || fields == nil {
		return "", false
	}
	removed := 0
	for k, v := range fields {
		if len(v) > prunedArgFieldBytes {
			removed += len(v)
			delete(fields, k)
		}
	}
	if removed == 0 {
		return "", false
	}
	count, err := json.Marshal(removed)
	if err != nil {
		return "", false
	}
	fields[prunedArgsMarker] = count
	out, err := json.Marshal(fields)
	if err != nil || len(out) >= len(args) {
		return "", false
	}
	return string(out), true
}
