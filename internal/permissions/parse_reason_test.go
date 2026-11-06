package permissions

import (
	"errors"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
)

// parseReasonRows enumerates one bash command per parser refusal shape, the
// *ParseError.Reason the gate surfaces on the parse-layer permission decision,
// and the audit-trail wrapping (parseFloorReason prepends "unparseable
// command: "). Every row is fed through both the parser directly and the real
// gate so a future change that drops a reason — or relabels a construct — is
// caught at the shape responsible, not at a generic catch-all.
//
// Adding a new refusal shape to shellparse.go without adding a row here is a
// review defect: the audit trail will read every such refusal as
// "unparseable command" and we will not be able to attribute the cost to a
// construct.
var parseReasonRows = []struct {
	desc, cmd, reason string
}{
	{"arbitrary-arg wrapper (sudo)", "sudo rm -rf /", "arbitrary-arg wrapper (sudo)"},
	{"arbitrary-arg wrapper (xargs)", "xargs rm", "arbitrary-arg wrapper (xargs)"},
	{"arbitrary-arg wrapper (npx)", "npx create-x", "arbitrary-arg wrapper (npx)"},
	{"path-qualified argv0 (relative)", "./rm -rf x", "path-qualified argv0 (basename matching must not collapse a path to a bare name)"},
	{"path-qualified argv0 (absolute)", "/tmp/git status", "path-qualified argv0 (basename matching must not collapse a path to a bare name)"},
	{"opaque command word", "$CMD x", "opaque command name"},
	{"non-inert env assignment prefix", "FOO=$(id) make", "env-var assignment prefix not on the inert allowlist"},
	{"non-inert assignment-only statement", "LD_PRELOAD=evil.so", "assignment-only statement not on the inert allowlist"},
	{"process substitution", "make <(ls)", "word not statically resolvable"},
	{"arithmetic substitution", "echo $((1+1))", "word not statically resolvable"},
	{"here-doc body with substitution", "cat <<EOF\n$(rm -rf /)\nEOF", "redirect target not statically resolvable"},
	{"variable redirect target", "ls > $F", "redirect target not statically resolvable"},
	{"read-write redirect <>", "cat <> scratch", "redirect target not statically resolvable"},
	{"redirect-only statement", "> out.txt", "redirect without a command to carry its target"},
	{"assignment-only with redirect", "CI=1 > out.txt", "redirect without a command to carry its target"},
	{"here-string", "cat <<< hi", "redirect target not statically resolvable"},
	{"bare bash without -c", "bash", "bash/sh invocation without a parseable -c script"},
	{"bash with a script file (no -c)", "bash script.sh", "bash/sh invocation without a parseable -c script"},
	{"sh with no script word", "sh -c", "bash/sh invocation without a parseable -c script"},
	{"function declaration (plain)", "f() { rm -rf /; }", "unmodelled shell construct (FuncDecl)"},
	{"fork bomb", ":(){ :|:& };:", "unmodelled shell construct (FuncDecl)"},
	{"[[ ]] test clause", "[[ -f x ]]", "unmodelled shell construct (TestClause)"},
	{"arithmetic command", "((1+1))", "unmodelled shell construct (ArithmCmd)"},
	{"let clause", "let x=1", "unmodelled shell construct (LetClause)"},
	{"c-style for header", "for ((i=0;i<10;i++)); do ls; done", "c-style for loop header (arithmetic, not words)"},
	{"env -i clears environment", "env -i make", "env with an unmodelled flag, unrecognised assignment, or non-inert prefix"},
	{"env -S re-splits", "env -S 'foo bar'", "env with an unmodelled flag, unrecognised assignment, or non-inert prefix"},
	{"env LD_PRELOAD assignment", "env LD_PRELOAD=x make", "env with an unmodelled flag, unrecognised assignment, or non-inert prefix"},
	{"timeout without duration", "timeout go test", "timeout with an unmodelled flag, missing duration, or non-duration operand"},
	{"timeout with non-duration operand", "timeout 30x make", "timeout with an unmodelled flag, missing duration, or non-duration operand"},
	{"timeout with two durations", "timeout 30 30 make", "timeout with an unmodelled flag, missing duration, or non-duration operand"},
	{"nice -n with no value", "nice -n", "wrapper (nice) with an unmodelled flag or value"},
	{"nohup with a flag-shaped word", "nohup -x make", "wrapper (nohup) with an unmodelled flag or value"},
	{"stdbuf -o with no value", "stdbuf -o", "wrapper (stdbuf) with an unmodelled flag or value"},
	{"wrapper peels down to no inner command", "timeout 30", "wrapper peeled down to no inner command"},
	{"redirect into a non-existent slot", "echo $(rm -rf /) > /dev/null", "word not statically resolvable"},
	{"case discriminant is a substitution running", "case $(rm -rf /) in a) ls ;; esac", "case discriminant not statically resolvable"},
	// A C-style for header is arithmetic, not words; that row above covers it.
	// A for-clause with a substituted item (`for f in $LIST`) is now ALLOWED
	// under change 0070 D5: $LIST is opaque and the body determines whether
	// the loop is safe. To exercise the "for-clause item not resolvable"
	// reason we need an item whose own word fails classifyWord, which an
	// arithmetic substitution does.
	{"for-clause item is an arithmetic substitution", "for f in $((1+1)); do ls; done", "for-clause item not statically resolvable"},
	// The length cap is checked before the parse, so its reason is fixed.
	{"command over the length cap", "echo " + strings.Repeat("a", maxCommandBytes-4), "command over the length cap"},
	// A literal that the bash parser itself rejects (here: an array assignment
	// in inline position; bash itself errors). The parse error path produces
	// "shell parse error" rather than a more specific construct name — the
	// parser never sees the AST, only the error string.
	{"bash parser error", "if then fi (", "shell parse error"},
}

// TestSplitSegments_ParseReason_PerShape pins the *ParseError.Reason each
// refusal shape produces, end-to-end at the parser. errors.Is(err,
// ErrUnparseable) keeps returning true for backwards compatibility; errors.As
// recovers the reason the audit trail will carry.
func TestSplitSegments_ParseReason_PerShape(t *testing.T) {
	for _, r := range parseReasonRows {
		t.Run(r.desc, func(t *testing.T) {
			_, err := splitSegments(r.cmd)
			if err == nil {
				t.Fatalf("splitSegments(%q) must fail closed", r.cmd)
			}
			if !errors.Is(err, ErrUnparseable) {
				t.Errorf("errors.Is(%v, ErrUnparseable) must hold; sentinel compatibility", err)
			}
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("splitSegments(%q) must return *ParseError, got %T", r.cmd, err)
			}
			if pe.Reason != r.reason {
				t.Errorf("splitSegments(%q) Reason = %q, want %q", r.cmd, pe.Reason, r.reason)
			}
			// Error() embeds the reason so logs that format the error string
			// see it without going through errors.As.
			if !strings.Contains(err.Error(), r.reason) {
				t.Errorf("err.Error() = %q must contain reason %q", err.Error(), r.reason)
			}
		})
	}
}

// TestGate_ParseReason_ReachesAuditTrail pins the end-to-end promise of the
// widening: the parse-layer permission decision (the ask emitted when a human
// is bound) carries the *ParseError.Reason in Decision.Reason, so the audit
// trail can attribute each refusal to a shell construct rather than reading
// every refusal as the same opaque string.
//
// The policy-bound path's LayerRules deny keeps its old reason: the user asked
// only for the parse-layer decision to carry the reason, and the policy deny
// is a different code path with a different audit shape. A row above in
// TestSplitSegments_ParseReason_PerShape proves the parser's reason; this
// test proves the gate threads it onto the audit event.
func TestGate_ParseReason_ReachesAuditTrail(t *testing.T) {
	// One row per refusal shape is overkill here — the parser table already
	// pins each Reason, and the gate plumbing is uniform — but spot-checking a
	// few shapes from each family catches a future regression that drops the
	// threading in just one of them (e.g. only the auto path, or only the
	// smart path).
	cases := []struct {
		desc, cmd, reason string
	}{
		{"wrapper", "sudo rm -rf /", "arbitrary-arg wrapper (sudo)"},
		{"path-qualified argv0", "/bin/rm -rf /", "path-qualified argv0 (basename matching must not collapse a path to a bare name)"},
		{"unresolvable word", "make <(ls)", "word not statically resolvable"},
		{"unmodelled construct", "f() { ls; }", "unmodelled shell construct (FuncDecl)"},
		{"length cap", "echo " + strings.Repeat("a", maxCommandBytes-4), "command over the length cap"},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			ctx, rec := recordDecisions()
			approve, called := newApproveRecorder(true)
			g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), Human(approve),
				WithWorkspaceRoot(t.TempDir()))
			g.Execute(ctx, "bash", bashArgs(tc.cmd))
			if !*called {
				t.Fatalf("approver must be consulted (human-bound path); got %+v", *rec)
			}
			if len(*rec) == 0 {
				t.Fatalf("no decision recorded for %q", tc.cmd)
			}
			ask := (*rec)[0]
			want := parseFloorReason(tc.reason)
			if ask.Verdict != "ask" || ask.Layer != LayerParse || ask.Reason != want {
				t.Errorf("ask decision = %+v, want ask/parse with reason %q", ask, want)
			}
		})
	}
}

// TestGate_ParseReason_SmartMode proves the smart-mode human-bound parse ask
// carries the reason too — the gate's smart branch threads it through
// unparseableBash, which the auto-mode path doesn't share. A regression that
// only fixes auto mode would leave smart-mode refusals opaque, and the audit
// trail would still be unable to attribute them.
func TestGate_ParseReason_SmartMode(t *testing.T) {
	ctx, rec := recordDecisions()
	approve, called := newApproveRecorder(true)
	cfg := config.PermissionsConfig{Mode: "smart"}
	g := New(cfg, newTestRegistry("bash"), Human(approve), WithWorkspaceRoot(t.TempDir()))
	g.Execute(ctx, "bash", bashArgs("sudo ls"))
	if !*called {
		t.Fatalf("approver must be consulted; got %+v", *rec)
	}
	ask := (*rec)[0]
	want := parseFloorReason("arbitrary-arg wrapper (sudo)")
	if ask.Verdict != "ask" || ask.Layer != LayerParse || ask.Mode != "smart" || ask.Reason != want {
		t.Errorf("smart-mode ask = %+v, want ask/parse/smart with reason %q", ask, want)
	}
}

// TestParseError_SentinelCompat pins the back-compat surface: every call site
// in the codebase that uses errors.Is(err, ErrUnparseable) keeps working, the
// rich error implements Error() in a way that surfaces the reason when one
// exists, and a *ParseError without a Reason degrades to the original
// ErrUnparseable.Error() so log lines stay byte-identical to the pre-change
// shape on the (impossible-by-construction) defensive empty-reason path.
func TestParseError_SentinelCompat(t *testing.T) {
	pe := &ParseError{Reason: "opaque command name"}
	if !errors.Is(pe, ErrUnparseable) {
		t.Errorf("errors.Is(*ParseError, ErrUnparseable) must hold")
	}
	if got := pe.Error(); got == ErrUnparseable.Error() {
		t.Errorf("Error() = %q must include the reason", got)
	}
	var dst *ParseError
	if !errors.As(pe, &dst) {
		t.Errorf("errors.As must recover *ParseError")
	}
	if dst.Reason != "opaque command name" {
		t.Errorf("recovered Reason = %q, want %q", dst.Reason, "opaque command name")
	}

	empty := &ParseError{}
	if empty.Error() != ErrUnparseable.Error() {
		t.Errorf("empty *ParseError Error() = %q, want %q", empty.Error(), ErrUnparseable.Error())
	}
	if !errors.Is(empty, ErrUnparseable) {
		t.Errorf("empty *ParseError must still satisfy errors.Is(_, ErrUnparseable)")
	}

	// A plain ErrUnparseable stays a plain ErrUnparseable — nothing in the
	// flow returns it, but a caller recovering through errors.As must see no
	// *ParseError rather than a panic or a mis-classified nil.
	var recovered *ParseError
	if errors.As(ErrUnparseable, &recovered) {
		t.Errorf("errors.As(ErrUnparseable, **ParseError) must be false, got %+v", recovered)
	}
}

// TestParseError_NilSafety makes sure a *ParseError(nil) — reachable in theory
// through an interface variable — does not panic and degrades to the
// sentinel's Error() text.
func TestParseError_NilSafety(t *testing.T) {
	var pe *ParseError
	if got := pe.Error(); got != ErrUnparseable.Error() {
		t.Errorf("(*ParseError)(nil).Error() = %q, want %q", got, ErrUnparseable.Error())
	}
}

// TestParseReject_CentralisedVocabulary keeps the refusal vocabulary in one
// place: parseReject and parseRejectf are the only constructors the parser
// uses, so a future refusal site that hand-rolls &ParseError{…} is a review
// defect caught here.
func TestParseReject_CentralisedVocabulary(t *testing.T) {
	if err := parseReject("opaque command name"); err == nil {
		t.Fatal("parseReject returned nil")
	} else if !errors.Is(err, ErrUnparseable) {
		t.Errorf("parseReject result must satisfy errors.Is(_, ErrUnparseable); got %v", err)
	}
	if err := parseRejectf("arbitrary-arg wrapper (%s)", "sudo"); err == nil {
		t.Fatal("parseRejectf returned nil")
	} else if !errors.Is(err, ErrUnparseable) {
		t.Errorf("parseRejectf result must satisfy errors.Is(_, ErrUnparseable); got %v", err)
	}
	if err := parseRejectf("wrapper (%s) with an unmodelled flag", "nice"); err == nil {
		t.Fatal("parseRejectf returned nil")
	} else if !strings.Contains(err.Error(), "wrapper (nice)") {
		t.Errorf("parseRejectf formatting lost the name: %v", err)
	}
}

// TestParseFloorReason_EmptyStaysEmpty is a defensive pin: when the parser
// returns a *ParseError without a Reason (which it cannot, but the gate should
// still behave sanely on the off-chance), parseFloorReason must keep the old
// empty-reason shape rather than synthesising "unparseable command: ".
func TestParseFloorReason_EmptyStaysEmpty(t *testing.T) {
	if got := parseFloorReason(""); got != "" {
		t.Errorf("parseFloorReason(\"\") = %q, want \"\"", got)
	}
	if got := parseFloorReason("opaque command name"); got != "unparseable command: opaque command name" {
		t.Errorf("parseFloorReason = %q, want prefix %q", got, "unparseable command: opaque command name")
	}
}

// TestGate_ParseReason_DoesNotChangePolicyDeny verifies that the
// widening only affects the human-bound parse-layer ask: the policy-bound
// LayerRules deny reason keeps its old "unparseable command denied; no
// approver bound: <cmd>" shape, because a separate code path serves it and
// the user's request was narrowly scoped to the parse-layer decision.
func TestGate_ParseReason_DoesNotChangePolicyDeny(t *testing.T) {
	ctx, rec := recordDecisions()
	g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), AlwaysApprove,
		WithWorkspaceRoot(t.TempDir()))
	res := g.Execute(ctx, "bash", bashArgs("sudo rm -rf /"))
	if !res.Denied || res.DenyLayer != LayerRules {
		t.Fatalf("policy-bound unparseable must deny at rules; got %+v", res)
	}
	want := unparseableDenyPrefix + "sudo rm -rf /"
	if len(*rec) != 1 {
		t.Fatalf("want exactly one decision, got %+v", *rec)
	}
	if (*rec)[0].Reason != want {
		t.Errorf("emitted decision reason = %q, want %q (unchanged)", (*rec)[0].Reason, want)
	}
}
