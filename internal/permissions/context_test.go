package permissions

import (
	"context"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
)

// TestClassifyOrAsk_ForwardsCtxUserMessages proves the classifier-layer
// invocation site (classifyOrAsk, reached via the bash gray-area path) forwards
// the user turns carried on ctx by WithUserMessages: the completer's request
// contains the user turns and NO tool-result or assistant-reasoning message.
func TestClassifyOrAsk_ForwardsCtxUserMessages(t *testing.T) {
	stub := &stubCompleter{resp: model.CompletionResp{Content: `{"verdict":"allow","reason":"ok"}`}}
	cls := newTestClassifier(t, stub)
	approve, _ := newApproveRecorder(true)
	g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), approve,
		WithWorkspaceRoot(t.TempDir()), WithClassifier(cls))

	msgs := []model.Message{
		{Role: "user", Content: "CANARY_USER_TURN please clean up"},
		{Role: "tool", ToolCallID: "c1", Name: "bash", Content: "CANARY_TOOL_RESULT wiped"},
		{Role: "assistant", Content: "CANARY_ASSISTANT reasoning here"},
	}
	ctx := WithUserMessages(context.Background(), msgs)

	// A mutating command whose path escapes the workspace ⇒ heuristic Ask ⇒
	// classifier (the classifyOrAsk seam under test).
	g.Execute(ctx, "bash", bashArgs("touch /etc/escapes-workspace"))

	if stub.calls != 1 {
		t.Fatalf("classifier should have been consulted exactly once, got %d", stub.calls)
	}

	var blob strings.Builder
	for _, m := range stub.lastReq.Messages {
		if m.Role == "tool" {
			t.Errorf("forwarded classifier prompt must not contain a tool-result message (Role==tool)")
		}
		blob.WriteString(m.Role + "|" + m.Content + "\n")
	}
	s := blob.String()
	if !strings.Contains(s, "CANARY_USER_TURN") {
		t.Errorf("forwarded prompt must contain the user turn; got:\n%s", s)
	}
	if strings.Contains(s, "CANARY_TOOL_RESULT") {
		t.Errorf("forwarded prompt leaked a tool result; got:\n%s", s)
	}
	if strings.Contains(s, "CANARY_ASSISTANT") {
		t.Errorf("forwarded prompt leaked assistant reasoning; got:\n%s", s)
	}
}

// TestClassifyOrAsk_NilSafeWithoutCtxMessages proves the nil-safe default:
// when WithUserMessages was never called, classifyOrAsk forwards nil user
// messages (today's behavior), so the classifier prompt carries only the system
// instruction plus the pending-call turn — no extra user turns.
func TestClassifyOrAsk_NilSafeWithoutCtxMessages(t *testing.T) {
	stub := &stubCompleter{resp: model.CompletionResp{Content: `{"verdict":"allow","reason":"ok"}`}}
	cls := newTestClassifier(t, stub)
	approve, _ := newApproveRecorder(true)
	g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), approve,
		WithWorkspaceRoot(t.TempDir()), WithClassifier(cls))

	// Bare context: never passed through WithUserMessages.
	g.Execute(context.Background(), "bash", bashArgs("touch /etc/escapes-workspace"))

	if stub.calls != 1 {
		t.Fatalf("classifier should have been consulted exactly once, got %d", stub.calls)
	}
	// Exactly two messages: the system instruction and the pending-call user turn.
	if got := len(stub.lastReq.Messages); got != 2 {
		t.Fatalf("nil user history should yield system + pending-call only (2 msgs), got %d", got)
	}
}

// TestClassifyWebFetch_ForwardsCtxUserMessages proves the web_fetch classifier
// seam (classifyWebFetch) forwards the ctx-carried user turns too, dropping
// tool/assistant messages.
func TestClassifyWebFetch_ForwardsCtxUserMessages(t *testing.T) {
	stub := &stubCompleter{resp: model.CompletionResp{Content: `{"verdict":"allow","reason":"ok"}`}}
	cls := newTestClassifier(t, stub)
	approve, _ := newApproveRecorder(true)
	g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("web_fetch"), approve,
		WithWorkspaceRoot(t.TempDir()), WithClassifier(cls))

	msgs := []model.Message{
		{Role: "user", Content: "WF_CANARY_USER fetch the docs"},
		{Role: "tool", ToolCallID: "c1", Name: "web_fetch", Content: "WF_CANARY_TOOL body"},
		{Role: "assistant", Content: "WF_CANARY_ASSISTANT reasoning"},
	}
	ctx := WithUserMessages(context.Background(), msgs)

	// A fallthrough host reaches the reputation-aware classifier.
	g.Execute(ctx, "web_fetch", `{"url":"https://a.example.com/x"}`)

	if stub.calls != 1 {
		t.Fatalf("web_fetch classifier should have been consulted exactly once, got %d", stub.calls)
	}
	var blob strings.Builder
	for _, m := range stub.lastReq.Messages {
		if m.Role == "tool" {
			t.Errorf("forwarded web_fetch prompt must not contain a tool-result message")
		}
		blob.WriteString(m.Role + "|" + m.Content + "\n")
	}
	s := blob.String()
	if !strings.Contains(s, "WF_CANARY_USER") {
		t.Errorf("forwarded web_fetch prompt must contain the user turn; got:\n%s", s)
	}
	if strings.Contains(s, "WF_CANARY_TOOL") || strings.Contains(s, "WF_CANARY_ASSISTANT") {
		t.Errorf("forwarded web_fetch prompt leaked non-user content; got:\n%s", s)
	}
}

// TestUserMessagesFrom_NilSafe proves userMessagesFrom returns nil for a
// context that never carried user messages.
func TestUserMessagesFrom_NilSafe(t *testing.T) {
	if got := userMessagesFrom(context.Background()); got != nil {
		t.Fatalf("bare context should yield nil user messages, got %#v", got)
	}
	msgs := []model.Message{{Role: "user", Content: "hi"}}
	ctx := WithUserMessages(context.Background(), msgs)
	got := userMessagesFrom(ctx)
	if len(got) != 1 || got[0].Content != "hi" {
		t.Fatalf("round-trip failed; got %#v", got)
	}
}
