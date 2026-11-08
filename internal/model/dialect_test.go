package model

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestDetectDialect(t *testing.T) {
	for id, want := range map[string]string{
		"cloud/claude-haiku-5.5":               DialectAnthropic,
		"openrouter/anthropic/claude-sonnet-5": DialectAnthropic,
		"cloud/opus-5.5":                       DialectAnthropic,
		"claude/sonnet":                        DialectAnthropic,
		"vision/claude-haiku-4.5":              DialectAnthropic,
		"cloud/deepseek-v4-flash":              DialectOpenAI,
		"openrouter/moonshotai/kimi-k3":        DialectOpenAI,
		"local/qwen3-coder":                    DialectOpenAI,
		"":                                     DialectOpenAI,
	} {
		if got := DetectDialect(id); got != want {
			t.Errorf("DetectDialect(%q) = %q, want %q", id, got, want)
		}
	}
}

// agentHistory is a short tool-call loop: system, task, two tool rounds (one
// with an empty result), and a trailing assistant note.
func agentHistory() []Message {
	return []Message{
		{Role: "system", Content: "be useful"},
		{Role: "user", Content: "fix the bug"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "a", Name: "bash", Arguments: `{"command":"ls"}`}}},
		{Role: "tool", ToolCallID: "a", Name: "bash", Content: "main.go"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "b", Name: "bash", Arguments: `{"command":"true"}`}}},
		{Role: "tool", ToolCallID: "b", Name: "bash", Content: ""},
		{Role: "user", Content: "keep going"},
	}
}

func sent(t *testing.T, req CompletionReq) []any {
	t.Helper()
	var bodies []map[string]any
	srv := fixedServer(t, &bodies, true, sse(`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`))
	if _, err := NewAdapter(srv.URL, "k", nil).Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	return bodies[0]["messages"].([]any)
}

// marked returns the indexes of messages sent as a cache-marked content part,
// and fails on any other non-string content.
func marked(t *testing.T, msgs []any) []int {
	t.Helper()
	var out []int
	for i, raw := range msgs {
		m := raw.(map[string]any)
		switch c := m["content"].(type) {
		case string:
		case []any:
			part := c[0].(map[string]any)
			if len(c) != 1 || part["type"] != "text" || part["cache_control"].(map[string]any)["type"] != "ephemeral" {
				t.Errorf("message %d content %v", i, c)
			}
			out = append(out, i)
		default:
			t.Errorf("message %d content %T", i, c)
		}
	}
	return out
}

// Claude models get cache breakpoints on the system prompt and the last two
// non-empty user or tool messages; assistant turns and empty text never.
func TestAnthropicDialectMarksCacheBreakpoints(t *testing.T) {
	msgs := sent(t, CompletionReq{Model: "cloud/claude-haiku-5.5", Messages: agentHistory()})
	got := marked(t, msgs)
	if want := []int{0, 3, 6}; len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("marked %v, want %v", got, want)
	}
	if text := msgs[3].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]; text != "main.go" {
		t.Errorf("marked text = %v", text)
	}
	tool := msgs[3].(map[string]any)
	if tool["tool_call_id"] != "a" || tool["role"] != "tool" {
		t.Errorf("other fields lost: %v", tool)
	}
	if msgs[5].(map[string]any)["content"] != "" {
		t.Errorf("empty tool result must stay an empty string: %v", msgs[5])
	}
}

func TestOpenAIDialectSendsPlainContent(t *testing.T) {
	if got := marked(t, sent(t, CompletionReq{Model: "cloud/deepseek-v4-flash", Messages: agentHistory()})); len(got) != 0 {
		t.Fatalf("marked %v", got)
	}
}

// The configured dialect wins over detection, both ways.
func TestConfiguredDialectOverridesDetection(t *testing.T) {
	if got := marked(t, sent(t, CompletionReq{Model: "cloud/claude-haiku-5.5", Dialect: DialectOpenAI, Messages: agentHistory()})); len(got) != 0 {
		t.Errorf("openai on a claude id: marked %v", got)
	}
	if got := marked(t, sent(t, CompletionReq{Model: "my-proxy/alias", Dialect: DialectAnthropic, Messages: agentHistory()})); len(got) != 3 {
		t.Errorf("anthropic on an opaque alias: marked %v", got)
	}
}

// Cache writes are read from the usage and kept in the trace, so traces can be
// priced (writes cost more than plain input).
func TestCacheWriteTokensAreReadAndTraced(t *testing.T) {
	var bodies []map[string]any
	srv := fixedServer(t, &bodies, true, sse(
		`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":5000,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":3000,"cache_write_tokens":1900}}}`,
	))
	var trace bytes.Buffer
	resp, err := NewAdapter(srv.URL, "k", nil).WithTrace(&trace).Complete(context.Background(), CompletionReq{Model: "cloud/claude-haiku-5.5", Messages: agentHistory()})
	if err != nil {
		t.Fatal(err)
	}
	if resp.CachedTokens != 3000 || resp.CacheWrite != 1900 {
		t.Errorf("cached %d write %d", resp.CachedTokens, resp.CacheWrite)
	}
	if !strings.Contains(trace.String(), `"cache_write_tokens": 1900`) {
		t.Errorf("trace lacks cache writes")
	}
}
