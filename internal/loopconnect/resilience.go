package loopconnect

import (
	"context"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
)

// ForwardedScheme derives the EXTERNAL request scheme, honoring a reverse proxy /
// gateway that terminated TLS. It reads X-Forwarded-Proto first (a proxy in front of
// this Connect server sets it), falling back to the direct-connection TLS state only
// when no forwarded header is present. It NEVER hard-assumes the server's own TLS, so
// a plaintext hop behind an HTTPS-terminating gateway is correctly reported as https.
//
// directTLS is whether THIS server saw the connection over TLS (r.TLS != nil).
func ForwardedScheme(h http.Header, directTLS bool) string {
	if v := h.Get("X-Forwarded-Proto"); v != "" {
		// A comma-separated list (proxy chain) — the left-most is the original client.
		if i := strings.IndexByte(v, ','); i >= 0 {
			v = v[:i]
		}
		if s := strings.TrimSpace(v); s != "" {
			return strings.ToLower(s)
		}
	}
	if directTLS {
		return "https"
	}
	return "http"
}

// RetryUnary runs a unary Connect call with up to maxAttempts attempts, retrying on
// retryable Connect codes (Unavailable, ResourceExhausted, DeadlineExceeded — a proxy
// hiccup, a rate limit, or a deadline crossing an intermediary). The caller supplies
// a `call` closure that REBUILDS its request each attempt: this is the rewind — an
// http request body is a one-shot io.Reader, so re-sending the same *connect.Request
// after a failed attempt would send an empty body through an intermediary
// (rewind-request-body-on-manual-retry). Rebuilding via connect.NewRequest per attempt
// gives a fresh, full body every time.
//
// It is deadline-safe: it stops retrying when ctx is done and returns ctx.Err().
func RetryUnary[T any](ctx context.Context, maxAttempts int, call func(ctx context.Context) (*connect.Response[T], error)) (*connect.Response[T], error) {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		resp, err := call(ctx) // fresh request (rewound body) each attempt.
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !retryableCode(connect.CodeOf(err)) {
			return nil, err
		}
		// Backoff before the next attempt, but never past the deadline.
		if attempt+1 < maxAttempts {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff(attempt)):
			}
		}
	}
	return nil, lastErr
}

// retryableCode reports whether a Connect error code is worth retrying.
func retryableCode(c connect.Code) bool {
	switch c {
	case connect.CodeUnavailable, connect.CodeResourceExhausted, connect.CodeDeadlineExceeded:
		return true
	default:
		return false
	}
}

// backoff is a short capped exponential backoff (25ms, 50ms, 100ms, … capped at 1s).
func backoff(attempt int) time.Duration {
	d := 25 * time.Millisecond << attempt
	if d > time.Second {
		d = time.Second
	}
	return d
}
