package agent

import (
	"context"
	"reflect"
	"testing"

	"github.com/ethanhinson/fuse/internal/model"
)

type dialectReqRecorder struct{ reqs []model.CompletionReq }

func (r *dialectReqRecorder) Complete(_ context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	r.reqs = append(r.reqs, req)
	return model.CompletionResp{Content: "done"}, nil
}

func TestLoopSendsDialectOptions(t *testing.T) {
	on := true
	opts := model.DialectOptions{CacheMarkers: &on, ReasoningField: "reasoning"}
	comp := &dialectReqRecorder{}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 10, 100)
	a.ContextWindow = 4000
	a.Dialect = model.DialectAnthropic
	a.DialectOptions = opts
	sc := &dialectReqRecorder{}
	a.SetSummarizer(newSummarizer(&dialectSummaryCompleter{dialectReqRecorder: sc}, "m", 2000), nil)

	if _, err := a.Run(context.Background(), overBudgetHistory()); err != nil {
		t.Fatal(err)
	}
	if len(comp.reqs) == 0 || !reflect.DeepEqual(comp.reqs[0].DialectOptions, opts) {
		t.Fatalf("agent request options = %+v", comp.reqs)
	}
	if len(sc.reqs) == 0 {
		t.Fatal("the summarizer was not called")
	}
	for _, r := range sc.reqs {
		if !reflect.DeepEqual(r.DialectOptions, model.DialectOptions{}) || r.Dialect != "" {
			t.Errorf("summarizer request carries dialect settings: %+v", r)
		}
	}
}

type dialectSummaryCompleter struct{ *dialectReqRecorder }

func (s *dialectSummaryCompleter) Complete(ctx context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	s.reqs = append(s.reqs, req)
	return model.CompletionResp{Content: "Objective: x\nNext: y"}, nil
}
