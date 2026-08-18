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

// TestPendingCallPrompt_WorkspaceContext pins the #0069 D1b context line: when a
// workspace root and/or scratch dir are set the pending-call prompt names them,
// and when both are empty the line is suppressed entirely — a zero-value
// Classifier must never emit a degenerate "workspace: , scratch: " line. The
// existing pending-call shape and the trailing JSON instruction survive either
// way.
func TestPendingCallPrompt_WorkspaceContext(t *testing.T) {
	const (
		root    = "/tmp/ws-root"
		scratch = "/tmp/ws-root/.fuse/scratch"
	)

	t.Run("present when set", func(t *testing.T) {
		stub := &stubCompleter{resp: model.CompletionResp{Content: `{"verdict":"allow"}`}}
		c := newTestClassifier(t, stub).WithWorkspaceContext(root, scratch)

		c.Classify(context.Background(), nil, "bash", "ls -la")

		last := stub.lastReq.Messages[len(stub.lastReq.Messages)-1].Content
		if !strings.Contains(last, "workspace: "+root) {
			t.Errorf("pending prompt must name the workspace root; got:\n%s", last)
		}
		if !strings.Contains(last, "scratch: "+scratch) {
			t.Errorf("pending prompt must name the scratch dir; got:\n%s", last)
		}
		if !strings.Contains(last, "Pending tool call to classify:\ntool: bash\ncommand: ls -la") {
			t.Errorf("pending prompt lost its pending-call shape; got:\n%s", last)
		}
		if !strings.Contains(last, "Respond with the JSON verdict object.") {
			t.Errorf("pending prompt lost the JSON verdict instruction; got:\n%s", last)
		}
	})

	t.Run("omitted when both empty", func(t *testing.T) {
		stub := &stubCompleter{resp: model.CompletionResp{Content: `{"verdict":"allow"}`}}
		c := newTestClassifier(t, stub) // zero workspace context

		c.Classify(context.Background(), nil, "bash", "ls -la")

		last := stub.lastReq.Messages[len(stub.lastReq.Messages)-1].Content
		if strings.Contains(last, "workspace:") || strings.Contains(last, "scratch:") {
			t.Errorf("pending prompt must omit the context line when unset; got:\n%s", last)
		}
		if !strings.Contains(last, "Respond with the JSON verdict object.") {
			t.Errorf("pending prompt lost the JSON verdict instruction; got:\n%s", last)
		}
	})

	t.Run("nil receiver is safe", func(t *testing.T) {
		var c *Classifier
		if got := c.WithWorkspaceContext(root, scratch); got != nil {
			t.Fatalf("WithWorkspaceContext on a nil receiver must return nil, got %v", got)
		}
	})
}

// TestClassifierSystemPrompt_AllowBiased pins the #0069 retune of the shared
// classifier system instruction: routine developer work is named as expected and
// allowable, the genuinely dangerous shapes are named as the deny set, the old
// block-bias sentence is gone, and the JSON verdict contract survives verbatim.
// Assertions are on lowercased substrings for concepts — never whole sentences —
// so the wording can breathe without breaking the test.
func TestClassifierSystemPrompt_AllowBiased(t *testing.T) {
	lower := strings.ToLower(classifierSystemPrompt)

	// Allow-shapes the gate must explicitly recognize as routine dev work.
	allowShapes := map[string][]string{
		"network reads":                 {"curl", "wget", "api call"},
		"package installs":              {"npm", "pip", "cargo", "go "},
		"managing own dev processes":    {"kill", "pkill", "dev server"},
		"temp/scratch directory use":    {"temp", "scratch"},
		"an explicit allow disposition": {"allow"},
	}
	for shape, needles := range allowShapes {
		for _, n := range needles {
			if !strings.Contains(lower, n) {
				t.Errorf("system prompt must name the allow-shape %q (missing %q); got:\n%s", shape, n, classifierSystemPrompt)
			}
		}
	}

	// Deny-shapes: the only things the retuned prompt should refuse outright.
	denyShapes := map[string][]string{
		"secret/workspace-data exfiltration": {"exfiltrat", "secret"},
		"piping remote content into a shell": {"into a shell"},
		"privilege escalation":               {"privilege escalation"},
		"destruction outside the workspace":  {"outside the workspace"},
		"credential harvesting":              {"credential harvesting"},
	}
	for shape, needles := range denyShapes {
		for _, n := range needles {
			if !strings.Contains(lower, n) {
				t.Errorf("system prompt must name the deny-shape %q (missing %q); got:\n%s", shape, n, classifierSystemPrompt)
			}
		}
	}

	// The old block-bias instruction must be gone: it is what produced the
	// routine-dev-op denials #0069 exists to fix.
	if strings.Contains(lower, "be block-biased") {
		t.Errorf("system prompt must no longer instruct block bias; got:\n%s", classifierSystemPrompt)
	}

	// "ask" must be reserved for genuine ambiguity, not the default posture.
	if !strings.Contains(lower, "ambigu") {
		t.Errorf("system prompt must reserve \"ask\" for genuinely ambiguous calls; got:\n%s", classifierSystemPrompt)
	}

	// The machine contract is load-bearing and survives the retune verbatim:
	// parseVerdict only maps allow/deny/ask out of a single JSON object.
	if !strings.Contains(classifierSystemPrompt, `{"verdict":"allow|deny|ask"`) {
		t.Errorf("system prompt must keep the JSON verdict contract verbatim; got:\n%s", classifierSystemPrompt)
	}
	if !strings.Contains(lower, "exactly one json object") {
		t.Errorf("system prompt must keep the one-object reply instruction; got:\n%s", classifierSystemPrompt)
	}
}

// TestClassifierSystemPrompt_BoundsKillFamily pins the kill-family clause of the
// #0069 allow bias. internal/permissions/heuristics.go routes the whole family to
// the classifier unconditionally (`case "pkill", "killall": return VerdictAsk`,
// and any kill that is not provably benign), so this prompt clause is the ONLY
// gate in front of `pkill -9 -f node`. The allow must therefore be bounded to
// what the model can actually check in the command text it receives — a numeric
// PID, or a pattern naming a specific dev-server/watcher binary — and a broad or
// unclear pattern must be named as "ask". Assertions are lowercased concept
// substrings, never whole sentences.
func TestClassifierSystemPrompt_BoundsKillFamily(t *testing.T) {
	lower := strings.ToLower(classifierSystemPrompt)

	// The old wording authorized the family on "a dev server or watcher it
	// started" — a predicate the classifier's inputs (system prompt + user turns
	// + one pending command) cannot evaluate. It must be gone.
	if strings.Contains(lower, "it started") {
		t.Errorf("system prompt must not gate the kill family on an uncheckable \"it started\" predicate; got:\n%s", classifierSystemPrompt)
	}

	// The bounded allow: a specific numeric PID, or a specifically-named binary.
	for _, n := range []string{"numeric pid", "specific"} {
		if !strings.Contains(lower, n) {
			t.Errorf("system prompt must bound the kill allow to a checkable target (missing %q); got:\n%s", n, classifierSystemPrompt)
		}
	}

	// The bounded ask must sit with the kill clause, not merely somewhere in the
	// prompt: a broad/unclear pattern, a bare -9 with no specific target, a
	// wildcard, or a system process name.
	idx := strings.Index(lower, "pkill")
	if idx < 0 {
		t.Fatalf("system prompt must still name pkill/killall as the routed family; got:\n%s", classifierSystemPrompt)
	}
	end := idx + 600
	if end > len(lower) {
		end = len(lower)
	}
	clause := lower[idx:end]
	if !strings.Contains(clause, "killall") {
		t.Errorf("kill clause must cover killall alongside pkill; got:\n%s", classifierSystemPrompt)
	}
	for _, n := range []string{"ask", "broad", "wildcard", "-9", "system"} {
		if !strings.Contains(clause, n) {
			t.Errorf("kill clause must send a broad/unclear pattern to \"ask\" (missing %q near the kill wording); got:\n%s", n, classifierSystemPrompt)
		}
	}
}

// TestClassifyWebFetch_PromptNamesHostAndReputation proves the web_fetch verdict
// call names the target host and, since #0069, frames the fetch allow-biased
// (a read-only GET returning page text; fetching public pages is routine) while
// naming the concrete deny shapes that replaced the old vague "weigh domain
// reputation" instruction. Input hygiene is asserted alongside: only the system
// instruction, the user's own turns, and the pending prompt reach the model (no
// tool-result or assistant-reasoning messages).
func TestClassifyWebFetch_PromptNamesHostAndReputation(t *testing.T) {
	stub := &stubCompleter{resp: model.CompletionResp{Content: `{"verdict":"ask","reason":"unknown host"}`}}
	c := newTestClassifier(t, stub)

	userMessages := []model.Message{
		{Role: "user", Content: "fetch the docs from that site"},
		{Role: "tool", ToolCallID: "call_1", Name: "web_fetch", Content: "SECRET_TOOL_RESULT page body"},
		{Role: "assistant", Content: "ACTOR_REASONING I will fetch it"},
		{Role: "user", Content: "go ahead"},
	}

	got := c.ClassifyWebFetch(context.Background(), userMessages, "unknown-blog.example", false, `{"url":"https://unknown-blog.example/x"}`)
	if got != VerdictAsk {
		t.Fatalf("expected VerdictAsk from the stub, got %v", got)
	}
	if stub.calls != 1 {
		t.Fatalf("expected exactly 1 completer call, got %d", stub.calls)
	}

	var joined strings.Builder
	sawUser := false
	for _, m := range stub.lastReq.Messages {
		joined.WriteString(m.Role)
		joined.WriteString("|")
		joined.WriteString(m.Content)
		joined.WriteString("\n")
		if m.Role == "tool" {
			t.Errorf("web_fetch classifier prompt must not contain a tool-result message (Role==tool)")
		}
		if m.Role == "assistant" {
			t.Errorf("web_fetch classifier prompt must not contain an assistant message")
		}
		if strings.Contains(m.Content, "fetch the docs from that site") {
			sawUser = true
		}
	}
	blob := joined.String()

	if !sawUser {
		t.Errorf("web_fetch prompt must contain the user's messages; got:\n%s", blob)
	}
	if strings.Contains(blob, "SECRET_TOOL_RESULT") || strings.Contains(blob, "ACTOR_REASONING") {
		t.Errorf("web_fetch prompt leaked a tool result / actor reasoning; got:\n%s", blob)
	}
	// The pending prompt must name the target host.
	if !strings.Contains(blob, "unknown-blog.example") {
		t.Errorf("web_fetch prompt must name the target host; got:\n%s", blob)
	}
	lower := strings.ToLower(blob)

	// Allow framing: the fetch is a read-only GET returning page text, and doing
	// it is routine work that defaults to allow.
	for _, want := range []string{"read-only get", "page text", "routine", "allow"} {
		if !strings.Contains(lower, want) {
			t.Errorf("web_fetch prompt must carry the allow framing %q; got:\n%s", want, blob)
		}
	}

	// Deny shapes the spec fixes, in place of the old vague "reputation" nudge.
	denyShapes := map[string][]string{
		"credential-bearing URLs":      {"credential", "token", "secret"},
		"webhook endpoints":            {"webhook", "hooks.slack.com", "discord.com/api/webhooks"},
		"paste/upload services":        {"paste", "upload", "exfiltrat"},
		"raw-IP URLs":                  {"raw-ip"},
		"URLs encoding workspace data": {"workspace data"},
	}
	for shape, wants := range denyShapes {
		for _, want := range wants {
			if !strings.Contains(lower, want) {
				t.Errorf("web_fetch prompt must name the deny shape %s (missing %q); got:\n%s", shape, want, blob)
			}
		}
	}
}

// TestClassifyWebFetch_KnownGoodHintNotABypass proves the known-good hint is
// surfaced to the model as a bias, not an absolute bypass: the prompt names the
// host AND states that a compromised subdomain of an otherwise good host stays
// deniable. The allow-biased rewrite (#0069) must not have softened this into a
// blanket permit.
func TestClassifyWebFetch_KnownGoodHintNotABypass(t *testing.T) {
	stub := &stubCompleter{resp: model.CompletionResp{Content: `{"verdict":"allow","reason":"docs"}`}}
	c := newTestClassifier(t, stub)

	c.ClassifyWebFetch(context.Background(), nil, "docs.example.com", true, `{"url":"https://docs.example.com/x"}`)

	var blob string
	for _, m := range stub.lastReq.Messages {
		blob += m.Content + "\n"
	}
	lower := strings.ToLower(blob)
	if !strings.Contains(lower, "docs.example.com") {
		t.Errorf("prompt must name the host; got:\n%s", blob)
	}
	// The known-good hint must be communicated as non-absolute (deniable).
	if !strings.Contains(lower, "not") || !strings.Contains(lower, "deniab") {
		t.Errorf("prompt must state the known-good hint is not an absolute bypass (still deniable); got:\n%s", blob)
	}
	// ...and specifically that a compromised subdomain of a good host is the
	// case the hint does not cover, and that the hint is not a bypass.
	for _, want := range []string{"subdomain", "bypass"} {
		if !strings.Contains(lower, want) {
			t.Errorf("prompt must state the hint is a bias not a bypass (missing %q); got:\n%s", want, blob)
		}
	}
	// The deny shapes stay in force for a known-good host too.
	for _, want := range []string{"credential", "webhook", "raw-ip", "workspace data"} {
		if !strings.Contains(lower, want) {
			t.Errorf("deny shapes must be named even when the known-good hint is true (missing %q); got:\n%s", want, blob)
		}
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
