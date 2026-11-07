package model

import "testing"

func TestCompletionRespTruncated(t *testing.T) {
	call := []ToolCall{{ID: "1"}}
	for _, tc := range []struct {
		name  string
		resp  CompletionResp
		limit int
		want  bool
	}{
		{"openai length", CompletionResp{FinishReason: "length"}, 100, true},
		{"anthropic max_tokens", CompletionResp{FinishReason: "max_tokens"}, 100, true},
		{"length with no cap sent", CompletionResp{FinishReason: "length"}, 0, true},
		{"clean stop", CompletionResp{FinishReason: "stop", OutputTokens: 100}, 100, false},
		{"tool calls", CompletionResp{FinishReason: "tool_calls", OutputTokens: 100}, 100, false},
		{"unreported, cap spent, nothing back", CompletionResp{OutputTokens: 100}, 100, true},
		{"unreported, under the cap", CompletionResp{OutputTokens: 99}, 100, false},
		// A subprocess-backed model ignores the cap and can report more.
		{"unreported, over the cap with content", CompletionResp{OutputTokens: 500, Content: "x"}, 100, false},
		{"unreported, over the cap with calls", CompletionResp{OutputTokens: 500, ToolCalls: call}, 100, false},
		{"unreported, no cap", CompletionResp{}, 0, false},
	} {
		if got := tc.resp.Truncated(tc.limit); got != tc.want {
			t.Errorf("%s: Truncated = %v, want %v", tc.name, got, tc.want)
		}
	}
}
