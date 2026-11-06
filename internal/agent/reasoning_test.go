package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
)

type effortRecorder struct{ got []string }

func (e *effortRecorder) Complete(_ context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	e.got = append(e.got, req.ReasoningEffort)
	return model.CompletionResp{Content: "done", OutputTokens: 100, ReasoningTokens: 90}, nil
}

func TestReasoningEffort_ReachesEveryAgentCallAndTokensAreEmitted(t *testing.T) {
	comp := &effortRecorder{}
	rec := &recordingStore{}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 5, 100)
	a.SetEventSink(rec)
	a.ReasoningEffort = "low"
	if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "go"}}); err != nil {
		t.Fatal(err)
	}
	if len(comp.got) != 1 || comp.got[0] != "low" {
		t.Fatalf("want low on the call, got %v", comp.got)
	}
	found := false
	for _, e := range rec.events {
		if e.Kind == event.KindModelCallEnd && string(e.Payload) != "" {
			found = true
			if !containsJSONField(e.Payload, `"reasoning_tokens":90`) {
				t.Fatalf("model.call.end should carry reasoning_tokens: %s", e.Payload)
			}
		}
	}
	if !found {
		t.Fatal("no model.call.end emitted")
	}
}

func containsJSONField(payload []byte, field string) bool {
	return len(payload) > 0 && strings.Contains(string(payload), field)
}
