package agent

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// lineRenderer records every Errorf line; everything else is dropped.
type lineRenderer struct {
	nopRenderer
	lines []string
}

func (r *lineRenderer) Errorf(format string, args ...any) {
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

var learnedWindowRE = regexp.MustCompile(`learned window of ~(\d+) tokens`)

// learnedWindows returns the window reported by each "learned window" line.
func (r *lineRenderer) learnedWindows(t *testing.T) []int {
	t.Helper()
	var out []int
	for _, l := range r.lines {
		m := learnedWindowRE.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("bad learned window in %q: %v", l, err)
		}
		out = append(out, n)
	}
	return out
}

func TestLearnContextWindow(t *testing.T) {
	cases := []struct {
		current, rejected int
		want              int
		ok                bool
	}{
		{0, 0, 0, false},
		{0, 1000, 1000, true},
		{1000, 800, 800, true},
		{1000, 1000, 1000, false},
		{1000, 1200, 1000, false},
	}
	for _, c := range cases {
		got, ok := learnContextWindow(c.current, c.rejected)
		if got != c.want || ok != c.ok {
			t.Errorf("learnContextWindow(%d, %d) = (%d, %v), want (%d, %v)", c.current, c.rejected, got, ok, c.want, c.ok)
		}
	}
}

// With no configured window, a provider length rejection teaches the loop a
// window from the rejected request; the normal prune path then runs against it
// and the run finishes.
func TestRunUnknownWindowLearnsFromRejectionAndRecovers(t *testing.T) {
	comp := &lengthRejectingCompleter{limitBytes: 70_000}
	r := &lineRenderer{}
	a := New(comp, &fakeExec{}, r, "m", "", 10, 100)

	big := strings.Repeat("x", 60_000)
	history := []model.Message{
		{Role: "user", Content: "task"},
		{Role: "tool", ToolCallID: "1", Name: "read_file", Content: big},
		{Role: "tool", ToolCallID: "2", Name: "read_file", Content: big},
	}
	hist, err := a.Run(context.Background(), history)
	if err != nil {
		t.Fatalf("expected recovery, got %v", err)
	}
	if comp.calls != 2 {
		t.Errorf("calls = %d, want 2 (reject, learn, prune, accepted)", comp.calls)
	}
	if hist[1].Content != prunedStub {
		t.Error("older tool result should be pruned against the learned window")
	}
	if got := r.learnedWindows(t); len(got) != 1 {
		t.Errorf("learned window lines = %v, want exactly one; lines: %q", got, r.lines)
	}
}

// shrinkingLimitCompleter accepts any request at or under its token limit and
// rejects anything over it for length. After its first rejection it lowers the
// limit to secondLimit, so the request sent after the first learn-and-prune is
// rejected again. Accepted requests report their size as InputTokens, so the
// loop's hybrid accounting has a non-empty accounted prefix when the rejections
// happen. Until it has rejected twice it keeps asking for another tool call;
// after that it answers "done".
type shrinkingLimitCompleter struct {
	limit       int
	secondLimit int
	rejections  int
	calls       int
	lastSent    int
}

func (c *shrinkingLimitCompleter) Complete(_ context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	c.calls++
	in := messagesSize(req.Messages) / bytesPerToken
	if in > c.limit {
		c.rejections++
		if c.rejections == 1 {
			c.limit = c.secondLimit
		}
		return model.CompletionResp{}, errors.New("gateway status 400: maximum context length exceeded")
	}
	c.lastSent = in
	if c.rejections >= 2 {
		return model.CompletionResp{Content: "done", InputTokens: in}, nil
	}
	return model.CompletionResp{
		ToolCalls:   []model.ToolCall{{ID: fmt.Sprintf("c%d", c.calls), Name: "read_file", Arguments: fmt.Sprintf(`{"path":"f%d"}`, c.calls)}},
		InputTokens: in,
	}, nil
}

// bigResultExec returns the same large output for every call.
type bigResultExec struct{ out string }

func (bigResultExec) Schemas() []model.ToolSchema { return nil }
func (e bigResultExec) Execute(context.Context, string, string) tools.Result {
	return tools.Result{Output: e.out}
}

// A second rejection learns a lower window and the run still recovers. This
// pins the accounting fix: stubs written inside the accounted prefix after the
// first learn must come off the estimate even though no successful response
// has reported fresh usage since.
//
// Numbers: each tool result is 40,000 bytes (~10,000 tokens). The first limit
// is 35,000 tokens, so turns 0-3 are accepted (~1, ~10k, ~20k, ~30k tokens)
// and the ~40k request on turn 4 is rejected; the loop learns ~40k. Its prune
// (protect ~10k) stubs two of the four results, leaving ~20k, which the lowered
// 15,000-token limit rejects; the loop learns ~20k. Without the accounting fix
// the next estimate is still ~40k (lastUsage covers the stubbed results),
// pruning cannot get it under 95% of ~20k, and the run ends with a false
// ErrContextTooLarge. With the fix the estimate is ~20k, one more result is
// stubbed, and the ~10k request is accepted.
func TestRunLearnedWindowLowersOnSecondRejection(t *testing.T) {
	const secondLimit = 15_000
	comp := &shrinkingLimitCompleter{limit: 35_000, secondLimit: secondLimit}
	r := &lineRenderer{}
	a := New(comp, bigResultExec{out: strings.Repeat("r", 40_000)}, r, "m", "", 50, 100)

	_, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "task"}})
	if err != nil {
		t.Fatalf("expected recovery after two rejections, got %v; lines: %q", err, r.lines)
	}
	if comp.rejections != 2 {
		t.Errorf("rejections = %d, want 2", comp.rejections)
	}
	windows := r.learnedWindows(t)
	if len(windows) != 2 {
		t.Fatalf("learned windows = %v, want two; lines: %q", windows, r.lines)
	}
	if windows[1] >= windows[0] {
		t.Errorf("second learned window %d must be below the first %d", windows[1], windows[0])
	}
	if comp.lastSent > secondLimit {
		t.Errorf("final accepted request ~%d tokens, want at or under %d", comp.lastSent, secondLimit)
	}
}

// alwaysLengthRejecting rejects every request for length.
type alwaysLengthRejecting struct{ calls int }

func (c *alwaysLengthRejecting) Complete(context.Context, model.CompletionReq) (model.CompletionResp, error) {
	c.calls++
	return model.CompletionResp{}, errors.New("gateway status 400: maximum context length exceeded")
}

// With no configured window and nothing to prune, the run ends after one
// rejected request with an ErrContextTooLarge that names the learned window,
// the config key to set, and the provider's own error.
func TestRunUnknownWindowUnrecoverableEndsWithLearnedMessage(t *testing.T) {
	comp := &alwaysLengthRejecting{}
	a := New(comp, &fakeExec{}, &lineRenderer{}, "m", "", 10, 100)

	_, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: strings.Repeat("y", 100_000)}})
	if !errors.Is(err, ErrContextTooLarge) {
		t.Fatalf("expected ErrContextTooLarge, got %v", err)
	}
	for _, want := range []string{"learned from a provider rejection", "context_window", "maximum context length"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if comp.calls != 1 {
		t.Errorf("calls = %d, want 1", comp.calls)
	}
}

// A configured window keeps the old behaviour: one hard-prune retry, then the
// provider's error, and nothing is learned.
func TestRunConfiguredWindowRejectionStillRetriesOnce(t *testing.T) {
	comp := &alwaysLengthRejecting{}
	r := &lineRenderer{}
	a := New(comp, &fakeExec{}, r, "m", "", 10, 100)
	a.ContextWindow = 200_000

	big := strings.Repeat("x", 60_000)
	history := []model.Message{
		{Role: "user", Content: "task"},
		{Role: "tool", ToolCallID: "1", Name: "read_file", Content: big},
		{Role: "tool", ToolCallID: "2", Name: "read_file", Content: big},
	}
	_, err := a.Run(context.Background(), history)
	if err == nil || errors.Is(err, ErrContextTooLarge) {
		t.Fatalf("expected the provider error, got %v", err)
	}
	if !strings.Contains(err.Error(), "maximum context length") {
		t.Errorf("error %q is not the provider's", err)
	}
	if comp.calls != 2 {
		t.Errorf("calls = %d, want 2 (reject, retry once)", comp.calls)
	}
	if got := r.learnedWindows(t); len(got) != 0 {
		t.Errorf("a configured window must not learn; got %v", got)
	}
	for _, l := range r.lines {
		if strings.Contains(l, "learned window") {
			t.Errorf("unexpected learned window line: %q", l)
		}
	}
}
