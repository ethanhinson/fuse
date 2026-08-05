package permissions

import (
	"context"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
)

// stubCompleter is a test double for the completer interface. It records the
// request it received, counts how many times it was invoked, and returns a
// canned response (or error) so each classifier path can be exercised in
// isolation.
type stubCompleter struct {
	resp    model.CompletionResp
	err     error
	calls   int
	lastReq model.CompletionReq
}

func (s *stubCompleter) Complete(_ context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	s.calls++
	s.lastReq = req
	return s.resp, s.err
}

// newTestClassifier builds a Classifier over the given stub completer with a
// resolvable classifier model, so tests exercise the verdict logic rather than
// model resolution.
func newTestClassifier(t *testing.T, stub *stubCompleter) *Classifier {
	t.Helper()
	reg := model.NewRegistry("m", map[string]model.ModelConfig{
		"m": {ID: "cloud/m", MaxTokens: 256},
	})
	cfg := config.AutoConfig{ClassifierModel: "m"}
	return newClassifier(stub, reg, cfg, nil)
}

func TestClassifyDenyVerdict(t *testing.T) {
	stub := &stubCompleter{resp: model.CompletionResp{Content: `{"verdict":"deny","reason":"destructive"}`}}
	c := newTestClassifier(t, stub)

	got := c.Classify(context.Background(), nil, "bash", "rm -rf /")
	if got != VerdictDeny {
		t.Fatalf("expected VerdictDeny, got %v", got)
	}
}

func TestClassifyAllowVerdict(t *testing.T) {
	stub := &stubCompleter{resp: model.CompletionResp{Content: `{"verdict":"allow","reason":"read-only"}`}}
	c := newTestClassifier(t, stub)

	got := c.Classify(context.Background(), nil, "bash", "ls -la")
	if got != VerdictAllow {
		t.Fatalf("expected VerdictAllow, got %v", got)
	}
}

func TestClassifyTimeoutFailsClosed(t *testing.T) {
	stub := &stubCompleter{err: context.DeadlineExceeded}
	c := newTestClassifier(t, stub)

	got := c.Classify(context.Background(), nil, "bash", "rm -rf /")
	if got != VerdictAsk {
		t.Fatalf("timeout/error must fail closed to VerdictAsk, got %v", got)
	}
}

func TestClassifyMalformedReplyFailsClosed(t *testing.T) {
	for _, content := range []string{
		"not json at all",
		`{"verdict":"maybe"}`,
		`{"reason":"no verdict field"}`,
		"",
		`{`,
	} {
		stub := &stubCompleter{resp: model.CompletionResp{Content: content}}
		c := newTestClassifier(t, stub)
		got := c.Classify(context.Background(), nil, "bash", "echo hi")
		if got != VerdictAsk {
			t.Fatalf("malformed reply %q must fail closed to VerdictAsk, got %v", content, got)
		}
	}
}

func TestClassifyInputHygiene(t *testing.T) {
	stub := &stubCompleter{resp: model.CompletionResp{Content: `{"verdict":"ask","reason":"unclear"}`}}
	c := newTestClassifier(t, stub)

	userMessages := []model.Message{
		{Role: "user", Content: "please delete the temp files"},
		// A tool-result message and an assistant-reasoning message that MUST NOT
		// reach the classifier prompt.
		{Role: "tool", ToolCallID: "call_1", Name: "bash", Content: "SECRET_TOOL_RESULT deleted 5 files"},
		{Role: "assistant", Content: "ACTOR_REASONING I will now run rm to clean up"},
		{Role: "user", Content: "go ahead"},
	}

	c.Classify(context.Background(), userMessages, "bash", "rm -rf tmp/")

	if stub.calls != 1 {
		t.Fatalf("expected exactly 1 completer call, got %d", stub.calls)
	}

	// Assert on the request the classifier actually sent.
	var joined strings.Builder
	sawUser := false
	for _, m := range stub.lastReq.Messages {
		joined.WriteString(m.Role)
		joined.WriteString("|")
		joined.WriteString(m.Content)
		joined.WriteString("\n")
		if m.Role == "tool" {
			t.Errorf("classifier prompt must not contain a tool-result message (Role==tool)")
		}
		if strings.Contains(m.Content, "please delete the temp files") {
			sawUser = true
		}
	}
	blob := joined.String()

	if !sawUser {
		t.Errorf("classifier prompt must contain the user's messages; got:\n%s", blob)
	}
	if strings.Contains(blob, "SECRET_TOOL_RESULT") {
		t.Errorf("classifier prompt leaked a tool result; got:\n%s", blob)
	}
	if strings.Contains(blob, "ACTOR_REASONING") {
		t.Errorf("classifier prompt leaked actor reasoning; got:\n%s", blob)
	}
	// The pending tool call (tool name + command) must be present.
	if !strings.Contains(blob, "bash") || !strings.Contains(blob, "rm -rf tmp/") {
		t.Errorf("classifier prompt must contain the pending tool call; got:\n%s", blob)
	}
}

func TestClassifyCachesByToolAndNormalizedCommand(t *testing.T) {
	stub := &stubCompleter{resp: model.CompletionResp{Content: `{"verdict":"deny","reason":"cached"}`}}
	c := newTestClassifier(t, stub)

	first := c.Classify(context.Background(), nil, "bash", "rm -rf /")
	second := c.Classify(context.Background(), nil, "bash", "rm -rf /")

	if first != VerdictDeny || second != VerdictDeny {
		t.Fatalf("expected both verdicts VerdictDeny, got %v and %v", first, second)
	}
	if stub.calls != 1 {
		t.Fatalf("identical classify calls must hit the completer exactly once, got %d", stub.calls)
	}
}

func TestVerdictCacheClone(t *testing.T) {
	parent := newVerdictCache()
	parent.Store("bash", "rm -rf /", VerdictDeny)

	clone := parent.Clone()

	if v, ok := clone.Lookup("bash", "rm -rf /"); !ok || v != VerdictDeny {
		t.Error("clone must contain the entry that existed in parent before clone")
	}

	// Post-clone parent addition must not propagate to the clone.
	parent.Store("bash", "echo hi", VerdictAllow)
	if _, ok := clone.Lookup("bash", "echo hi"); ok {
		t.Error("post-clone parent addition must not propagate to clone")
	}

	// Clone addition must not propagate back to parent.
	clone.Store("read_file", "{}", VerdictAllow)
	if _, ok := parent.Lookup("read_file", "{}"); ok {
		t.Error("clone addition must not propagate back to parent")
	}
}
