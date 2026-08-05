package research

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// traceBuf is a mutex-guarded io.Writer test double. Both the write path and
// the read/inspection path take the same lock: a lock on one side only is
// still a race under -race (mutex-test-double-concurrent-provider).
type traceBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (t *traceBuf) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.Write(p)
}

func (t *traceBuf) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.String()
}

func newTestClient() *Client {
	c := NewClient("test", nil)
	c.RetryBackoff = time.Millisecond // keep unit tests fast
	return c
}

func mustReq(t *testing.T, ctx context.Context, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	return req
}

func TestDoSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok body")
	}))
	defer srv.Close()

	c := newTestClient()
	resp, err := c.Do(context.Background(), mustReq(t, context.Background(), srv.URL))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "ok body" {
		t.Errorf("body = %q, want %q", b, "ok body")
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestDoRetryAfterHonoredThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, "slow down")
			return
		}
		io.WriteString(w, "recovered")
	}))
	defer srv.Close()

	c := newTestClient()
	// RetryBackoff is tiny; only an honored Retry-After should force a ~1s wait.
	start := time.Now()
	resp, err := c.Do(context.Background(), mustReq(t, context.Background(), srv.URL))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "recovered" {
		t.Errorf("body = %q, want recovered", b)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("server calls = %d, want 2", calls)
	}
	if elapsed < 900*time.Millisecond {
		t.Errorf("elapsed = %s, expected to honor Retry-After: 1 (>= ~1s)", elapsed)
	}
}

func TestDoRetryExhaustionReturnsContextfulError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "boom")
	}))
	defer srv.Close()

	c := newTestClient()
	c.MaxAttempts = 3
	trace := &traceBuf{}
	c = c.WithTrace(trace)

	resp, err := c.Do(context.Background(), mustReq(t, context.Background(), srv.URL))
	if err == nil {
		if resp != nil {
			resp.Body.Close()
		}
		t.Fatal("expected error on 5xx exhaustion, got nil")
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Errorf("server calls = %d, want 3 (MaxAttempts)", calls)
	}
	msg := err.Error()
	// Rich context: provider name, attempt count, and elapsed duration.
	for _, want := range []string{"test", "3", "attempt"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
	if !strings.Contains(msg, "500") {
		t.Errorf("error %q should mention status 500", msg)
	}
}

func TestDoContextCancelAbortsMidRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		// Force a long Retry-After so the backoff wait is where cancellation lands.
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, "later")
	}))
	defer srv.Close()

	c := newTestClient()
	c.MaxAttempts = 4

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Let the first attempt complete, then cancel while we wait to retry.
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	resp, err := c.Do(ctx, mustReq(t, ctx, srv.URL))
	elapsed := time.Since(start)
	if err == nil {
		if resp != nil {
			resp.Body.Close()
		}
		t.Fatal("expected cancellation error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want wrapping context.Canceled", err)
	}
	// Must abort promptly (well under the 30s Retry-After), not wait it out.
	if elapsed > 5*time.Second {
		t.Errorf("elapsed = %s, cancellation did not abort mid-backoff", elapsed)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("server calls = %d, want 1 (cancelled before retry)", got)
	}
}
