package research

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// braveServerDouble is a mutex-guarded httptest handler double. Every field is
// touched under the same lock on both the write (handler goroutine) and the
// read (test-assertion goroutine) paths: a lock on only one side is still a
// race under -race (mutex-test-double-concurrent-provider).
type braveServerDouble struct {
	mu    sync.Mutex
	calls int
	// captured request details from the most recent call.
	subToken    string
	accept      string
	queryQ      string
	queryCount  string
	// responses drives per-call behavior; index i is used on call i (1-based).
	responses []braveResponse
}

type braveResponse struct {
	status     int
	body       string
	retryAfter string
}

func (d *braveServerDouble) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.calls++
		d.subToken = r.Header.Get("X-Subscription-Token")
		d.accept = r.Header.Get("Accept")
		d.queryQ = r.URL.Query().Get("q")
		d.queryCount = r.URL.Query().Get("count")

		resp := braveResponse{status: http.StatusOK, body: `{"web":{"results":[]}}`}
		idx := d.calls - 1
		if idx < len(d.responses) {
			resp = d.responses[idx]
		}
		if resp.retryAfter != "" {
			w.Header().Set("Retry-After", resp.retryAfter)
		}
		if resp.status == 0 {
			resp.status = http.StatusOK
		}
		w.WriteHeader(resp.status)
		io.WriteString(w, resp.body)
	}
}

func (d *braveServerDouble) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func (d *braveServerDouble) captured() (subToken, accept, q, count string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.subToken, d.accept, d.queryQ, d.queryCount
}

// newTestBraveProvider wires a BraveSearchProvider to an httptest server: it
// points baseURL at the server and injects the server's client so the bounded
// research Client talks to it, with a tiny retry backoff for fast tests.
func newTestBraveProvider(srv *httptest.Server) *BraveSearchProvider {
	p := NewBraveProvider("test-key")
	p.baseURL = srv.URL
	p.client = NewClient("brave", srv.Client())
	p.client.RetryBackoff = time.Millisecond
	return p
}

func TestBraveSearchHappyPath(t *testing.T) {
	d := &braveServerDouble{responses: []braveResponse{{
		status: http.StatusOK,
		body: `{"web":{"results":[
			{"title":"Go Programming","url":"https://go.dev","description":"The Go language"},
			{"title":"Rust Lang","url":"https://rust-lang.org","description":"Systems language"}
		]}}`,
	}}}
	srv := httptest.NewServer(d.handler())
	defer srv.Close()

	p := newTestBraveProvider(srv)
	results, err := p.Search(context.Background(), "programming languages", 2)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	if results[0].Title != "Go Programming" || results[0].URL != "https://go.dev" || results[0].Snippet != "The Go language" {
		t.Errorf("result[0] = %+v, want title/url/description mapped", results[0])
	}
	if results[1].Title != "Rust Lang" || results[1].Snippet != "Systems language" {
		t.Errorf("result[1] = %+v, wrong mapping", results[1])
	}

	subToken, accept, q, count := d.captured()
	if subToken != "test-key" {
		t.Errorf("X-Subscription-Token = %q, want test-key", subToken)
	}
	if accept != "application/json" {
		t.Errorf("Accept = %q, want application/json", accept)
	}
	if q != "programming languages" {
		t.Errorf("q = %q, want %q", q, "programming languages")
	}
	if count != "2" {
		t.Errorf("count = %q, want 2", count)
	}
}

func TestBraveSearchMalformedJSON(t *testing.T) {
	d := &braveServerDouble{responses: []braveResponse{{
		status: http.StatusOK,
		body:   `{"web":{"results":[ this is not json`,
	}}}
	srv := httptest.NewServer(d.handler())
	defer srv.Close()

	p := newTestBraveProvider(srv)
	_, err := p.Search(context.Background(), "q", 5)
	if err == nil {
		t.Fatal("expected error on malformed JSON, got nil")
	}
}

func TestBraveSearchNonProblemStatus(t *testing.T) {
	d := &braveServerDouble{responses: []braveResponse{{
		status: http.StatusUnauthorized,
		body:   `{"error":"bad key"}`,
	}}}
	srv := httptest.NewServer(d.handler())
	defer srv.Close()

	p := newTestBraveProvider(srv)
	_, err := p.Search(context.Background(), "q", 5)
	if err == nil {
		t.Fatal("expected error on non-2xx status, got nil")
	}
}

func TestBraveSearchRetryAfterThenSucceeds(t *testing.T) {
	d := &braveServerDouble{responses: []braveResponse{
		{status: http.StatusTooManyRequests, retryAfter: "1", body: "slow down"},
		{status: http.StatusOK, body: `{"web":{"results":[{"title":"T","url":"https://u","description":"D"}]}}`},
	}}
	srv := httptest.NewServer(d.handler())
	defer srv.Close()

	p := newTestBraveProvider(srv)
	results, err := p.Search(context.Background(), "q", 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].Title != "T" {
		t.Fatalf("results = %+v, want single mapped hit after retry", results)
	}
	if got := d.callCount(); got != 2 {
		t.Errorf("server calls = %d, want 2 (retry exercised)", got)
	}
}

func TestBraveSearchEmptyResults(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty results array", `{"web":{"results":[]}}`},
		{"absent web", `{}`},
		{"absent results", `{"web":{}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &braveServerDouble{responses: []braveResponse{{status: http.StatusOK, body: tc.body}}}
			srv := httptest.NewServer(d.handler())
			defer srv.Close()

			p := newTestBraveProvider(srv)
			results, err := p.Search(context.Background(), "q", 5)
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(results) != 0 {
				t.Errorf("results = %d, want 0", len(results))
			}
		})
	}
}

func TestBraveName(t *testing.T) {
	if got := NewBraveProvider("k").Name(); got != "brave" {
		t.Errorf("Name() = %q, want brave", got)
	}
}
