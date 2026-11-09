package model

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func boolp(b bool) *bool { return &b }

// withTestDialect prepends d to dialects for one test (so it wins detection and
// is findable by name). Tests using it must not run in parallel.
func withTestDialect(t *testing.T, d *dialect) {
	t.Helper()
	old := dialects
	dialects = append([]*dialect{d}, old...)
	t.Cleanup(func() { dialects = old })
}

func sentBody(t *testing.T, req CompletionReq, replies ...string) ([]map[string]any, string) {
	t.Helper()
	srv, raw := rawServer(t, replies...)
	var trace bytes.Buffer
	a := NewAdapter(srv.URL, "k", nil).WithTrace(&trace)
	a.MaxAttempts = 1
	if _, err := a.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, b := range *raw {
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out, trace.String()
}

func TestDialectWire_OpenAICacheMarkersOption(t *testing.T) {
	opt := sent(t, CompletionReq{Model: "cloud/deepseek-v4-flash", DialectOptions: DialectOptions{CacheMarkers: boolp(true)}, Messages: agentHistory()})
	if got := marked(t, opt); !slices.Equal(got, []int{0, 3, 6}) {
		t.Fatalf("marked %v, want [0 3 6]", got)
	}
	anth := sent(t, CompletionReq{Model: "cloud/deepseek-v4-flash", Dialect: DialectAnthropic, Messages: agentHistory()})
	if !reflect.DeepEqual(opt, anth) {
		t.Errorf("wire differs from the anthropic dialect:\n%v\n%v", opt, anth)
	}
}

func TestDialectWire_AnthropicCacheMarkersOff(t *testing.T) {
	got := marked(t, sent(t, CompletionReq{Model: "cloud/claude-haiku-5.5", DialectOptions: DialectOptions{CacheMarkers: boolp(false)}, Messages: agentHistory()}))
	if len(got) != 0 {
		t.Fatalf("marked %v", got)
	}
}

func reasoningFields(m map[string]any) []string {
	var out []string
	for _, f := range ReasoningFields() {
		if _, ok := m[f]; ok {
			out = append(out, f)
		}
	}
	return out
}

func TestDialectWire_ReasoningFieldOption(t *testing.T) {
	hist := []Message{
		{Role: "user", Content: "x"},
		{Role: "assistant", Content: "a", Reasoning: "thought", ReasoningField: "reasoning"},
		{Role: "user", Content: "y"},
	}
	cases := []struct {
		name   string
		replay bool
		opt    string
		want   []string
	}{
		{"option replaces arrived field", true, "reasoning_content", []string{"reasoning_content"}},
		{"replay off sends nothing", false, "reasoning_content", nil},
		{"unset keeps arrived field", true, "", []string{"reasoning"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msgs := sent(t, CompletionReq{Model: "cloud/deepseek-v4-flash", ReplayReasoning: c.replay,
				DialectOptions: DialectOptions{ReasoningField: c.opt}, Messages: hist})
			got := reasoningFields(msgs[1].(map[string]any))
			if !slices.Equal(got, c.want) {
				t.Errorf("reasoning fields = %v, want %v", got, c.want)
			}
		})
	}
}

func TestDialectWire_InvalidReasoningFieldIgnored(t *testing.T) {
	hist := []Message{
		{Role: "user", Content: "x"},
		{Role: "assistant", Content: "a", Reasoning: "thought", ReasoningField: "reasoning_text"},
	}
	msgs := sent(t, CompletionReq{Model: "cloud/deepseek-v4-flash", ReplayReasoning: true,
		DialectOptions: DialectOptions{ReasoningField: "bogus"}, Messages: hist})
	if got := reasoningFields(msgs[1].(map[string]any)); !slices.Equal(got, []string{"reasoning_text"}) {
		t.Errorf("reasoning fields = %v, want the arrived field", got)
	}
}

func TestRecoverToolCallsOptionOff(t *testing.T) {
	content := "Reading.\n\n" + xmlCall
	replies := map[string]string{
		"streamed": sse(chunk(`{"content":`+jsonStr(content)+`}`, "stop")),
		"buffered": `{"choices":[{"message":{"role":"assistant","content":` + jsonStr(content) + `},"finish_reason":"stop"}]}`,
	}
	for name, reply := range replies {
		t.Run(name, func(t *testing.T) {
			srv, _ := rawServer(t, reply)
			var trace bytes.Buffer
			resp, err := NewAdapter(srv.URL, "k", nil).WithTrace(&trace).Complete(context.Background(), CompletionReq{
				Model: "cloud/deepseek-v4-flash", Tools: equivTools(), Messages: agentHistory(),
				DialectOptions: DialectOptions{RecoverToolCalls: boolp(false)},
			})
			if err != nil {
				t.Fatal(err)
			}
			if resp.Content != content || len(resp.ToolCalls) != 0 {
				t.Errorf("reply was changed: %+v", resp)
			}
			if strings.Contains(trace.String(), "RECOVERED") {
				t.Errorf("trace has a RECOVERED block")
			}
		})
	}
}

// retryEmptyUnstreamed is a per-family capability, not a user option.
func TestRetryEmptyUnstreamedCapabilityOff(t *testing.T) {
	withTestDialect(t, &dialect{name: "noretry", markers: []string{"noretry"}, defaults: capabilities{recoverToolCalls: true}})
	srv, raw := rawServer(t,
		sse(chunk(`{"reasoning_content":"hm"}`, "stop")),
		`{"choices":[{"message":{"role":"assistant","content":"full"},"finish_reason":"stop"}]}`)
	var trace bytes.Buffer
	a := NewAdapter(srv.URL, "k", nil).WithTrace(&trace)
	a.MaxAttempts = 1
	resp, err := a.Complete(context.Background(), CompletionReq{Model: "x/noretry", Messages: agentHistory()})
	if err != nil {
		t.Fatal(err)
	}
	if len(*raw) != 1 || resp.Content != "" {
		t.Errorf("requests = %d, content = %q; want one request and the empty reply", len(*raw), resp.Content)
	}
	if strings.Contains(trace.String(), "empty streamed reply") {
		t.Errorf("trace has the unstreamed retry")
	}
}

func TestDialectHooksRunLast(t *testing.T) {
	var sawMarks bool
	var finishSaw []bool
	withTestDialect(t, &dialect{
		name: "hooked", markers: []string{"hooked"},
		defaults: capabilities{cacheMarkers: true, recoverToolCalls: true, retryEmptyUnstreamed: true},
		prepare: func(req *wireReq, c capabilities) {
			for _, m := range req.Messages {
				if m.cacheBreakpoint {
					sawMarks = true
				}
			}
			req.ToolChoice = "required"
		},
		finish: func(resp *CompletionResp, tools []ToolSchema, c capabilities) {
			finishSaw = append(finishSaw, len(resp.ToolCalls) == 1)
		},
	})
	content := "Reading.\n\n" + xmlCall
	run := func(replies ...string) []map[string]any {
		finishSaw = nil
		bodies, _ := sentBody(t, CompletionReq{Model: "x/hooked", Tools: equivTools(), Messages: agentHistory()}, replies...)
		return bodies
	}

	bodies := run(sse(chunk(`{"content":`+jsonStr(content)+`}`, "stop")))
	if !sawMarks {
		t.Error("prepare ran before cache marking")
	}
	if bodies[0]["tool_choice"] != "required" {
		t.Errorf("prepare's edit did not reach the wire: %v", bodies[0]["tool_choice"])
	}
	if !slices.Equal(finishSaw, []bool{true}) {
		t.Errorf("streamed: finish saw recovered calls = %v", finishSaw)
	}

	run(`{"choices":[{"message":{"role":"assistant","content":` + jsonStr(content) + `},"finish_reason":"stop"}]}`)
	if !slices.Equal(finishSaw, []bool{true}) {
		t.Errorf("buffered: finish saw recovered calls = %v", finishSaw)
	}

	// Empty streamed reply, then a full buffered one: finish runs on both reads.
	bodies = run(sse(chunk(`{"reasoning_content":"hm"}`, "stop")),
		`{"choices":[{"message":{"role":"assistant","content":`+jsonStr(content)+`},"finish_reason":"stop"}]}`)
	if len(bodies) != 2 {
		t.Fatalf("requests = %d, want 2", len(bodies))
	}
	if !slices.Equal(finishSaw, []bool{false, true}) {
		t.Errorf("unstreamed retry: finish saw recovered calls = %v, want [false true]", finishSaw)
	}
}

func TestDialectRegistryInvariants(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range dialects {
		if seen[d.name] {
			t.Errorf("duplicate dialect %q", d.name)
		}
		seen[d.name] = true
		if !d.defaults.recoverToolCalls || !d.defaults.retryEmptyUnstreamed {
			t.Errorf("%s: recoverToolCalls and retryEmptyUnstreamed default on today", d.name)
		}
	}
	if got := DialectNames(); !slices.Equal(got, []string{DialectAnthropic, DialectOpenAI}) {
		t.Errorf("DialectNames = %v", got)
	}
	last := dialects[len(dialects)-1]
	if last.name != DialectOpenAI || last.markers != nil {
		t.Errorf("the last dialect must be openai with no markers, got %s %v", last.name, last.markers)
	}
}
