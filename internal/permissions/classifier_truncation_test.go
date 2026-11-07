package permissions

import (
	"context"
	"errors"
	"testing"

	"github.com/ethanhinson/fuse/internal/model"
)

// verdictSeq replays replies and records each request's cap.
type verdictSeq struct {
	replies []model.CompletionResp
	errs    []error
	caps    []int
}

func (s *verdictSeq) Complete(_ context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	i := len(s.caps)
	s.caps = append(s.caps, req.MaxTokens)
	if i < len(s.errs) && s.errs[i] != nil {
		return model.CompletionResp{}, s.errs[i]
	}
	if i >= len(s.replies) {
		return s.replies[len(s.replies)-1], nil
	}
	return s.replies[i], nil
}

func classifierOver(t *testing.T, c completer) *Classifier {
	t.Helper()
	return newTestClassifierFrom(t, c)
}

var cutOffVerdict = model.CompletionResp{FinishReason: "length", InputTokens: 50, OutputTokens: classifierMaxTokens}

// A budget problem must not interrupt the human: one retry reaches the verdict.
func TestClassifierRetriesACutOffReply(t *testing.T) {
	stub := &verdictSeq{replies: []model.CompletionResp{cutOffVerdict, {Content: `{"verdict":"allow","reason":"ok"}`, FinishReason: "stop", InputTokens: 50, OutputTokens: 40}}}
	out := classifierOver(t, stub).Classify(context.Background(), nil, "bash", "make test")
	if out.Verdict != VerdictAllow || !out.Call.ParseOK || out.Call.Truncated {
		t.Fatalf("outcome = %+v", out)
	}
	if len(stub.caps) != 2 || stub.caps[1] != classifierRetryMaxTokens {
		t.Errorf("caps = %v", stub.caps)
	}
	if out.Call.OutputTokens != classifierMaxTokens+40 || out.Call.InputTokens != 100 {
		t.Errorf("audit does not show both attempts: %+v", out.Call)
	}
}

// Still fail closed when the retry misses too, and only retry once.
func TestClassifierFailsClosedAfterOneRetry(t *testing.T) {
	stub := &verdictSeq{replies: []model.CompletionResp{cutOffVerdict}}
	out := classifierOver(t, stub).Classify(context.Background(), nil, "bash", "make test")
	if out.Verdict != VerdictAsk || out.Call.ParseOK || !out.Call.Truncated {
		t.Fatalf("outcome = %+v", out)
	}
	if len(stub.caps) != 2 {
		t.Errorf("caps = %v, want exactly one retry", stub.caps)
	}
}

func TestClassifierFailsClosedWhenRetryErrors(t *testing.T) {
	stub := &verdictSeq{replies: []model.CompletionResp{cutOffVerdict}, errs: []error{nil, errors.New("boom")}}
	if out := classifierOver(t, stub).Classify(context.Background(), nil, "bash", "x"); out.Verdict != VerdictAsk {
		t.Fatalf("outcome = %+v", out)
	}
}

// A clean reply with no verdict is not a budget problem, so no retry.
func TestClassifierDoesNotRetryAnUntruncatedBadReply(t *testing.T) {
	stub := &verdictSeq{replies: []model.CompletionResp{{Content: "no idea", FinishReason: "stop", OutputTokens: 5}}}
	classifierOver(t, stub).Classify(context.Background(), nil, "bash", "x")
	if len(stub.caps) != 1 {
		t.Errorf("caps = %v", stub.caps)
	}
}

// A truncated reply can't sneak an allow through a partial parse.
func TestClassifierNeverAllowsOnGarbledTruncation(t *testing.T) {
	stub := &verdictSeq{replies: []model.CompletionResp{{Content: `{"verdict":"all`, FinishReason: "length", OutputTokens: classifierMaxTokens}}}
	if out := classifierOver(t, stub).Classify(context.Background(), nil, "bash", "rm x"); out.Verdict == VerdictAllow {
		t.Fatalf("outcome = %+v", out)
	}
}

// A degraded outcome says nothing about the command, so it must not stick for the
// session and keep prompting on every repeat.
func TestClassifierDoesNotCacheDegradedOutcomes(t *testing.T) {
	for name, stub := range map[string]*verdictSeq{
		"truncated": {replies: []model.CompletionResp{cutOffVerdict, cutOffVerdict, {Content: `{"verdict":"allow","reason":"ok"}`, FinishReason: "stop"}}},
		"error":     {replies: []model.CompletionResp{{Content: `{"verdict":"allow","reason":"ok"}`, FinishReason: "stop"}}, errs: []error{errors.New("boom")}},
	} {
		t.Run(name, func(t *testing.T) {
			c := classifierOver(t, stub)
			if first := c.Classify(context.Background(), nil, "bash", "make test"); first.Verdict != VerdictAsk {
				t.Fatalf("first = %+v", first)
			}
			second := c.Classify(context.Background(), nil, "bash", "make test")
			if second.Verdict != VerdictAllow || second.Call.Cached {
				t.Fatalf("second = %+v, want a fresh allow", second)
			}
		})
	}
}

// A considered verdict still caches.
func TestClassifierStillCachesConsideredVerdicts(t *testing.T) {
	stub := &verdictSeq{replies: []model.CompletionResp{{Content: `{"verdict":"deny","reason":"no"}`, FinishReason: "stop"}}}
	c := classifierOver(t, stub)
	c.Classify(context.Background(), nil, "bash", "x")
	if second := c.Classify(context.Background(), nil, "bash", "x"); !second.Call.Cached || len(stub.caps) != 1 {
		t.Fatalf("second = %+v, calls = %d", second, len(stub.caps))
	}
}

func TestWebFetchClassifierRetriesToo(t *testing.T) {
	stub := &verdictSeq{replies: []model.CompletionResp{cutOffVerdict, {Content: `{"verdict":"allow","reason":"ok"}`, FinishReason: "stop"}}}
	out := classifierOver(t, stub).ClassifyWebFetch(context.Background(), nil, "example.org", false, `{"url":"https://example.org"}`)
	if out.Verdict != VerdictAllow {
		t.Fatalf("outcome = %+v", out)
	}
}
