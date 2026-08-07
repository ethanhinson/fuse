package model

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Default request bounds. A stalled gateway connection must never hang an
// agent turn forever: the transport bounds the wait for response headers,
// RequestTimeout bounds one full attempt (headers + body), and failed
// attempts are retried with backoff up to MaxAttempts.
const (
	defaultRequestTimeout = 5 * time.Minute
	defaultMaxAttempts    = 3
	defaultRetryBackoff   = time.Second
)

// defaultGatewayClient bounds connection setup and the wait for response
// headers. It deliberately has no overall Client.Timeout: long completions
// stream no headers late, so the per-attempt deadline in Complete governs
// total request time instead.
var defaultGatewayClient = &http.Client{
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
	},
}

// RateGate smooths turn-level throughput across the harness (change 0036,
// Acceptance 4). It is consulted by Adapter.Complete exactly once per call —
// before dispatch, not per retry attempt — so N agents in tight turn loops
// cannot outrun a configured requests-per-minute / tokens-per-minute budget.
//
// The gate is defined here, in internal/model, because internal/agent imports
// internal/model (never the reverse): the choke point lives here, so the seam
// must too, with the concrete token bucket injected from cmd/fuse.
//
//   - Wait blocks until the request (and its token estimate) fits the bucket, or
//     ctx is cancelled — Ctrl-C still stops a gated call. provider identifies the
//     rate axis (see the bucket's provider-key mapping); estTokens is what the
//     caller can cheaply predict up front (the adapter passes a conservative
//     len(body)/4 — see Complete) so N concurrent first dispatches reserve ahead
//     and cannot all burst past the tpm cap before any usage is reported.
//   - Report reconciles that same estimate against the actual usage the gateway
//     reported, after a successful response: it charges only the DELTA
//     (in+out − estTokens) so the estimate already charged at Wait is not
//     double-counted. estTokens must be the value passed to the matching Wait;
//     inTokens/outTokens come from CompletionResp.InputTokens/OutputTokens.
//
// A nil gate on the Adapter is the unlimited fast path: Complete makes no gate
// calls and adds zero latency (Acceptance 4's "unset config ⇒ no gate").
type RateGate interface {
	Wait(ctx context.Context, provider string, estTokens int) error
	Report(provider string, estTokens, inTokens, outTokens int)
}

// Adapter is an OpenAI-compatible client for the LiteLLM gateway.
type Adapter struct {
	baseURL string
	key     string
	hc      *http.Client

	// RequestTimeout bounds a single attempt (connect through full body read).
	RequestTimeout time.Duration
	// MaxAttempts is the total number of tries (1 = no retries).
	MaxAttempts int
	// RetryBackoff is the base sleep between attempts (grows linearly).
	RetryBackoff time.Duration

	// gate, when non-nil, smooths dispatch to the configured rpm/tpm budget.
	// nil ⇒ the unlimited fast path (no gate calls, zero added latency). Set via
	// WithRateGate. Consulted once per Complete, before the retry loop.
	gate RateGate

	trace      io.Writer // when non-nil, raw JSON req/resp are written here
	traceLabel string    // identifies the agent in shared trace files
}

// WithRateGate returns a copy of the adapter that consults g before each
// dispatch and reports usage after each success. Following the package's copy-on-
// configure idiom (WithTrace/WithTraceLabel), it does not mutate the receiver, so
// a single base adapter can be shared and re-decorated per agent. A nil g leaves
// the gate off (the fast path).
func (a *Adapter) WithRateGate(g RateGate) *Adapter {
	cp := *a
	cp.gate = g
	return &cp
}

// NewAdapter builds an Adapter. baseURL is the gateway root, e.g.
// "http://localhost:4000/v1". If hc is nil, a shared client with sane
// connection and header timeouts is used.
func NewAdapter(baseURL, key string, hc *http.Client) *Adapter {
	if hc == nil {
		hc = defaultGatewayClient
	}
	return &Adapter{
		baseURL:        strings.TrimRight(baseURL, "/"),
		key:            key,
		hc:             hc,
		RequestTimeout: defaultRequestTimeout,
		MaxAttempts:    defaultMaxAttempts,
		RetryBackoff:   defaultRetryBackoff,
	}
}

// WithTrace returns a copy of the adapter that writes raw JSON request and
// response bodies to w (e.g. os.Stderr). Each request/response pair is
// surrounded by ── REQ ── / ── RESP ── markers.
func (a *Adapter) WithTrace(w io.Writer) *Adapter {
	return a.WithTraceLabel(w, "")
}

// WithTraceLabel is WithTrace with an agent label included in the block
// markers, so concurrent agents sharing one trace file stay attributable.
func (a *Adapter) WithTraceLabel(w io.Writer, label string) *Adapter {
	cp := *a
	cp.trace = w
	cp.traceLabel = label
	return &cp
}

// wire types mirror the OpenAI-compatible JSON payloads.
type wireMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type wireReq struct {
	Model         string             `json:"model"`
	Messages      []wireMessage      `json:"messages"`
	Tools         []wireTool         `json:"tools,omitempty"`
	MaxTokens     int                `json:"max_tokens,omitempty"`
	ToolChoice    string             `json:"tool_choice,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	StreamOptions *wireStreamOptions `json:"stream_options,omitempty"`
}

// wireStreamOptions asks the gateway to include a final usage chunk in the
// stream (OpenAI/LiteLLM convention) so token counts survive streaming.
type wireStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type wireResp struct {
	Choices []struct {
		Message wireMessage `json:"message"`
	} `json:"choices"`
	Usage wireUsage `json:"usage"`
}

// wireStreamChunk is one SSE `data:` event of a streamed chat completion. Each
// carries a partial `delta` for choice 0; a trailing chunk may carry usage, and
// a mid-stream failure arrives as an `error` object instead of a choice.
type wireStreamChunk struct {
	Choices []struct {
		Delta struct {
			Role      string              `json:"role"`
			Content   string              `json:"content"`
			ToolCalls []wireDeltaToolCall `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// wireDeltaToolCall is a streamed tool-call fragment. `index` identifies which
// call it extends; the first fragment for an index carries id+name, and later
// fragments append `arguments`.
type wireDeltaToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func prettyJSON(b []byte) []byte {
	var buf bytes.Buffer
	if err := json.Indent(&buf, b, "", "  "); err != nil {
		return b
	}
	return buf.Bytes()
}

// tracef writes one whole trace block in a single Write so that concurrent
// agents sharing a trace writer never interleave inside a block.
func (a *Adapter) tracef(marker string, body []byte) {
	if a.trace == nil {
		return
	}
	label := ""
	if a.traceLabel != "" {
		label = " [" + a.traceLabel + "]"
	}
	fmt.Fprintf(a.trace, "\n── %s%s ──\n%s\n", marker, label, body)
}

// retryableStatus reports whether an HTTP status is worth retrying.
func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

// Complete sends a completion request and returns the assistant reply.
// Each attempt is bounded by RequestTimeout; transport errors, timeouts,
// 429s, and 5xx responses are retried up to MaxAttempts with backoff.
// Cancellation of ctx aborts immediately, including mid-backoff.
func (a *Adapter) Complete(ctx context.Context, req CompletionReq) (CompletionResp, error) {
	// Stream by default. LiteLLM buffers non-streaming completions and sends no
	// response headers until the whole (possibly minutes-long) generation
	// finishes — which trips the transport's ResponseHeaderTimeout on a big
	// synthesis. Streaming makes the first byte arrive promptly, so the header
	// timeout measures time-to-first-byte while RequestTimeout still bounds the
	// full attempt. We consume the SSE stream and return the assembled result,
	// so Complete's signature and the agent loop are unchanged.
	payload := wireReq{
		Model: req.Model, MaxTokens: req.MaxTokens, ToolChoice: req.ToolChoice,
		Stream: true, StreamOptions: &wireStreamOptions{IncludeUsage: true},
	}
	for _, m := range req.Messages {
		wm := wireMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID, Name: m.Name}
		for _, tc := range m.ToolCalls {
			w := wireToolCall{ID: tc.ID, Type: "function"}
			w.Function.Name = tc.Name
			w.Function.Arguments = tc.Arguments
			wm.ToolCalls = append(wm.ToolCalls, w)
		}
		payload.Messages = append(payload.Messages, wm)
	}
	for _, t := range req.Tools {
		wt := wireTool{Type: "function"}
		wt.Function.Name = t.Name
		wt.Function.Description = t.Description
		wt.Function.Parameters = t.Parameters
		payload.Tools = append(payload.Tools, wt)
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return CompletionResp{}, err
	}
	a.tracef("REQ", prettyJSON(body))

	// Rate gate: consulted ONCE per Complete, before the retry loop. It gates a
	// logical request, not a transport attempt — charging each retry would double-
	// count the request bucket for one turn and let a flaky gateway silently eat an
	// agent's rpm allowance. It also sits at dispatch, so a turn that never gets
	// here (queued upstream by the scheduler) consumes nothing — spec Acceptance 4.
	// nil gate ⇒ fast path: no call, no wait, no allocation.
	//
	// estTokens is a conservative len(body)/4 charged at Wait: the marshalled
	// payload is the one cheap signal the adapter has before dispatch (the gateway
	// tokenizer is authoritative but unavailable here), and ~4 bytes/token is a
	// deliberate under-estimate that still reserves budget so N concurrent first
	// dispatches cannot all burst past the tpm cap before any usage is reported.
	// Report then charges only the delta (actuals − estTokens) so this estimate is
	// not double-counted; a caller passing 0 (unreserved) is charged the full
	// actuals by Report exactly as before.
	estTokens := len(body) / 4
	if a.gate != nil {
		if err := a.gate.Wait(ctx, req.Model, estTokens); err != nil {
			return CompletionResp{}, err
		}
	}

	maxAttempts := a.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	start := time.Now()
	var lastErr error
	attempts := 0
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		attempts = attempt
		resp, err, retryable := a.completeOnce(ctx, body)
		if err == nil {
			// Reconcile the pre-dispatch estimate with the gateway's reported usage so
			// the tpm axis reflects real spend without double-charging the estimate
			// already taken at Wait. Reported once, on the successful attempt.
			if a.gate != nil {
				a.gate.Report(req.Model, estTokens, resp.InputTokens, resp.OutputTokens)
			}
			return resp, nil
		}
		lastErr = err
		// The parent context ending (user cancel, node cancel via the agents
		// overlay) is terminal regardless of what the attempt reported.
		if ctx.Err() != nil {
			return CompletionResp{}, ctx.Err()
		}
		if !retryable || attempt == maxAttempts {
			break
		}
		a.tracef("RETRY", fmt.Appendf(nil, "model=%s attempt %d/%d payload=%dKB elapsed=%s: %v",
			req.Model, attempt, maxAttempts, len(body)>>10, time.Since(start).Round(time.Second), err))
		select {
		case <-time.After(a.RetryBackoff * time.Duration(attempt)):
		case <-ctx.Done():
			return CompletionResp{}, ctx.Err()
		}
	}

	// Enrich the failure with everything needed to respond to it: which
	// model, how big the request was, how many attempts, how long. A bare
	// "timeout awaiting response headers" is undiagnosable after the fact.
	finalErr := fmt.Errorf("model %s: %d attempt(s) failed over %s (payload %dKB, ~%dk tokens est): %w",
		req.Model, attempts, time.Since(start).Round(time.Second), len(body)>>10, len(body)/4/1000, lastErr)
	a.tracef("ERROR", []byte(finalErr.Error()))
	return CompletionResp{}, finalErr
}

// completeOnce performs a single bounded request attempt. retryable reports
// whether the failure class is worth another attempt.
func (a *Adapter) completeOnce(ctx context.Context, body []byte) (_ CompletionResp, err error, retryable bool) {
	attemptCtx := ctx
	if a.RequestTimeout > 0 {
		var cancel context.CancelFunc
		attemptCtx, cancel = context.WithTimeout(ctx, a.RequestTimeout)
		defer cancel()
	}

	httpReq, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, a.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return CompletionResp{}, err, false
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+a.key)

	res, err := a.hc.Do(httpReq)
	if err != nil {
		// Transport errors and per-attempt timeouts are retryable; the caller
		// separately checks the parent ctx for terminal cancellation.
		return CompletionResp{}, err, true
	}
	defer res.Body.Close()

	// Non-2xx: read the (small) error body and classify. This path also covers
	// a gateway that returns an error page before any stream starts.
	if res.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(res.Body)
		a.tracef("RESP", prettyJSON(raw))
		return CompletionResp{},
			fmt.Errorf("gateway status %d: %s", res.StatusCode, strings.TrimSpace(string(raw))),
			retryableStatus(res.StatusCode)
	}

	// Branch on the response shape: a streamed (text/event-stream) body is
	// parsed incrementally; a buffered JSON body (a gateway that ignored
	// stream, or a test double) is parsed whole. Both return the same result.
	if strings.Contains(res.Header.Get("Content-Type"), "text/event-stream") {
		return a.readStream(res.Body)
	}
	return a.readBuffered(res.Body)
}

// readBuffered parses a whole non-streamed chat-completion JSON body.
func (a *Adapter) readBuffered(body io.Reader) (_ CompletionResp, err error, retryable bool) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return CompletionResp{}, fmt.Errorf("read gateway response: %w", err), true
	}
	a.tracef("RESP", prettyJSON(raw))
	var wr wireResp
	if err := json.Unmarshal(raw, &wr); err != nil {
		return CompletionResp{}, fmt.Errorf("decode gateway response: %w", err), false
	}
	if len(wr.Choices) == 0 {
		return CompletionResp{}, fmt.Errorf("gateway returned no choices"), false
	}
	msg := wr.Choices[0].Message
	out := CompletionResp{
		Content:      msg.Content,
		InputTokens:  wr.Usage.PromptTokens,
		OutputTokens: wr.Usage.CompletionTokens,
	}
	for _, tc := range msg.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
	}
	return out, nil, false
}

// readStream consumes an SSE chat-completion stream and reassembles it into a
// single CompletionResp: content deltas are concatenated, tool-call fragments
// are merged by index (first fragment carries id+name, later ones append
// arguments), and the trailing usage chunk supplies token counts. A read error
// mid-stream is retryable (the generation may simply have dropped); a streamed
// `error` event is not (the upstream rejected the request).
func (a *Adapter) readStream(body io.Reader) (_ CompletionResp, err error, retryable bool) {
	var content strings.Builder
	byIndex := map[int]*ToolCall{}
	var order []int
	var usage wireUsage

	sc := bufio.NewScanner(body)
	// Allow long SSE lines (a single delta can carry a large tool-call arg).
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk wireStreamChunk
		if jerr := json.Unmarshal([]byte(data), &chunk); jerr != nil {
			// A malformed chunk is a genuine decode failure, not worth retrying.
			return CompletionResp{}, fmt.Errorf("decode stream chunk: %w", jerr), false
		}
		if chunk.Error != nil {
			return CompletionResp{}, fmt.Errorf("gateway stream error: %s", chunk.Error.Message), false
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		d := chunk.Choices[0].Delta
		content.WriteString(d.Content)
		for _, tc := range d.ToolCalls {
			cur, ok := byIndex[tc.Index]
			if !ok {
				cur = &ToolCall{}
				byIndex[tc.Index] = cur
				order = append(order, tc.Index)
			}
			if tc.ID != "" {
				cur.ID = tc.ID
			}
			if tc.Function.Name != "" {
				cur.Name = tc.Function.Name
			}
			cur.Arguments += tc.Function.Arguments
		}
	}
	if serr := sc.Err(); serr != nil {
		return CompletionResp{}, fmt.Errorf("read gateway stream: %w", serr), true
	}

	out := CompletionResp{
		Content:      content.String(),
		InputTokens:  usage.PromptTokens,
		OutputTokens: usage.CompletionTokens,
	}
	sort.Ints(order)
	for _, idx := range order {
		out.ToolCalls = append(out.ToolCalls, *byIndex[idx])
	}
	// Record a reassembled RESP block so --trace stays useful with streaming on.
	a.traceReassembled(out)
	return out, nil, false
}

// traceReassembled writes a RESP trace block for a streamed response, shaped
// like the buffered JSON so trace output is comparable across both paths.
func (a *Adapter) traceReassembled(out CompletionResp) {
	if a.trace == nil {
		return
	}
	var wr wireResp
	wr.Choices = make([]struct {
		Message wireMessage `json:"message"`
	}, 1)
	wr.Choices[0].Message.Role = "assistant"
	wr.Choices[0].Message.Content = out.Content
	for _, tc := range out.ToolCalls {
		var w wireToolCall
		w.ID, w.Type = tc.ID, "function"
		w.Function.Name, w.Function.Arguments = tc.Name, tc.Arguments
		wr.Choices[0].Message.ToolCalls = append(wr.Choices[0].Message.ToolCalls, w)
	}
	wr.Usage = wireUsage{PromptTokens: out.InputTokens, CompletionTokens: out.OutputTokens}
	if b, jerr := json.Marshal(wr); jerr == nil {
		a.tracef("RESP", prettyJSON(b))
	}
}
