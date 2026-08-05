package research

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

// HTTP bounds mirror the discipline in internal/model/adapter.go: a stalled
// search or fetch must never hang an agent turn forever. RequestTimeout bounds
// one full attempt (headers + body), failed attempts are retried with backoff
// up to MaxAttempts, and a server-supplied Retry-After is honored but capped.
const (
	defaultRequestTimeout = 10 * time.Second
	defaultMaxAttempts    = 3
	defaultRetryBackoff   = time.Second
	defaultMaxRetryAfter  = 30 * time.Second
)

// defaultResearchClient bounds connection setup and the wait for response
// headers. The per-attempt deadline in Do governs total request time; this
// client deliberately sets no overall Client.Timeout of its own.
var defaultResearchClient = &http.Client{
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
	},
}

// Client is a bounded, retrying HTTP doer shared by the search providers and
// the scraper. It is safe to construct once per provider and reuse; the base
// *http.Client is injectable so tests can pass httptest's client.
type Client struct {
	name string
	hc   *http.Client

	// RequestTimeout bounds a single attempt (connect through full body read).
	RequestTimeout time.Duration
	// MaxAttempts is the total number of tries (1 = no retries).
	MaxAttempts int
	// RetryBackoff is the base sleep between attempts (grows linearly).
	RetryBackoff time.Duration
	// MaxRetryAfter caps how long an honored Retry-After header may make us
	// wait, so a hostile or misconfigured server cannot stall a turn.
	MaxRetryAfter time.Duration

	trace      io.Writer
	traceLabel string
}

// NewClient builds a Client labeled with name (used in trace output and error
// context). If hc is nil, a shared client with sane connection and header
// timeouts is used.
func NewClient(name string, hc *http.Client) *Client {
	if hc == nil {
		hc = defaultResearchClient
	}
	return &Client{
		name:           name,
		hc:             hc,
		RequestTimeout: defaultRequestTimeout,
		MaxAttempts:    defaultMaxAttempts,
		RetryBackoff:   defaultRetryBackoff,
		MaxRetryAfter:  defaultMaxRetryAfter,
	}
}

// WithTrace returns a copy of the Client that writes labeled trace lines to w.
// When w is shared across goroutines it must guard its own writes (each trace
// line is emitted in a single Write so lines never interleave).
func (c *Client) WithTrace(w io.Writer) *Client {
	return c.WithTraceLabel(w, c.name)
}

// WithTraceLabel is WithTrace with an explicit label, so concurrent providers
// sharing one trace writer stay attributable.
func (c *Client) WithTraceLabel(w io.Writer, label string) *Client {
	cp := *c
	cp.trace = w
	cp.traceLabel = label
	return &cp
}

func (c *Client) tracef(format string, args ...any) {
	if c.trace == nil {
		return
	}
	label := c.traceLabel
	if label == "" {
		label = c.name
	}
	fmt.Fprintf(c.trace, "── research [%s] ── %s\n", label, fmt.Sprintf(format, args...))
}

// retryableStatus reports whether an HTTP status is worth retrying.
func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

// Do performs req with per-attempt timeouts and bounded retries on transport
// errors, 429, and 5xx. On a retryable status it honors a Retry-After header
// (delta-seconds or HTTP-date, capped by MaxRetryAfter); otherwise it backs off
// linearly. Cancellation of req's context aborts immediately, including
// mid-backoff. On success the returned *http.Response's body is the caller's to
// close. Failures carry provider/attempt/status/duration context.
//
// req must be repeatable: for retries to be safe it should have no body or a
// GetBody set (as http.NewRequestWithContext does for byte/string bodies).
func (c *Client) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	maxAttempts := c.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	start := time.Now()
	var lastErr error
	var lastStatus int
	attempts := 0

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		attempts = attempt

		resp, err, retryable, retryAfter := c.doOnce(ctx, req)
		if err == nil && resp != nil && resp.StatusCode < 400 {
			return resp, nil
		}

		// A cancelled/deadline-exceeded parent context is terminal regardless of
		// what the attempt reported.
		if ctx.Err() != nil {
			if resp != nil {
				resp.Body.Close()
			}
			return nil, fmt.Errorf("%s: request cancelled after %d attempt(s) over %s: %w",
				c.name, attempt, time.Since(start).Round(time.Millisecond), ctx.Err())
		}

		if err != nil {
			lastErr = err
			lastStatus = 0
		} else {
			lastStatus = resp.StatusCode
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
			// A non-retryable, non-2xx/3xx response is returned to the caller as
			// a success at the HTTP layer: the caller inspects the status. Only
			// retryable statuses drive the retry loop; everything else returns.
			if !retryable {
				return resp, nil
			}
			resp.Body.Close()
		}

		if !retryable || attempt == maxAttempts {
			break
		}

		wait := c.RetryBackoff * time.Duration(attempt)
		if retryAfter > 0 {
			if retryAfter > c.MaxRetryAfter {
				retryAfter = c.MaxRetryAfter
			}
			if retryAfter > wait {
				wait = retryAfter
			}
		}
		c.tracef("retry attempt %d/%d status=%d wait=%s elapsed=%s: %v",
			attempt, maxAttempts, lastStatus, wait.Round(time.Millisecond),
			time.Since(start).Round(time.Millisecond), lastErr)

		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("%s: request cancelled after %d attempt(s) over %s: %w",
				c.name, attempt, time.Since(start).Round(time.Millisecond), ctx.Err())
		}
	}

	statusNote := ""
	if lastStatus != 0 {
		statusNote = fmt.Sprintf(" (last status %d)", lastStatus)
	}
	return nil, fmt.Errorf("%s: %d attempt(s) failed over %s%s: %w",
		c.name, attempts, time.Since(start).Round(time.Millisecond), statusNote, lastErr)
}

// doOnce performs a single bounded request attempt. It returns the response
// (whose body the caller closes on success), a transport error if any, whether
// the failure class is worth retrying, and a parsed Retry-After delay (0 if
// absent/unparseable).
//
// The per-attempt RequestTimeout bounds the whole attempt including the
// caller's body read. On a non-retryable success we hand the live body back
// with its cancel func chained to Body.Close, so the deadline stays armed until
// the caller finishes reading without leaking the context's timer.
func (c *Client) doOnce(ctx context.Context, req *http.Request) (*http.Response, error, bool, time.Duration) {
	attemptCtx := ctx
	cancel := context.CancelFunc(func() {})
	if c.RequestTimeout > 0 {
		attemptCtx, cancel = context.WithTimeout(ctx, c.RequestTimeout)
	}
	req = req.Clone(attemptCtx)

	// Rewind the body for each attempt. Clone shares the same drained
	// io.ReadCloser, and http.Client.Do only auto-invokes GetBody for its own
	// redirect handling — never for this caller-driven retry loop. Without this
	// reset a retried POST (e.g. Tavily) sends an empty body on attempt 2+.
	if req.GetBody != nil {
		body, gerr := req.GetBody()
		if gerr != nil {
			cancel()
			return nil, gerr, false, 0
		}
		req.Body = body
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		cancel()
		// Transport errors and per-attempt timeouts are retryable; the caller
		// separately checks the parent ctx for terminal cancellation.
		return nil, err, true, 0
	}
	if retryableStatus(resp.StatusCode) {
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		resp.Body.Close()
		cancel()
		return resp, nil, true, retryAfter
	}
	// Non-retryable: hand the live response back, keeping the attempt deadline
	// armed for the caller's body read. Closing the body releases the timer.
	resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil, false, 0
}

// cancelBody chains a context.CancelFunc to a response body's Close, so the
// per-attempt deadline is released exactly when the caller is done reading.
type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// parseRetryAfter interprets a Retry-After header value in either supported
// form: delta-seconds (e.g. "120") or an HTTP-date (RFC 1123). It returns the
// delay relative to now, clamped to >= 0. An empty or unparseable value yields
// zero, letting the caller fall back to its own backoff.
func parseRetryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		// Compute against the supplied now for testability.
		if d := t.Sub(now); d > 0 {
			return d
		}
		return 0
	}
	return 0
}
