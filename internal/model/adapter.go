package model

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Default request bounds. A stalled gateway connection must never hang an
// agent turn forever: the transport bounds the wait for response headers,
// RequestTimeout bounds one full attempt (headers + body), StreamIdleTimeout
// bounds silence inside a stream, and failed
// attempts are retried with backoff up to MaxAttempts.
const (
	// HelperRequestTimeout bounds a short helper call (permission classifier,
	// relevance scorer, human router). The caller is blocked on a small decision,
	// so unlike a model turn, it has a reason to give up on total time.
	HelperRequestTimeout = 5 * time.Minute
	// listModelsTimeout bounds the catalogue lookup, which is never a long call.
	listModelsTimeout = time.Minute
	// defaultStreamIdleTimeout ends an attempt whose response has gone silent, so
	// a dead provider fails in minutes. There is deliberately no default limit on
	// total time: a reasoning model can legitimately think for a long time, and a
	// response that is still arriving is not stalled. A binding with nobody there
	// to cancel sets RequestTimeout itself.
	defaultStreamIdleTimeout = 10 * time.Minute
	// Six attempts span about half a minute, which outlasts a gateway restart or a
	// short rate-limit window. An unattended run has nobody to press retry.
	defaultMaxAttempts  = 6
	defaultRetryBackoff = time.Second
	maxRetryBackoff     = 30 * time.Second
	// maxRetryAfter caps an honored Retry-After, so a bad header cannot stall a run.
	maxRetryAfter = 2 * time.Minute
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
		TLSHandshakeTimeout: 10 * time.Second,
		// Streaming sends headers promptly, but a busy gateway or provider queue
		// can take minutes to start, and that is not yet a failure.
		ResponseHeaderTimeout: 10 * time.Minute,
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

	// RequestTimeout bounds a single attempt (connect through full body read). 0,
	// the default, sets no limit on total time.
	RequestTimeout time.Duration
	// StreamIdleTimeout ends an attempt when a started response sends nothing for
	// this long. 0 disables it.
	StreamIdleTimeout time.Duration
	// MaxAttempts is the total number of tries (1 = no retries).
	MaxAttempts int
	// RetryBackoff is the base sleep between attempts. It doubles each attempt up to
	// maxRetryBackoff, with jitter so replicas of a failure do not retry in step.
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
		baseURL:           strings.TrimRight(baseURL, "/"),
		key:               key,
		hc:                hc,
		StreamIdleTimeout: defaultStreamIdleTimeout,
		MaxAttempts:       defaultMaxAttempts,
		RetryBackoff:      defaultRetryBackoff,
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
	Role string `json:"role"`
	// Content is deliberately NOT omitempty: OpenAI-compatible gateways
	// (litellm) 400 on messages whose content field is absent — an empty
	// tool result or a pure tool-call assistant turn must serialize as
	// "content": "" rather than dropping the field.
	Content    string         `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
	// Reasoning arrives in one of these depending on the gateway and provider
	// (LiteLLM sends the same text in reasoning_content and reasoning). A
	// replayed message sets only the field its reasoning arrived in.
	ReasoningContent string `json:"reasoning_content,omitempty"`
	Reasoning        string `json:"reasoning,omitempty"`
	ReasoningText    string `json:"reasoning_text,omitempty"`

	// cacheBreakpoint sends Content as one text part marked cache_control, the
	// OpenAI content-part form of an Anthropic prompt-cache breakpoint. Set by
	// a dialect with cacheMarkers on (see markPromptCache); never read from a reply.
	cacheBreakpoint bool
	// images are sent as image_url parts after the text, in content-part form.
	// Set from Message.Attachments; never read from a reply.
	images []Attachment
}

// wireContentPart is one part of a message sent in content-part form.
type wireContentPart struct {
	Type         string            `json:"type"`
	Text         string            `json:"text,omitempty"`
	ImageURL     *wireImageURL     `json:"image_url,omitempty"`
	CacheControl *wireCacheControl `json:"cache_control,omitempty"`
}

type wireImageURL struct {
	URL string `json:"url"`
}

type wireCacheControl struct {
	Type string `json:"type"`
}

func (m *wireMessage) markCacheBreakpoint() { m.cacheBreakpoint = true }

// MarshalJSON sends Content as a string, or in content-part form (a text part,
// then one image part per attachment) when the message is a cache breakpoint or
// carries images. A cache mark goes on the last part, so the cached prefix
// includes the images.
func (m wireMessage) MarshalJSON() ([]byte, error) {
	type plain wireMessage
	if !m.cacheBreakpoint && len(m.images) == 0 {
		return json.Marshal(plain(m))
	}
	var parts []wireContentPart
	if m.Content != "" {
		parts = append(parts, wireContentPart{Type: "text", Text: m.Content})
	}
	for _, im := range m.images {
		url := "data:" + im.MediaType + ";base64," + base64.StdEncoding.EncodeToString(im.Data)
		parts = append(parts, wireContentPart{Type: "image_url", ImageURL: &wireImageURL{URL: url}})
	}
	if m.cacheBreakpoint && len(parts) > 0 {
		parts[len(parts)-1].CacheControl = &wireCacheControl{Type: "ephemeral"}
	}
	return json.Marshal(struct {
		plain
		Content []wireContentPart `json:"content"`
	}{plain(m), parts})
}

// Reasoning wire fields, in the order a reply's reasoning is read from them.
const (
	fieldReasoningContent = "reasoning_content"
	fieldReasoning        = "reasoning"
	fieldReasoningText    = "reasoning_text"
)

// pickReasoning returns the first non-empty reasoning field and its name.
func pickReasoning(content, reasoning, text string) (string, string) {
	switch {
	case content != "":
		return content, fieldReasoningContent
	case reasoning != "":
		return reasoning, fieldReasoning
	case text != "":
		return text, fieldReasoningText
	}
	return "", ""
}

// ReasoningFields lists the wire fields a reply's reasoning can arrive in, in
// the order they are read.
func ReasoningFields() []string {
	return []string{fieldReasoningContent, fieldReasoning, fieldReasoningText}
}

// setReasoning puts text in the wire field named by field (reasoning_content
// when unknown).
func (w *wireMessage) setReasoning(text, field string) {
	switch field {
	case fieldReasoning:
		w.Reasoning = text
	case fieldReasoningText:
		w.ReasoningText = text
	default:
		w.ReasoningContent = text
	}
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
	Model           string             `json:"model"`
	Messages        []wireMessage      `json:"messages"`
	Tools           []wireTool         `json:"tools,omitempty"`
	MaxTokens       int                `json:"max_tokens,omitempty"`
	ReasoningEffort string             `json:"reasoning_effort,omitempty"`
	ToolChoice      string             `json:"tool_choice,omitempty"`
	Stream          bool               `json:"stream,omitempty"`
	StreamOptions   *wireStreamOptions `json:"stream_options,omitempty"`
}

// wireStreamOptions asks the gateway to include a final usage chunk in the
// stream (OpenAI/LiteLLM convention) so token counts survive streaming.
type wireStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	// PromptTokensDetails carries the prefix-cache hit count where the
	// provider reports one (OpenAI-compatible shape, forwarded by LiteLLM and
	// OpenRouter). Omitted on the wire when nil so a buffered reply and a
	// reassembled trace block stay comparable.
	PromptTokensDetails *wirePromptTokensDetails `json:"prompt_tokens_details,omitempty"`
	// CompletionTokensDetails carries the reasoning share of the output where the
	// provider reports one.
	CompletionTokensDetails *wireCompletionTokensDetails `json:"completion_tokens_details,omitempty"`
}

type wireCompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

func (u wireUsage) reasoningTokens() int {
	if u.CompletionTokensDetails == nil {
		return 0
	}
	return u.CompletionTokensDetails.ReasoningTokens
}

type wirePromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
	// CacheWriteTokens is how many prompt tokens were written to the provider's
	// cache on this call (OpenRouter's name for Anthropic's cache creation),
	// billed above the plain input rate.
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

// cachedTokens reads the prefix-cache hit count, 0 when unreported.
func (u wireUsage) cachedTokens() int {
	if u.PromptTokensDetails == nil {
		return 0
	}
	return u.PromptTokensDetails.CachedTokens
}

// cacheWriteTokens reads the cache-write count, 0 when unreported.
func (u wireUsage) cacheWriteTokens() int {
	if u.PromptTokensDetails == nil {
		return 0
	}
	return u.PromptTokensDetails.CacheWriteTokens
}

type wireChoice struct {
	Message      wireMessage `json:"message"`
	FinishReason string      `json:"finish_reason,omitempty"`
}

type wireResp struct {
	Choices []wireChoice `json:"choices"`
	Usage   wireUsage    `json:"usage"`
}

// wireStreamChunk is one SSE `data:` event of a streamed chat completion. Each
// carries a partial `delta` for choice 0; a trailing chunk may carry usage, and
// a mid-stream failure arrives as an `error` object instead of a choice.
type wireStreamChunk struct {
	Choices []struct {
		Delta struct {
			Role             string              `json:"role"`
			Content          string              `json:"content"`
			ReasoningContent string              `json:"reasoning_content"`
			Reasoning        string              `json:"reasoning"`
			ReasoningText    string              `json:"reasoning_text"`
			ToolCalls        []wireDeltaToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		// Code is a string ("500") from LiteLLM and a number from some
		// upstreams; it is read leniently.
		Code any `json:"code"`
	} `json:"error"`
}

// retryableStreamError classifies an error object that arrived as an SSE
// data event. LiteLLM wraps everything that goes wrong after the headers were
// sent in this one shape, so it covers both a genuine rejection (bad request,
// auth, context too long: pointless to retry) and a transient failure (the
// provider dropped the connection mid-generation, a 5xx, a rate limit: worth
// another attempt). A 4xx code other than 408/429, or an exception class that
// names a client-side fault, is terminal; everything else is retried.
func retryableStreamError(typ string, code any, message string) bool {
	if n, ok := errorCode(code); ok && n >= 400 && n < 500 && n != http.StatusRequestTimeout && n != http.StatusTooManyRequests {
		return false
	}
	head := typ + " " + message
	if len(head) > 200 {
		head = head[:200]
	}
	for _, terminal := range []string{
		"invalid_request", "BadRequest", "Authentication", "PermissionDenied",
		"NotFound", "ContextWindowExceeded", "ContentPolicy", "UnsupportedParams",
	} {
		if strings.Contains(head, terminal) {
			return false
		}
	}
	return true
}

// errorCode reads a numeric HTTP-style code out of the lenient Code field.
func errorCode(code any) (int, bool) {
	switch v := code.(type) {
	case float64:
		return int(v), true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		return n, err == nil
	}
	return 0, false
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
	d := resolveDialect(req.Dialect, req.Model, req.DialectOptions)
	payload := wireReq{
		Model: req.Model, MaxTokens: req.MaxTokens, ToolChoice: req.ToolChoice, ReasoningEffort: req.ReasoningEffort,
		Stream: true, StreamOptions: &wireStreamOptions{IncludeUsage: true},
	}
	var nImages, b64Bytes int
	names := newWireNames(req.Tools, req.Messages)
	ts := toolSet{schemas: req.Tools, names: names}
	for _, m := range req.Messages {
		wm := wireMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		if m.Name != "" {
			wm.Name = names.wire(m.Name)
		}
		for _, att := range m.Attachments {
			switch {
			case req.Attachments && len(att.Data) > 0:
				wm.images = append(wm.images, att)
				nImages++
				b64Bytes += base64.StdEncoding.EncodedLen(len(att.Data))
			case !req.Attachments:
				wm.Content += "\n[image " + att.Label() + " not sent: this model does not accept attachments]"
			default:
				wm.Content += "\n[image " + att.Label() + " not available]"
			}
		}
		if req.ReplayReasoning && m.Role == "assistant" && m.Reasoning != "" {
			wm.setReasoning(m.Reasoning, d.replayField(m.ReasoningField))
		}
		for _, tc := range m.ToolCalls {
			w := wireToolCall{ID: tc.ID, Type: "function"}
			w.Function.Name = names.wire(tc.Name)
			// The OpenAI wire format requires arguments to be a JSON string;
			// some models emit an empty string, which strict backends (LM
			// Studio chat templates) reject with a 500 when the conversation
			// is replayed. Normalize to the empty object.
			if strings.TrimSpace(tc.Arguments) == "" {
				w.Function.Arguments = "{}"
			} else {
				w.Function.Arguments = tc.Arguments
			}
			wm.ToolCalls = append(wm.ToolCalls, w)
		}
		payload.Messages = append(payload.Messages, wm)
	}
	for _, t := range req.Tools {
		wt := wireTool{Type: "function"}
		wt.Function.Name = names.wire(t.Name)
		wt.Function.Description = t.Description
		wt.Function.Parameters = t.Parameters
		payload.Tools = append(payload.Tools, wt)
	}
	// The model family's additions (cache marks when its capabilities ask for
	// them, then its prepare hook); see dialect.go.
	d.prepareRequest(&payload)

	body, err := json.Marshal(payload)
	if err != nil {
		return CompletionResp{}, err
	}
	a.tracef("REQ", prettyJSON(redactDataURLs(body)))

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
	// Image bytes are excluded and each image costs AttachmentTokenEstimate
	// instead, or a 1 MB image would reserve about 330k tokens.
	estTokens := (len(body)-b64Bytes)/4 + nImages*AttachmentTokenEstimate
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
		resp, err, retryable := a.completeOnce(ctx, body, ts, d)
		if err == nil && d.caps.retryEmptyUnstreamed && replyEmpty(resp) {
			resp = a.completeUnstreamed(ctx, payload, ts, d, resp)
		}
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
		wait := retryDelay(a.RetryBackoff, attempt)
		var limited *retryAfterError
		if errors.As(err, &limited) && limited.after > wait {
			wait = limited.after
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return CompletionResp{}, ctx.Err()
		}
	}

	// Enrich the failure with everything needed to respond to it: which
	// model, how big the request was, how many attempts, how long. A bare
	// "timeout awaiting response headers" is undiagnosable after the fact.
	finalErr := fmt.Errorf("model %s: %d attempt(s) failed over %s (payload %dKB, ~%dk tokens est): %w",
		req.Model, attempts, time.Since(start).Round(time.Second), len(body)>>10, estTokens/1000, lastErr)
	a.tracef("ERROR", []byte(finalErr.Error()))
	return CompletionResp{}, finalErr
}

var dataURLRe = regexp.MustCompile(`data:(image/[A-Za-z0-9.+-]+);base64,([A-Za-z0-9+/=]+)`)

// redactDataURLs replaces each base64 data URL in a request body with a marker
// of its size, so traces never hold image bytes. The body sent is unchanged.
func redactDataURLs(b []byte) []byte {
	return dataURLRe.ReplaceAllFunc(b, func(m []byte) []byte {
		sub := dataURLRe.FindSubmatch(m)
		return fmt.Appendf(nil, "data:%s;base64,<elided %d bytes>", sub[1], len(sub[2]))
	})
}

// retryDelay is the jittered wait before the retry after attempt: half the doubled
// backoff is fixed and half random.
func retryDelay(base time.Duration, attempt int) time.Duration {
	if base <= 0 {
		return 0
	}
	d := base
	for i := 1; i < attempt && d < maxRetryBackoff; i++ {
		d *= 2
	}
	if d > maxRetryBackoff {
		d = maxRetryBackoff
	}
	return d/2 + rand.N(d/2+1)
}

// retryAfterError carries the wait a server asked for alongside its error.
type retryAfterError struct {
	after time.Duration
	err   error
}

func (e *retryAfterError) Error() string { return e.err.Error() }
func (e *retryAfterError) Unwrap() error { return e.err }

// parseRetryAfter reads a Retry-After header in seconds or as an HTTP date, capped
// at maxRetryAfter. Anything unusable or already past returns 0.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	var d time.Duration
	if secs, err := strconv.Atoi(v); err == nil {
		d = time.Duration(secs) * time.Second
	} else if at, err := http.ParseTime(v); err == nil {
		d = at.Sub(now)
	}
	if d <= 0 {
		return 0
	}
	if d > maxRetryAfter {
		return maxRetryAfter
	}
	return d
}

// replyEmpty reports a reply with nothing the agent can use. A reply cut off at the
// output cap is excluded: the agent loop raises the cap for that, and asking again at
// the same cap would only repeat it.
func replyEmpty(r CompletionResp) bool {
	return strings.TrimSpace(r.Content) == "" && len(r.ToolCalls) == 0 && r.FinishReason != "length"
}

// completeUnstreamed asks once more without streaming after a streamed reply came
// back empty. Some gateway routes end a reasoning stream early with a normal stop
// and [DONE] while the same request answers fully when buffered, so the empty stream
// says nothing about the model. It returns the original reply if this fails too, and
// the agent loop decides what to do about an empty reply.
func (a *Adapter) completeUnstreamed(ctx context.Context, payload wireReq, tools toolSet, d resolvedDialect, empty CompletionResp) CompletionResp {
	payload.Stream = false
	payload.StreamOptions = nil
	body, err := json.Marshal(payload)
	if err != nil {
		return empty
	}
	a.tracef("RETRY", []byte("empty streamed reply; asking again without streaming"))
	a.tracef("REQ", prettyJSON(redactDataURLs(body)))
	resp, err, _ := a.completeOnce(ctx, body, tools, d)
	if err != nil {
		a.tracef("ERROR", []byte(err.Error()))
		return empty
	}
	return resp
}

// completeOnce performs a single bounded request attempt. retryable reports
// whether the failure class is worth another attempt.
func (a *Adapter) completeOnce(ctx context.Context, body []byte, tools toolSet, d resolvedDialect) (_ CompletionResp, err error, retryable bool) {
	attemptCtx, cancelAttempt := context.WithCancel(ctx)
	defer cancelAttempt()
	if a.RequestTimeout > 0 {
		var cancel context.CancelFunc
		attemptCtx, cancel = context.WithTimeout(attemptCtx, a.RequestTimeout)
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
		statusErr := fmt.Errorf("gateway status %d: %s", res.StatusCode, strings.TrimSpace(string(raw)))
		if after := parseRetryAfter(res.Header.Get("Retry-After"), time.Now()); after > 0 {
			statusErr = &retryAfterError{after: after, err: statusErr}
		}
		return CompletionResp{}, statusErr, retryableStatus(res.StatusCode)
	}

	// Branch on the response shape: a streamed (text/event-stream) body is
	// parsed incrementally; a buffered JSON body (a gateway that ignored
	// stream, or a test double) is parsed whole. Both return the same result.
	var respBody io.Reader = res.Body
	var stalled atomic.Bool
	if a.StreamIdleTimeout > 0 {
		watchdog := time.AfterFunc(a.StreamIdleTimeout, func() {
			stalled.Store(true)
			cancelAttempt()
		})
		defer watchdog.Stop()
		respBody = idleResetReader{r: res.Body, watchdog: watchdog, idle: a.StreamIdleTimeout}
	}
	var out CompletionResp
	if strings.Contains(res.Header.Get("Content-Type"), "text/event-stream") {
		out, err, retryable = a.readStream(respBody, tools, d)
	} else {
		out, err, retryable = a.readBuffered(respBody, tools, d)
	}
	if err != nil && stalled.Load() {
		return CompletionResp{}, fmt.Errorf("gateway response silent for %s: %w", a.StreamIdleTimeout, err), true
	}
	return out, err, retryable
}

// idleResetReader pushes the stream's idle watchdog back whenever bytes arrive.
type idleResetReader struct {
	r        io.Reader
	watchdog *time.Timer
	idle     time.Duration
}

func (i idleResetReader) Read(p []byte) (int, error) {
	n, err := i.r.Read(p)
	if n > 0 {
		i.watchdog.Reset(i.idle)
	}
	return n, err
}

// readBuffered parses a whole non-streamed chat-completion JSON body.
func (a *Adapter) readBuffered(body io.Reader, tools toolSet, d resolvedDialect) (_ CompletionResp, err error, retryable bool) {
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
	reasoning, field := pickReasoning(msg.ReasoningContent, msg.Reasoning, msg.ReasoningText)
	out := CompletionResp{
		Content:         msg.Content,
		Reasoning:       reasoning,
		ReasoningField:  field,
		InputTokens:     wr.Usage.PromptTokens,
		CachedTokens:    wr.Usage.cachedTokens(),
		CacheWrite:      wr.Usage.cacheWriteTokens(),
		OutputTokens:    wr.Usage.CompletionTokens,
		ReasoningTokens: wr.Usage.reasoningTokens(),
		FinishReason:    wr.Choices[0].FinishReason,
	}
	for _, tc := range msg.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
	}
	return a.finishReply(out, tools, d), nil, false
}

// toolSet is the request's advertised tools plus its wire-name mapping, handed
// to the reply readers so they decode with the exact mapping the request used.
type toolSet struct {
	schemas []ToolSchema
	names   *wireNames
}

// finishReply applies the dialect's reply steps: XML tool-call recovery when
// on, then the family's finish hook.
func (a *Adapter) finishReply(out CompletionResp, tools toolSet, d resolvedDialect) CompletionResp {
	// The model saw provider-safe wire names; map them back to fuse's names so
	// dispatch is unchanged. XML recovery runs on wire names (that is what the
	// model wrote in text) and its calls are decoded after.
	names := tools.names
	names.decode(out.ToolCalls)
	if d.caps.recoverToolCalls {
		hadCalls := len(out.ToolCalls) > 0
		out = a.recoverToolCalls(out, names.wireSchemas(tools.schemas))
		if !hadCalls {
			names.decode(out.ToolCalls)
		}
	}
	if d.finish != nil {
		d.finish(&out, tools.schemas, d.caps)
	}
	return out
}

// recoverToolCalls applies the Qwen-style XML fallback (see recover.go) to a
// reply that carried no structured tool call. Both reader paths funnel through
// it so a leaked call is lifted the same way whether the gateway streamed or
// buffered. A recovery is recorded in the trace so a run's transcript shows
// that the harness, not the provider, produced the structured call.
func (a *Adapter) recoverToolCalls(out CompletionResp, tools []ToolSchema) CompletionResp {
	if len(out.ToolCalls) > 0 {
		return out
	}
	content, calls, ok := recoverXMLToolCalls(out.Content, tools)
	if !ok {
		return out
	}
	out.Content, out.ToolCalls = content, calls
	a.traceReassembled("RECOVERED", out)
	return out
}

// Bounds for the runaway-reply watchdog in readStream. A model can get stuck
// emitting one short unit over and over until the output cap, and nothing else
// notices: the reply keeps arriving (no idle timeout) and finishes "cleanly".
const (
	// repeatMinTailBytes is how much of the reply's tail must be one repeated
	// unit before it counts as degenerate. Below it, ordinary repetitive
	// prose, markdown tables, and numbered lists stay safe.
	repeatMinTailBytes = 2048
	// repeatMaxUnitBytes caps the repeated unit. Longer units are
	// indistinguishable from structured prose at this cost, so they are not
	// watched.
	repeatMaxUnitBytes = 256
	// repeatCheckStride is how much new visible text must arrive before the
	// check re-runs, so a long healthy reply pays a bounded scan per KB of
	// content, not per chunk.
	repeatCheckStride = 1024
)

// repeatsAtTail reports whether s ends in degenerate repetition: its last
// repeatMinTailBytes are one short unit (1..repeatMaxUnitBytes bytes) repeated
// back to back. Only the tail is examined — a reply may open with normal prose
// and then run away, so the head is irrelevant. Near-misses (a table whose
// rows share a shape, a numbered list, log lines differing by timestamp) are
// not pure repeats and return false.
func repeatsAtTail(s string) bool {
	n := len(s)
	if n < repeatMinTailBytes+2 {
		return false
	}
	maxU := repeatMaxUnitBytes
	if n-repeatMinTailBytes < maxU {
		maxU = n - repeatMinTailBytes
	}
	for u := 1; u <= maxU; u++ {
		// A period-u tail means s[i] == s[i-u] for every i in the region.
		run := 0
		for i := n - 1; i >= u && s[i] == s[i-u]; i-- {
			run++
		}
		if run >= repeatMinTailBytes {
			return true
		}
	}
	return false
}

// readStream consumes an SSE chat-completion stream and reassembles it into a
// single CompletionResp: content deltas are concatenated, tool-call fragments
// are merged by index (first fragment carries id+name, later ones append
// arguments), and the trailing usage chunk supplies token counts. A read error
// mid-stream is retryable (the generation may simply have dropped); a streamed
// `error` event is retryable unless it reports a client-side rejection (see
// retryableStreamError). A reply degenerating into repetition is cut off early
// and retried: returning here unwinds completeOnce, whose deferred cancel
// aborts the request so the provider stops generating.
func (a *Adapter) readStream(body io.Reader, tools toolSet, d resolvedDialect) (_ CompletionResp, err error, retryable bool) {
	var content strings.Builder
	// Each reasoning field is collected separately: a gateway may send the same
	// text in several, and only one is kept.
	var rContent, rReasoning, rText strings.Builder
	byIndex := map[int]*ToolCall{}
	var order []int
	var finishReason string
	var usage wireUsage
	sawDone := false
	nextRepeatCheck := repeatMinTailBytes + 2

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
			sawDone = true
			break
		}
		var chunk wireStreamChunk
		if jerr := json.Unmarshal([]byte(data), &chunk); jerr != nil {
			// A malformed chunk is a genuine decode failure, not worth retrying.
			return CompletionResp{}, fmt.Errorf("decode stream chunk: %w", jerr), false
		}
		if chunk.Error != nil {
			return CompletionResp{}, fmt.Errorf("gateway stream error: %s", chunk.Error.Message),
				retryableStreamError(chunk.Error.Type, chunk.Error.Code, chunk.Error.Message)
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		if fr := chunk.Choices[0].FinishReason; fr != "" {
			finishReason = fr
		}
		d := chunk.Choices[0].Delta
		content.WriteString(d.Content)
		rContent.WriteString(d.ReasoningContent)
		rReasoning.WriteString(d.Reasoning)
		rText.WriteString(d.ReasoningText)
		// Watch only the visible reply for degenerate repetition: tool-call
		// arguments can legitimately hold a repetitive file, and reasoning is
		// not shown to the caller, so neither is checked.
		if d.Content != "" && content.Len() >= nextRepeatCheck {
			nextRepeatCheck = content.Len() + repeatCheckStride
			if repeatsAtTail(content.String()) {
				return CompletionResp{}, fmt.Errorf(
					"model reply degenerated into repetition after %d bytes of content", content.Len()), true
			}
		}
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
	// A body that closes cleanly with neither terminator is a lost response. Taking
	// it as complete would make half an answer the run's final, successful output.
	if !sawDone && finishReason == "" {
		return CompletionResp{}, errors.New("gateway stream ended before the reply finished"), true
	}

	reasoning, field := pickReasoning(rContent.String(), rReasoning.String(), rText.String())
	out := CompletionResp{
		Content:         content.String(),
		Reasoning:       reasoning,
		ReasoningField:  field,
		InputTokens:     usage.PromptTokens,
		CachedTokens:    usage.cachedTokens(),
		CacheWrite:      usage.cacheWriteTokens(),
		OutputTokens:    usage.CompletionTokens,
		ReasoningTokens: usage.reasoningTokens(),
		FinishReason:    finishReason,
	}
	sort.Ints(order)
	for _, idx := range order {
		out.ToolCalls = append(out.ToolCalls, *byIndex[idx])
	}
	// Record a reassembled RESP block so --trace stays useful with streaming on.
	a.traceReassembled("RESP", out)
	return a.finishReply(out, tools, d), nil, false
}

// traceReassembled writes a trace block under marker for an assembled
// response, shaped like the buffered JSON so trace output is comparable across
// both paths ("RESP" for a streamed reply, "RECOVERED" after the XML fallback).
//
// RESP and RECOVERED blocks show wire (provider-safe) tool names, not fuse's
// internal names: they are traced before finishReply decodes them.
func (a *Adapter) traceReassembled(marker string, out CompletionResp) {
	if a.trace == nil {
		return
	}
	var wr wireResp
	wr.Choices = make([]wireChoice, 1)
	wr.Choices[0].Message.Role = "assistant"
	wr.Choices[0].Message.Content = out.Content
	wr.Choices[0].Message.setReasoning(out.Reasoning, out.ReasoningField)
	wr.Choices[0].FinishReason = out.FinishReason
	for _, tc := range out.ToolCalls {
		var w wireToolCall
		w.ID, w.Type = tc.ID, "function"
		w.Function.Name, w.Function.Arguments = tc.Name, tc.Arguments
		wr.Choices[0].Message.ToolCalls = append(wr.Choices[0].Message.ToolCalls, w)
	}
	wr.Usage = wireUsage{PromptTokens: out.InputTokens, CompletionTokens: out.OutputTokens}
	if out.CachedTokens > 0 || out.CacheWrite > 0 {
		wr.Usage.PromptTokensDetails = &wirePromptTokensDetails{CachedTokens: out.CachedTokens, CacheWriteTokens: out.CacheWrite}
	}
	if out.ReasoningTokens > 0 {
		wr.Usage.CompletionTokensDetails = &wireCompletionTokensDetails{ReasoningTokens: out.ReasoningTokens}
	}
	if b, jerr := json.Marshal(wr); jerr == nil {
		a.tracef(marker, prettyJSON(b))
	}
}

// ListModels asks the gateway which model IDs it serves (GET {base}/models, the
// OpenAI-compatible listing LiteLLM exposes). IDs come back sorted and
// de-duplicated. A single attempt bounded by listModelsTimeout: this is an
// interactive lookup, so a failure is reported rather than retried.
func (a *Adapter) ListModels(ctx context.Context) ([]string, error) {
	if a.baseURL == "" {
		return nil, errors.New("no gateway configured")
	}
	limit := a.RequestTimeout
	if limit <= 0 || limit > listModelsTimeout {
		limit = listModelsTimeout
	}
	{
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, limit)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.key)
	res, err := a.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gateway status %d: %s", res.StatusCode, strings.TrimSpace(string(raw)))
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("decode model list: %w", err)
	}
	seen := map[string]bool{}
	var ids []string
	for _, d := range body.Data {
		// Wildcard routes (openrouter/*) are patterns, not models.
		if d.ID != "" && !strings.Contains(d.ID, "*") && !seen[d.ID] {
			seen[d.ID] = true
			ids = append(ids, d.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}
