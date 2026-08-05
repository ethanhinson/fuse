package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
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

	trace      io.Writer // when non-nil, raw JSON req/resp are written here
	traceLabel string    // identifies the agent in shared trace files
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
	Model      string        `json:"model"`
	Messages   []wireMessage `json:"messages"`
	Tools      []wireTool    `json:"tools,omitempty"`
	MaxTokens  int           `json:"max_tokens,omitempty"`
	ToolChoice string        `json:"tool_choice,omitempty"`
}

type wireResp struct {
	Choices []struct {
		Message wireMessage `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
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
	payload := wireReq{Model: req.Model, MaxTokens: req.MaxTokens, ToolChoice: req.ToolChoice}
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
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return CompletionResp{}, fmt.Errorf("read gateway response: %w", err), true
	}
	a.tracef("RESP", prettyJSON(raw))
	if res.StatusCode != http.StatusOK {
		return CompletionResp{},
			fmt.Errorf("gateway status %d: %s", res.StatusCode, strings.TrimSpace(string(raw))),
			retryableStatus(res.StatusCode)
	}

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
