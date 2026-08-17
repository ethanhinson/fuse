package permissions

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
)

// seqCompleter is a test completer that returns a scripted sequence of replies,
// one per call, so a test can drive the classifier through a specific verdict
// pattern. Once the sequence is exhausted it repeats the final reply.
type seqCompleter struct {
	replies []string
	calls   int
}

func (s *seqCompleter) Complete(_ context.Context, _ model.CompletionReq) (model.CompletionResp, error) {
	i := s.calls
	s.calls++
	if i >= len(s.replies) {
		i = len(s.replies) - 1
	}
	return model.CompletionResp{Content: s.replies[i]}, nil
}

// newTestClassifierFrom builds a Classifier over any completer (the interface),
// so tests can drive it with a scripted seqCompleter as well as a stubCompleter.
func newTestClassifierFrom(t *testing.T, c completer) *Classifier {
	t.Helper()
	reg := model.NewRegistry("m", map[string]model.ModelConfig{
		"m": {ID: "cloud/m", MaxTokens: 256},
	})
	return newClassifier(c, reg, config.AutoConfig{ClassifierModel: "m"}, nil)
}

// grayAreaCmd is a mutating command whose path escapes the workspace so the
// heuristic returns Ask and the pipeline routes to the classifier — the site the
// escalation valve counts. Each distinct index yields a fresh command so the
// per-command verdict cache never short-circuits the classifier call.
func grayAreaCmd(i int) string {
	return "touch /etc/escapes-workspace-" + string(rune('a'+i))
}

// valvePromptRecorder is an ApprovalFunc that records every request it sees and
// answers the valve-recovery sentinel with valveAnswer and everything else with
// perCallAnswer, so a test can drive the change-0067 recovery flow precisely.
type valvePromptRecorder struct {
	reqs          []ApprovalRequest
	valveAnswer   bool
	perCallAnswer bool
}

func (r *valvePromptRecorder) approve(_ context.Context, req ApprovalRequest) (bool, bool, error) {
	r.reqs = append(r.reqs, req)
	if req.ToolName == ValveApprovalToolName {
		return r.valveAnswer, false, nil
	}
	return r.perCallAnswer, false, nil
}

func (r *valvePromptRecorder) valvePrompts() int {
	n := 0
	for _, req := range r.reqs {
		if req.ToolName == ValveApprovalToolName {
			n++
		}
	}
	return n
}

// TestValve_TripInteractive_OneRecoveryPromptResetsAndContinues proves the
// change-0067 recovery semantics: after 3 consecutive classifier blocks trip
// the valve, the NEXT gray-area call issues exactly ONE valve-recovery prompt
// (the sentinel ToolName, counts in the preview); approving it resets the valve
// and the pending call proceeds to the classifier (the stub call count
// advances) instead of auto mode staying paused forever.
func TestValve_TripInteractive_OneRecoveryPromptResetsAndContinues(t *testing.T) {
	stub := &stubCompleter{resp: model.CompletionResp{Content: `{"verdict":"deny","reason":"x"}`}}
	cls := newTestClassifier(t, stub)
	rec := &valvePromptRecorder{valveAnswer: true, perCallAnswer: true}
	g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), rec.approve,
		WithWorkspaceRoot(t.TempDir()), WithClassifier(cls), WithInteractive(true))

	// First 3 gray-area calls each hit the classifier and get a deny (block).
	for i := 0; i < 3; i++ {
		res := g.Execute(context.Background(), "bash", bashArgs(grayAreaCmd(i)))
		if !res.IsError {
			t.Fatalf("block %d: classifier deny should surface as an error, got: %s", i, res.Output)
		}
		if len(rec.reqs) != 0 {
			t.Fatalf("block %d: classifier deny must not route to the human", i)
		}
	}
	if stub.calls != 3 {
		t.Fatalf("expected 3 classifier calls before the valve trips, got %d", stub.calls)
	}

	// The 4th gray-area call finds the valve tripped: ONE recovery prompt is
	// issued; approval resets the valve and the call proceeds to the classifier.
	res := g.Execute(context.Background(), "bash", bashArgs(grayAreaCmd(3)))
	if got := rec.valvePrompts(); got != 1 {
		t.Fatalf("expected exactly 1 valve recovery prompt, got %d (reqs: %+v)", got, rec.reqs)
	}
	if !strings.Contains(rec.reqs[0].Preview, "auto mode has denied") {
		t.Errorf("valve prompt preview should carry the counts; got %q", rec.reqs[0].Preview)
	}
	if stub.calls != 4 {
		t.Fatalf("approved recovery must consult the classifier for the pending call, got %d calls", stub.calls)
	}
	// The stub still denies, so the pending call surfaces as a classifier deny.
	if !res.IsError {
		t.Fatalf("stub denies: expected a classifier deny after recovery, got: %s", res.Output)
	}
	// Recovery reset both counters: the deny above re-recorded only 1 consecutive
	// block, so the next call goes straight to the classifier with NO new prompt.
	g.Execute(context.Background(), "bash", bashArgs(grayAreaCmd(4)))
	if got := rec.valvePrompts(); got != 1 {
		t.Fatalf("valve must not re-prompt below thresholds after reset, got %d prompts", got)
	}
	if stub.calls != 5 {
		t.Fatalf("post-recovery call must reach the classifier, got %d calls", stub.calls)
	}
}

// TestValve_TripInteractive_RejectionFallsBackToPerCallAsks proves the rejection
// arm: declining the one recovery prompt leaves the valve tripped, so gray-area
// calls fall back to per-call human asks — and the valve question itself is
// never re-asked (promptedOnce holds until reset).
func TestValve_TripInteractive_RejectionFallsBackToPerCallAsks(t *testing.T) {
	stub := &stubCompleter{resp: model.CompletionResp{Content: `{"verdict":"deny","reason":"x"}`}}
	cls := newTestClassifier(t, stub)
	rec := &valvePromptRecorder{valveAnswer: false, perCallAnswer: true}
	g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), rec.approve,
		WithWorkspaceRoot(t.TempDir()), WithClassifier(cls), WithInteractive(true))

	for i := 0; i < 3; i++ {
		g.Execute(context.Background(), "bash", bashArgs(grayAreaCmd(i)))
	}

	// 4th call: valve prompt rejected ⇒ per-call human ask for the actual tool
	// call (approved by the recorder), classifier NOT consulted.
	res := g.Execute(context.Background(), "bash", bashArgs(grayAreaCmd(3)))
	if got := rec.valvePrompts(); got != 1 {
		t.Fatalf("expected 1 valve prompt, got %d", got)
	}
	if stub.calls != 3 {
		t.Fatalf("rejected recovery must not consult the classifier, got %d calls", stub.calls)
	}
	if res.IsError {
		t.Fatalf("per-call ask approved should execute, got: %s", res.Output)
	}

	// 5th call: NO second valve prompt — straight to the per-call ask.
	g.Execute(context.Background(), "bash", bashArgs(grayAreaCmd(4)))
	if got := rec.valvePrompts(); got != 1 {
		t.Fatalf("valve question must not be re-asked after rejection, got %d prompts", got)
	}
	if stub.calls != 3 {
		t.Fatalf("classifier must stay paused after rejection, got %d calls", stub.calls)
	}
}

// TestValve_ThreeConsecutiveBlocks_AbortsNonInteractive proves that in a
// non-interactive gate the tripped valve aborts with a structured summary error
// naming the trip and the counts, without consulting the classifier again.
func TestValve_ThreeConsecutiveBlocks_AbortsNonInteractive(t *testing.T) {
	stub := &stubCompleter{resp: model.CompletionResp{Content: `{"verdict":"deny","reason":"x"}`}}
	cls := newTestClassifier(t, stub)
	approve, called := newApproveRecorder(true)
	g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), approve,
		WithWorkspaceRoot(t.TempDir()), WithClassifier(cls)) // non-interactive (default)

	for i := 0; i < 3; i++ {
		res := g.Execute(context.Background(), "bash", bashArgs(grayAreaCmd(i)))
		if !res.IsError {
			t.Fatalf("block %d should error, got: %s", i, res.Output)
		}
	}

	res := g.Execute(context.Background(), "bash", bashArgs(grayAreaCmd(3)))
	if stub.calls != 3 {
		t.Fatalf("valve tripped: classifier must not be consulted again, got %d calls", stub.calls)
	}
	if *called {
		t.Fatal("non-interactive valve trip must not route to the human")
	}
	if !res.IsError {
		t.Fatalf("non-interactive valve trip must abort with an error, got: %s", res.Output)
	}
	// The summary error names the valve and reports the counts.
	if !strings.Contains(strings.ToLower(res.Output), "auto mode paused") &&
		!strings.Contains(strings.ToLower(res.Output), "escalation valve") {
		t.Fatalf("valve abort must name the trip; got: %q", res.Output)
	}
	if !strings.Contains(res.Output, "3") {
		t.Fatalf("valve abort must report the block counts; got: %q", res.Output)
	}
}

// TestValve_NonBlockVerdictResetsConsecutive proves that an allow/ask verdict
// between blocks resets the consecutive counter, so it takes a fresh run of 3
// blocks to trip.
func TestValve_NonBlockVerdictResetsConsecutive(t *testing.T) {
	// A programmable completer: deny, deny, allow, then deny thereafter.
	seq := []string{
		`{"verdict":"deny","reason":"x"}`,
		`{"verdict":"deny","reason":"x"}`,
		`{"verdict":"allow","reason":"ok"}`, // resets consecutive
		`{"verdict":"deny","reason":"x"}`,
		`{"verdict":"deny","reason":"x"}`,
		`{"verdict":"deny","reason":"x"}`, // 3rd consecutive of the fresh run
	}
	stub := &seqCompleter{replies: seq}
	cls := newTestClassifierFrom(t, stub)
	approve, called := newApproveRecorder(true)
	g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), approve,
		WithWorkspaceRoot(t.TempDir()), WithClassifier(cls))

	// Run the 6 scripted gray-area calls; each must reach the classifier because
	// the valve has not yet tripped (the allow reset the run).
	for i := 0; i < 6; i++ {
		g.Execute(context.Background(), "bash", bashArgs(grayAreaCmd(i)))
	}
	if stub.calls != 6 {
		t.Fatalf("all 6 verdicts must reach the classifier (valve not tripped early); got %d calls", stub.calls)
	}
	if *called {
		t.Fatal("no valve trip yet, so the human must not have been consulted")
	}

	// The 7th call is the one after the 3rd consecutive block of the fresh run:
	// the valve is now tripped and must not consult the classifier again.
	g.Execute(context.Background(), "bash", bashArgs(grayAreaCmd(6)))
	if stub.calls != 6 {
		t.Fatalf("valve should have tripped on the 3rd fresh-run block; classifier calls = %d", stub.calls)
	}
}

// TestValve_TwentyTotalBlocks_TripsIndependentOfConsecutive proves the total
// threshold trips even when the consecutive counter is repeatedly reset.
func TestValve_TwentyTotalBlocks_TripsIndependentOfConsecutive(t *testing.T) {
	// Alternate deny, allow so the consecutive counter never reaches 3, but the
	// total block count climbs to 20 over 40 classified calls.
	var seq []string
	for i := 0; i < 40; i++ {
		if i%2 == 0 {
			seq = append(seq, `{"verdict":"deny","reason":"x"}`)
		} else {
			seq = append(seq, `{"verdict":"allow","reason":"ok"}`)
		}
	}
	stub := &seqCompleter{replies: seq}
	cls := newTestClassifierFrom(t, stub)
	approve, _ := newApproveRecorder(true)
	g := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), approve,
		WithWorkspaceRoot(t.TempDir()), WithClassifier(cls))

	// After 40 alternating verdicts there are 20 blocks total. The 39th
	// classified call is the 20th block (indices 0,2,...,38). After that block the
	// valve is tripped, so the 40th call (index 39) must NOT reach the classifier.
	for i := 0; i < 40; i++ {
		g.Execute(context.Background(), "bash", bashArgs(grayAreaCmd(i)))
	}
	if stub.calls != 39 {
		t.Fatalf("total-20 valve should trip after the 20th block (39 classified calls); got %d", stub.calls)
	}
}

// TestValve_SharedAcrossCloneForChild proves a child gate's classifier blocks
// count toward the same session valve as the parent.
func TestValve_SharedAcrossCloneForChild(t *testing.T) {
	stub := &stubCompleter{resp: model.CompletionResp{Content: `{"verdict":"deny","reason":"x"}`}}
	cls := newTestClassifier(t, stub)
	approve, _ := newApproveRecorder(true)
	parent := New(autoCfg(config.AutoConfig{}, nil, nil), newTestRegistry("bash"), approve,
		WithWorkspaceRoot(t.TempDir()), WithClassifier(cls))

	child := parent.CloneForChild("worker")

	// Parent absorbs 2 consecutive blocks.
	parent.Execute(context.Background(), "bash", bashArgs(grayAreaCmd(0)))
	parent.Execute(context.Background(), "bash", bashArgs(grayAreaCmd(1)))
	// The child's block is the 3rd toward the SAME session valve ⇒ trips it.
	child.Execute(context.Background(), "bash", bashArgs(grayAreaCmd(2)))
	if stub.calls != 3 {
		t.Fatalf("expected 3 classifier calls across parent+child before trip, got %d", stub.calls)
	}

	// Now the parent's next gray-area call must find the valve tripped (shared
	// counter) and NOT consult the classifier.
	parent.Execute(context.Background(), "bash", bashArgs(grayAreaCmd(3)))
	if stub.calls != 3 {
		t.Fatalf("valve tripped by child must pause the parent too; classifier calls = %d", stub.calls)
	}
}

// TestValve_InWorkspaceEditsDoNotAdvanceValve proves the D4 accept-edits posture:
// in-workspace write_file/edit_file calls resolve to VerdictAllow via the D1
// path-scoping branch BEFORE ever reaching the classifier, so they never feed the
// escalation valve. Only true classifier denies advance it. This is the structural
// fix that stops routine, task-implied edits from tripping the valve mid-task.
func TestValve_InWorkspaceEditsDoNotAdvanceValve(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(tempdir): %v", err)
	}
	inWorkspace := filepath.Join(root, "file.go")

	// A classifier that would DENY (block) everything it sees — so if any edit
	// reached it, the valve would advance and, past 3, trip.
	stub := &stubCompleter{resp: model.CompletionResp{Content: `{"verdict":"deny","reason":"x"}`}}
	cls := newTestClassifier(t, stub)
	approve, called := newApproveRecorder(true)
	g := New(autoCfg(config.AutoConfig{}, nil, nil),
		newTestRegistry("write_file", "edit_file"), approve,
		WithWorkspaceRoot(root), WithClassifier(cls))

	// Far more in-workspace edits than either valve threshold (3 consecutive / 20
	// total). If any advanced the valve, it would trip well before the end.
	for i := 0; i < 25; i++ {
		toolName := "write_file"
		if i%2 == 1 {
			toolName = "edit_file"
		}
		res := g.Execute(context.Background(), toolName, `{"path":`+jsonStr(inWorkspace)+`}`)
		if res.IsError {
			t.Fatalf("edit %d: in-workspace edit must auto-approve, got error: %s", i, res.Output)
		}
	}

	if stub.calls != 0 {
		t.Fatalf("in-workspace edits must never reach the classifier; got %d classifier calls", stub.calls)
	}
	if *called {
		t.Fatal("in-workspace edits must not route to the human approval func")
	}
	if consecutive, total := g.valve.counts(); consecutive != 0 || total != 0 {
		t.Fatalf("in-workspace edits must not advance the valve; got consecutive=%d total=%d", consecutive, total)
	}
}
