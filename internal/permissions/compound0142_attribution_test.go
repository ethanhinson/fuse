package permissions

import (
	"context"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
)

// TestSegmentVerdicts_AgreeWithTheHeuristic is the drift pin:
// segmentHeuristicVerdict re-implements classifyHeuristic per segment, and the
// two must not disagree. A whole-compound ask with every segment "allow" tells
// the classifier the risky part is already proven and names no culprit.
func TestSegmentVerdicts_AgreeWithTheHeuristic(t *testing.T) {
	root, _ := gateInRoot(t)
	roots := []string{root}
	for _, cmd := range []string{
		"echo x > /etc/passwd && make test",
		"cat a > /tmp/out && make test",
		"ls >> /etc/hosts && git status",
		"curl https://example.com && echo y > /etc/z",
		"kill -9 $PID && ls",
		"rm $(echo /) && ls",
		"docker compose up && ls",
		"pkill foo && ls",
		"rm -f /etc/hosts && ls",
		"cd /etc && ls",
		"git status && make test",
		"docker ps && make test",
		`cd "$(pwd)" && git status`,
		"cat a > out.log && make test",
		"kill -9 123 && ls",
	} {
		t.Run(cmd, func(t *testing.T) {
			segs, err := splitSegments(cmd)
			if err != nil {
				t.Fatalf("splitSegments: %v", err)
			}
			whole := classifyHeuristic(segs, roots)
			allAllow := true
			for _, sv := range segmentVerdicts(segs, roots) {
				if sv.Verdict != VerdictAllow {
					allAllow = false
				}
			}
			if whole == VerdictAllow && !allAllow {
				t.Errorf("heuristic allowed the compound but a segment is not allow: %+v", segmentVerdicts(segs, roots))
			}
			if whole != VerdictAllow && allAllow {
				t.Errorf("heuristic = %v but every segment is allow, so nothing is attributable: %+v", whole, segmentVerdicts(segs, roots))
			}
		})
	}
}

// decisionsFor runs cmd through a human-bound gate and returns every decision.
func decisionsFor(t *testing.T, root, cmd string) []Decision {
	t.Helper()
	approve, _ := newApproveRecorder(true)
	g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), Human(approve), WithWorkspaceRoot(root))
	var out []Decision
	ctx := WithDecisionSink(context.Background(), func(d Decision) { out = append(out, d) })
	g.Execute(ctx, "bash", bashArgs(cmd))
	if len(out) == 0 {
		t.Fatalf("%q: no decision emitted", cmd)
	}
	return out
}

func nonAllowSegment(segs []SegmentVerdict, name string) bool {
	for _, s := range segs {
		if s.Name == name && s.Verdict != VerdictAllow {
			return true
		}
	}
	return false
}

// TestAudit_DenyNamesTheSegment: "cat x && rm -rf ~ is denied at that segment"
// (spec, verification). The audit record must say which part.
func TestAudit_DenyNamesTheSegment(t *testing.T) {
	root, _ := gateInRoot(t)
	d := decisionsFor(t, root, "cat x && rm -rf ~")[0]
	if d.Verdict != "deny" {
		t.Fatalf("verdict = %q, want deny", d.Verdict)
	}
	if !nonAllowSegment(d.SegmentVerdicts, "rm") {
		t.Errorf("deny does not attribute the rm segment: %+v", d.SegmentVerdicts)
	}
}

// TestAudit_AskEventNamesTheSegment: the ask event is the record of what
// interrupted the human, so it has to carry the attribution itself rather than
// leaving it to a later human-layer event that a cancelled prompt never emits.
func TestAudit_AskEventNamesTheSegment(t *testing.T) {
	root, _ := gateInRoot(t)
	ds := decisionsFor(t, root, "curl https://example.com && make test")
	if ds[0].Verdict != "ask" {
		t.Fatalf("first decision = %q, want ask", ds[0].Verdict)
	}
	if !nonAllowSegment(ds[0].SegmentVerdicts, "curl") {
		t.Errorf("ask event does not attribute the curl segment: %+v", ds[0].SegmentVerdicts)
	}
}

// TestAudit_AllowedCompoundShowsPerSegmentAllows: "the audit record shows
// per-segment allows" (spec, verification).
func TestAudit_AllowedCompoundShowsPerSegmentAllows(t *testing.T) {
	root, _ := gateInRoot(t)
	d := decisionsFor(t, root, "git status && make test")[0]
	if d.Verdict != "allow" {
		t.Fatalf("verdict = %q at %q, want allow", d.Verdict, d.Layer)
	}
	if len(d.SegmentVerdicts) != 2 {
		t.Fatalf("want 2 segment verdicts, got %+v", d.SegmentVerdicts)
	}
	for _, s := range d.SegmentVerdicts {
		if s.Verdict != VerdictAllow {
			t.Errorf("segment %q = %v, want allow", s.Name, s.Verdict)
		}
	}
}

// TestAudit_SingleCommandOmitsSegments: the field is documented nil for
// non-compound commands, which is what keeps those events byte-identical.
func TestAudit_SingleCommandOmitsSegments(t *testing.T) {
	root, _ := gateInRoot(t)
	for _, cmd := range []string{"git status", "make test", "curl https://example.com", "rm -f /etc/hosts"} {
		for _, d := range decisionsFor(t, root, cmd) {
			if len(d.SegmentVerdicts) != 0 {
				t.Errorf("%q (%s/%s): non-compound command carries segments: %+v", cmd, d.Verdict, d.Layer, d.SegmentVerdicts)
			}
		}
	}
}

// TestClassifierPrompt_GrayCompoundCarriesDecomposition drives the real gate to
// the classifier on a genuinely gray segment and reads the prompt it sent.
func TestClassifierPrompt_GrayCompoundCarriesDecomposition(t *testing.T) {
	root, _ := gateInRoot(t)
	stub := &stubCompleter{resp: model.CompletionResp{Content: `{"verdict":"allow","reason":"ok"}`}}
	approve, called := newApproveRecorder(false)
	g := New(autoCfg(config.AutoConfig{ClassifierModel: "m"}, nil, nil), newTestRegistry("bash"), Human(approve),
		WithWorkspaceRoot(root), WithClassifier(newTestClassifier(t, stub)))
	g.Execute(context.Background(), "bash", bashArgs("curl https://example.com && make test"))
	if *called {
		t.Error("an allowing classifier must not fall through to the human")
	}
	if stub.calls != 1 {
		t.Fatalf("classifier calls = %d, want 1", stub.calls)
	}
	prompt := stub.lastReq.Messages[len(stub.lastReq.Messages)-1].Content
	for _, want := range []string{"Decomposed segments", "curl: ask", "make: allow"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

// TestClassifierPrompt_UnsafeRedirectIsNotPresentedAsResolved: the prompt says
// the deterministic layers have resolved the named segments, so a segment with
// an out-of-root write cannot be rendered as allow there.
func TestClassifierPrompt_UnsafeRedirectIsNotPresentedAsResolved(t *testing.T) {
	root, _ := gateInRoot(t)
	stub := &stubCompleter{resp: model.CompletionResp{Content: `{"verdict":"deny","reason":"no"}`}}
	approve, _ := newApproveRecorder(false)
	g := New(autoCfg(config.AutoConfig{ClassifierModel: "m"}, nil, nil), newTestRegistry("bash"), Human(approve),
		WithWorkspaceRoot(root), WithClassifier(newTestClassifier(t, stub)))
	g.Execute(context.Background(), "bash", bashArgs("echo x > /etc/passwd && make test"))
	if stub.calls != 1 {
		t.Fatalf("classifier calls = %d, want 1", stub.calls)
	}
	prompt := stub.lastReq.Messages[len(stub.lastReq.Messages)-1].Content
	if strings.Contains(prompt, "echo: allow") {
		t.Errorf("out-of-root redirect rendered as allow:\n%s", prompt)
	}
}

// TestClassifierPrompt_NonCompoundIsByteIdentical pins the stated compatibility
// promise of ClassifyWithSegments: nil or single-element segments change nothing.
func TestClassifierPrompt_NonCompoundIsByteIdentical(t *testing.T) {
	prompt := func(segs []SegmentVerdict) string {
		stub := &stubCompleter{resp: model.CompletionResp{Content: `{"verdict":"allow","reason":"ok"}`}}
		c := newTestClassifier(t, stub)
		if segs == nil {
			c.Classify(context.Background(), nil, "bash", "make test")
		} else {
			c.ClassifyWithSegments(context.Background(), nil, "bash", "make test", segs)
		}
		return stub.lastReq.Messages[len(stub.lastReq.Messages)-1].Content
	}
	base := prompt(nil)
	if got := prompt([]SegmentVerdict{{Name: "make", Verdict: VerdictAsk}}); got != base {
		t.Errorf("single-element segments changed the prompt:\n%q\nvs\n%q", got, base)
	}
}
