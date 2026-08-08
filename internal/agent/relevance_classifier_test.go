package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/model"
)

// recordingCompleter captures each request and returns a scripted response or
// error, for classifier tests.
type recordingCompleter struct {
	reqs   []model.CompletionReq
	resp   model.CompletionResp
	err    error
	respFn func(model.CompletionReq) (model.CompletionResp, error)
}

func (c *recordingCompleter) Complete(_ context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	c.reqs = append(c.reqs, req)
	if c.respFn != nil {
		return c.respFn(req)
	}
	return c.resp, c.err
}

func TestParseClassifierScores(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want int
		ok   bool
	}{
		{"clean", "0: 0.8\n1: 0.2\n", 2, true},
		{"with prose", "Here are scores:\n0: 0.9\nline 1: 0.1\n", 2, true},
		{"clamps", "0: 1.5\n1: -0.3\n", 2, true},
		{"missing index", "0: 0.5\n", 2, false},
		{"garbage", "no scores here", 1, false},
		{"empty", "", 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseClassifierScores(tt.out, tt.want)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v (got %v)", ok, tt.ok, got)
			}
			if ok {
				for _, v := range got {
					if v < 0 || v > 1 {
						t.Errorf("score out of range: %v", v)
					}
				}
			}
		})
	}
}

// borderlineResult is crafted so its heuristic score lands inside [lo,hi].
// A grep with no overlap and a mid recency lands around the middle.
func midResult(turn, curTurn int) (ToolResult, ScoreContext) {
	return ToolResult{ToolName: "grep", Result: "some match", Turn: turn},
		ScoreContext{Query: "unrelated query", CurTurn: curTurn}
}

func TestClassifierOnlyBorderlineIsClassified(t *testing.T) {
	h := defaultHeuristicScorer()
	comp := &recordingCompleter{resp: model.CompletionResp{Content: "0: 0.42"}}
	cs := newClassifierScorer(h, comp, "classifier", 10, 0.30, 0.60)

	// Clear-cut HIGH: newest read_file with query overlap → heuristic well above hi.
	clearCtx := ScoreContext{Query: "read internal/x.go", CurTurn: 5}
	clearHigh := ToolResult{ToolName: "read_file", Args: "internal/x.go", Result: "package x", Turn: 5}
	if cs.borderline(h.Score(clearHigh, clearCtx)) {
		t.Skip("crafted clear-cut result unexpectedly borderline; heuristic weights changed")
	}
	before := len(comp.reqs)
	cs.Score(clearHigh, clearCtx)
	if len(comp.reqs) != before {
		t.Errorf("clear-cut result must NOT call the classifier")
	}

	// Borderline: a mid-scoring result must trigger exactly one classifier call.
	r, ctx := midResult(3, 5)
	if !cs.borderline(h.Score(r, ctx)) {
		t.Skipf("crafted mid result not borderline (score=%v); heuristic weights changed", h.Score(r, ctx))
	}
	before = len(comp.reqs)
	got := cs.Score(r, ctx)
	if len(comp.reqs) != before+1 {
		t.Errorf("borderline result must call the classifier exactly once")
	}
	if got != 0.42 {
		t.Errorf("borderline score = %v, want the classifier's 0.42", got)
	}
}

func TestClassifierFailsBackToHeuristic(t *testing.T) {
	h := defaultHeuristicScorer()
	r, ctx := midResult(3, 5)
	heur := h.Score(r, ctx)
	if !newClassifierScorer(h, &recordingCompleter{}, "m", 1, 0.30, 0.60).borderline(heur) {
		t.Skipf("crafted result not borderline (score=%v)", heur)
	}

	// Transport error → heuristic score, and suppression arms.
	comp := &recordingCompleter{err: errors.New("gateway 500")}
	cs := newClassifierScorer(h, comp, "m", 1, 0.30, 0.60)
	if got := cs.Score(r, ctx); got != heur {
		t.Errorf("on error, score = %v, want heuristic %v", got, heur)
	}
	if !cs.suppressed {
		t.Error("a classifier failure must arm suppression")
	}
	// Subsequent calls skip the model entirely (suppressed).
	before := len(comp.reqs)
	cs.Score(r, ctx)
	if len(comp.reqs) != before {
		t.Error("suppressed classifier must not call the model again")
	}
}

func TestClassifierGarbageResponseFallsBack(t *testing.T) {
	h := defaultHeuristicScorer()
	r, ctx := midResult(3, 5)
	heur := h.Score(r, ctx)
	comp := &recordingCompleter{resp: model.CompletionResp{Content: "I cannot help with that"}}
	cs := newClassifierScorer(h, comp, "m", 1, 0.30, 0.60)
	if !cs.borderline(heur) {
		t.Skipf("crafted result not borderline (score=%v)", heur)
	}
	if got := cs.Score(r, ctx); got != heur {
		t.Errorf("garbage response: score = %v, want heuristic %v", got, heur)
	}
}

func TestClassifierBatchShapeAndTraceInputs(t *testing.T) {
	h := defaultHeuristicScorer()
	comp := &recordingCompleter{resp: model.CompletionResp{Content: "0: 0.5\n1: 0.5\n"}}
	cs := newClassifierScorer(h, comp, "classifier-model", 10, 0.30, 0.60)
	batch := []ToolResult{
		{ToolName: "grep", Args: "a", Result: "r1", Turn: 1},
		{ToolName: "bash", Args: "b", Result: "r2", Turn: 1},
	}
	scores, ok := cs.classify(context.Background(), batch, ScoreContext{Query: "q", CurTurn: 2})
	if !ok || len(scores) != 2 {
		t.Fatalf("classify returned ok=%v scores=%v", ok, scores)
	}
	req := comp.reqs[len(comp.reqs)-1]
	if req.Model != "classifier-model" {
		t.Errorf("classifier model = %q, want classifier-model", req.Model)
	}
	if req.ToolChoice != "none" {
		t.Errorf("classifier must force ToolChoice=none, got %q", req.ToolChoice)
	}
	// Every candidate index appears in the rendered user message.
	user := req.Messages[len(req.Messages)-1].Content
	if !strings.Contains(user, "0: tool=grep") || !strings.Contains(user, "1: tool=bash") {
		t.Errorf("batch message missing indexed candidates:\n%s", user)
	}
}
