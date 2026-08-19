package permissions

import (
	"context"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
)

// These tests cover the audit-completeness additions (change 0069 merge-gate
// follow-up): the classifier's rationale and call metadata surviving onto the
// decision, human-approval provenance, and credential redaction in previews.

func TestClassifierOutcomeRetainsReasonAndMeta(t *testing.T) {
	stub := &stubCompleter{resp: model.CompletionResp{
		Content:      `{"verdict":"allow","reason":"routine network read"}`,
		InputTokens:  600,
		OutputTokens: 115,
	}}
	c := newTestClassifier(t, stub)

	out := c.Classify(context.Background(), nil, "bash", "curl -s https://api.github.com/zen")
	if out.Verdict != VerdictAllow {
		t.Fatalf("verdict = %v, want allow", out.Verdict)
	}
	if out.Reason != "routine network read" {
		t.Fatalf("reason = %q, want the model's rationale retained", out.Reason)
	}
	call := out.Call
	if call.Model != "cloud/m" || call.InputTokens != 600 || call.OutputTokens != 115 {
		t.Fatalf("call meta = %+v, want model and token accounting", call)
	}
	if !call.ParseOK || call.Truncated || call.Cached {
		t.Fatalf("clean reply must be ParseOK, not Truncated/Cached: %+v", call)
	}

	// Second identical call: served from cache, marked as such.
	again := c.Classify(context.Background(), nil, "bash", "curl -s https://api.github.com/zen")
	if !again.Call.Cached {
		t.Fatalf("cache hit must set Cached, got %+v", again.Call)
	}
	if stub.calls != 1 {
		t.Fatalf("cache hit must not re-call the completer, calls=%d", stub.calls)
	}
}

func TestClassifierTruncationIsAttributed(t *testing.T) {
	// A reasoning model that burns the whole completion budget produces no
	// content: the verdict must fail closed to ask AND the call metadata must
	// say why — this is the 128-token incident made visible.
	stub := &stubCompleter{resp: model.CompletionResp{
		Content:      "",
		OutputTokens: classifierMaxTokens,
	}}
	c := newTestClassifier(t, stub)

	out := c.Classify(context.Background(), nil, "bash", "some gray command")
	if out.Verdict != VerdictAsk {
		t.Fatalf("truncated reply must fail closed to ask, got %v", out.Verdict)
	}
	if !out.Call.Truncated || out.Call.ParseOK {
		t.Fatalf("truncation must be attributed: %+v", out.Call)
	}
	if !strings.Contains(out.Reason, "truncated") {
		t.Fatalf("reason should name the truncation, got %q", out.Reason)
	}
}

func TestDecisionCarriesClassifierMetaAndReason(t *testing.T) {
	stub := &stubCompleter{resp: model.CompletionResp{
		Content:      `{"verdict":"allow","reason":"routine dev operation"}`,
		OutputTokens: 40,
	}}
	cls := newTestClassifier(t, stub)
	ctx, rec := recordDecisions()
	g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), AlwaysApprove,
		WithWorkspaceRoot(t.TempDir()), WithClassifier(cls))

	// A gray-area command that reaches the classifier.
	res := g.Execute(ctx, "bash", bashArgs("curl -s https://api.github.com/zen"))
	if res.IsError {
		t.Fatalf("classifier allow should execute, got %+v", res)
	}
	if len(*rec) != 1 {
		t.Fatalf("want one decision, got %+v", *rec)
	}
	d := (*rec)[0]
	if d.Verdict != "allow" || d.Layer != LayerClassifier {
		t.Fatalf("want classifier allow, got %+v", d)
	}
	if d.Reason != "routine dev operation" {
		t.Fatalf("allow decision must retain the classifier's rationale, got %q", d.Reason)
	}
	if d.Classifier == nil || d.Classifier.Model != "cloud/m" || !d.Classifier.ParseOK {
		t.Fatalf("allow decision must carry classifier call meta, got %+v", d.Classifier)
	}
}

func TestHumanDecisionCarriesProvenance(t *testing.T) {
	// smart mode, non-safelisted tool ⇒ ask ⇒ AlwaysApprove answers. With the
	// gate labeled policy-approved, the human-layer events must say "policy".
	ctx, rec := recordDecisions()
	g := New(config.PermissionsConfig{Mode: "smart"}, newTestRegistry("some_tool"), AlwaysApprove,
		WithApprovalProvenance(DecidedByPolicy))
	g.Execute(ctx, "some_tool", `{}`)

	var ask, allow *Decision
	for i := range *rec {
		switch (*rec)[i].Verdict {
		case "ask":
			ask = &(*rec)[i]
		case "allow":
			allow = &(*rec)[i]
		}
	}
	if ask == nil || allow == nil {
		t.Fatalf("want ask then allow decisions, got %+v", *rec)
	}
	if ask.DecidedBy != "" {
		t.Errorf("the pre-approval ask has no decider yet, got %q", ask.DecidedBy)
	}
	if allow.Layer != LayerHuman || allow.DecidedBy != DecidedByPolicy {
		t.Errorf("policy-approved allow must be labeled policy, got %+v", allow)
	}

	// Default (no option): the human posture.
	ctx2, rec2 := recordDecisions()
	g2 := New(config.PermissionsConfig{Mode: "smart"}, newTestRegistry("some_tool"), AlwaysApprove)
	g2.Execute(ctx2, "some_tool", `{}`)
	for _, d := range *rec2 {
		if d.Layer == LayerHuman && d.Verdict == "allow" && d.DecidedBy != DecidedByHuman {
			t.Errorf("default provenance must be human, got %+v", d)
		}
	}
}

func TestCommandPreviewRedactsURLUserinfo(t *testing.T) {
	got := commandPreview("web_fetch", `{"url":"https://alice:s3cret@example.com/data?q=1"}`)
	if strings.Contains(got, "s3cret") || strings.Contains(got, "alice") {
		t.Fatalf("preview leaked credentials: %q", got)
	}
	if !strings.Contains(got, "https://***@example.com") {
		t.Fatalf("preview should keep scheme and host, got %q", got)
	}

	// Bash previews with embedded URLs are scrubbed too.
	got = commandPreview("bash", bashArgs("curl https://token123@internal.example/x"))
	if strings.Contains(got, "token123") {
		t.Fatalf("bash preview leaked credentials: %q", got)
	}

	// No userinfo ⇒ untouched.
	got = commandPreview("bash", bashArgs("curl https://example.com/x"))
	if got != "curl https://example.com/x" {
		t.Fatalf("credential-free preview must be unchanged, got %q", got)
	}
}
