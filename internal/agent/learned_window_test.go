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
		current, prompt, outputCap int
		want                       int
		ok                         bool
	}{
		{0, 0, 0, 0, false},
		{0, 1000, 0, 1000, true},
		// The output cap the provider counted is part of the rejected size.
		{0, 1000, 500, 1500, true},
		{2000, 1000, 500, 1500, true},
		// prompt + cap is not lower than the current window: fall back to the
		// prompt alone, which still lowers it.
		{1500, 1000, 500, 1000, true},
		{1000, 800, 0, 800, true},
		{1000, 1000, 0, 1000, false},
		{1000, 1000, 500, 1000, false},
		{1000, 1200, 0, 1000, false},
	}
	for _, c := range cases {
		got, ok := learnContextWindow(c.current, c.prompt, c.outputCap)
		if got != c.want || ok != c.ok {
			t.Errorf("learnContextWindow(%d, %d, %d) = (%d, %v), want (%d, %v)", c.current, c.prompt, c.outputCap, got, ok, c.want, c.ok)
		}
	}
}

// totalLimitCompleter rejects for length whenever prompt + max_tokens is over
// its limit, the way OpenAI-compatible servers and vLLM count a request. With
// cutOffFirst its first accepted reply is cut off while writing, which raises
// the loop's output cap. Every other accepted reply is "done".
type totalLimitCompleter struct {
	limit       int
	cutOffFirst bool
	calls       int
	rejections  int
	accepted    int
	lastTotal   int
}

func (c *totalLimitCompleter) Complete(_ context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	c.calls++
	in := messagesSize(req.Messages) / bytesPerToken
	if in+req.MaxTokens > c.limit {
		c.rejections++
		return model.CompletionResp{}, fmt.Errorf("gateway status 400: This model's maximum context length is %d tokens. However, you requested %d tokens (%d in the messages, %d in the completion)", c.limit, in+req.MaxTokens, in, req.MaxTokens)
	}
	c.accepted++
	c.lastTotal = in + req.MaxTokens
	if c.cutOffFirst && c.accepted == 1 {
		return model.CompletionResp{Content: "partial", FinishReason: "length", OutputTokens: req.MaxTokens, InputTokens: in}, nil
	}
	return model.CompletionResp{Content: "done", InputTokens: in}, nil
}

// A first-turn rejection caused only by max_tokens: the prompt (~10k tokens)
// fits, prompt + max_tokens (~18k) does not. The loop must learn the size the
// provider counted, shrink only the output cap, and finish, instead of
// learning a window equal to the prompt and aborting on the spot.
func TestRunUnknownWindowCapOnlyOverflowRecovers(t *testing.T) {
	comp := &totalLimitCompleter{limit: 17_500}
	r := &lineRenderer{}
	a := New(comp, &fakeExec{}, r, "m", "", 10, 8000)

	_, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: strings.Repeat("y", 40_000)}})
	if err != nil {
		t.Fatalf("expected recovery by shrinking the output cap, got %v; lines: %q", err, r.lines)
	}
	if comp.calls != 2 || comp.rejections != 1 {
		t.Errorf("calls = %d, rejections = %d; want 2 calls with one rejection", comp.calls, comp.rejections)
	}
	if comp.lastTotal > comp.limit {
		t.Errorf("accepted request ~%d tokens is over the limit %d", comp.lastTotal, comp.limit)
	}
}

// A cut-off reply raises the output cap, and the raised cap pushes prompt + cap
// over what the provider allows although the prompt alone fits easily. The run
// must recover by lowering the cap, without pruning any context.
func TestRunUnknownWindowRaisedCapOverflowRecoversWithoutPruning(t *testing.T) {
	// Prompt ~20k tokens. max_tokens 8000 fits (28k); the raise to 16000 (36k)
	// does not fit the 35k limit.
	comp := &totalLimitCompleter{limit: 35_000, cutOffFirst: true}
	r := &lineRenderer{}
	a := New(comp, &fakeExec{}, r, "m", "", 10, 8000)

	big := strings.Repeat("x", 40_000)
	history := []model.Message{
		{Role: "user", Content: "task"},
		{Role: "tool", ToolCallID: "1", Name: "read_file", Content: big},
		{Role: "tool", ToolCallID: "2", Name: "read_file", Content: big},
	}
	hist, err := a.Run(context.Background(), history)
	if err != nil {
		t.Fatalf("expected recovery, got %v; lines: %q", err, r.lines)
	}
	for i, m := range hist {
		if m.Content == prunedStub {
			t.Errorf("message %d was pruned; the prompt fits, only the output cap was too large", i)
		}
	}
	if comp.rejections != 1 {
		t.Errorf("rejections = %d, want 1; lines: %q", comp.rejections, r.lines)
	}
	if comp.lastTotal > comp.limit {
		t.Errorf("accepted request ~%d tokens is over the limit %d", comp.lastTotal, comp.limit)
	}
	if got := r.learnedWindows(t); len(got) != 1 || got[0] < 35_000 {
		t.Errorf("learned windows = %v, want one at or above the rejected prompt + cap", got)
	}
}

// A max_tokens at or under outputCapFloor cannot be lowered by the clamp, so a
// cap-only retry would resend the rejected request unchanged. The window is
// learned from the prompt alone and the run ends after the one rejection.
func TestRunUnknownWindowCapAtFloorIsNotRetried(t *testing.T) {
	comp := &alwaysLengthRejecting{}
	a := New(comp, &fakeExec{}, &lineRenderer{}, "m", "", 10, 1000)

	_, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: strings.Repeat("y", 4000)}})
	if !errors.Is(err, ErrContextTooLarge) {
		t.Fatalf("expected ErrContextTooLarge, got %v", err)
	}
	if comp.calls != 1 {
		t.Errorf("calls = %d, want 1 (no identical resend)", comp.calls)
	}
}

// cutOffUnderTotalLimit counts prompt + max_tokens like totalLimitCompleter and
// cuts off every reply it accepts. It fails hard after maxCalls so a loop that
// never stops retrying shows up as a test failure, not a hang.
type cutOffUnderTotalLimit struct {
	limit, maxCalls int
	calls           int
}

func (c *cutOffUnderTotalLimit) Complete(_ context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	c.calls++
	if c.calls > c.maxCalls {
		return model.CompletionResp{}, errors.New("test: too many calls")
	}
	in := messagesSize(req.Messages) / bytesPerToken
	if in+req.MaxTokens > c.limit {
		return model.CompletionResp{}, errors.New("gateway status 400: maximum context length exceeded")
	}
	return model.CompletionResp{Content: "partial", FinishReason: "length", OutputTokens: req.MaxTokens, InputTokens: in}, nil
}

// With a learned window, a cut-off reply may only raise the cap within the
// room the request is clamped to. Raising it against the full learned window
// would be clamped straight back on the next pass and counted as a raise, not a
// retry, so the run would retry forever instead of ending ErrOutputTruncated.
func TestRunLearnedWindowCutOffDoesNotRaiseCapPastClamp(t *testing.T) {
	comp := &cutOffUnderTotalLimit{limit: 17_500, maxCalls: 10}
	r := &lineRenderer{}
	a := New(comp, &fakeExec{}, r, "m", "", 10, 8000)

	_, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: strings.Repeat("y", 40_000)}})
	if !errors.Is(err, ErrOutputTruncated) {
		t.Fatalf("expected ErrOutputTruncated, got %v; lines: %q", err, r.lines)
	}
	// One rejection, then the first cut-off reply and maxCutOffRetries retries.
	if want := 2 + maxCutOffRetries; comp.calls != want {
		t.Errorf("calls = %d, want %d; lines: %q", comp.calls, want, r.lines)
	}
}

// droppingTotalLimitCompleter counts prompt + max_tokens like
// totalLimitCompleter, keeps asking for another tool call, and lowers its limit
// to secondLimit once a request after the first rejection is accepted. After
// its second rejection the next accepted reply is "done".
type droppingTotalLimitCompleter struct {
	limit, secondLimit int
	calls, rejections  int
}

func (c *droppingTotalLimitCompleter) Complete(_ context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	c.calls++
	in := messagesSize(req.Messages) / bytesPerToken
	if in+req.MaxTokens > c.limit {
		c.rejections++
		return model.CompletionResp{}, errors.New("gateway status 400: maximum context length exceeded")
	}
	if c.rejections >= 2 {
		return model.CompletionResp{Content: "done", InputTokens: in}, nil
	}
	if c.rejections == 1 {
		c.limit = c.secondLimit
	}
	return model.CompletionResp{
		ToolCalls:   []model.ToolCall{{ID: fmt.Sprintf("c%d", c.calls), Name: "read_file", Arguments: fmt.Sprintf(`{"path":"f%d"}`, c.calls)}},
		InputTokens: in,
	}, nil
}

// A later rejection, after a successful reply, is a new data point: it learns
// prompt + cap again rather than being taken for a repeat of the earlier
// rejection, which would learn the prompt alone and prune context that fits.
//
// Numbers: each tool result is ~1k tokens and max_tokens is 8000. The ~12k
// prompt + 8000 is refused by the 20k limit; the loop learns ~20k and sends
// ~19k. The limit then drops to 18.5k, so the next ~13k prompt + ~6k cap is
// refused; learning ~19k lets the ~18k retry through with nothing pruned.
func TestRunUnknownWindowLaterRejectionCountsCapAgain(t *testing.T) {
	comp := &droppingTotalLimitCompleter{limit: 20_000, secondLimit: 18_500}
	r := &lineRenderer{}
	a := New(comp, bigResultExec{out: strings.Repeat("r", 4000)}, r, "m", "", 50, 8000)

	hist, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "task"}})
	if err != nil {
		t.Fatalf("expected recovery, got %v; lines: %q", err, r.lines)
	}
	if comp.rejections != 2 {
		t.Errorf("rejections = %d, want 2; lines: %q", comp.rejections, r.lines)
	}
	for i, m := range hist {
		if m.Content == prunedStub {
			t.Errorf("message %d was pruned; every prompt fit, only the output cap was too large; lines: %q", i, r.lines)
			break
		}
	}
}

// A provider that counts only the prompt: the first rejection learns prompt +
// cap, the cap-only retry is rejected again with the same prompt, so the loop
// learns from the prompt alone and prunes. One extra request, not a slow walk
// down in 5% steps.
func TestRunUnknownWindowPromptOnlyLimitFallsBackToPrompt(t *testing.T) {
	comp := &lengthRejectingCompleter{limitBytes: 70_000}
	r := &lineRenderer{}
	a := New(comp, &fakeExec{}, r, "m", "", 10, 8000)

	big := strings.Repeat("x", 60_000)
	history := []model.Message{
		{Role: "user", Content: "task"},
		{Role: "tool", ToolCallID: "1", Name: "read_file", Content: big},
		{Role: "tool", ToolCallID: "2", Name: "read_file", Content: big},
	}
	hist, err := a.Run(context.Background(), history)
	if err != nil {
		t.Fatalf("expected recovery, got %v; lines: %q", err, r.lines)
	}
	if comp.calls != 3 {
		t.Errorf("calls = %d, want 3 (reject, cap-only retry rejected, prune and accept); lines: %q", comp.calls, r.lines)
	}
	if hist[1].Content != prunedStub {
		t.Error("older tool result should be pruned once the prompt is known not to fit")
	}
	windows := r.learnedWindows(t)
	if len(windows) != 2 || windows[1] >= windows[0] {
		t.Errorf("learned windows = %v, want two, the second lower", windows)
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
