package research

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordedReq captures what the test server saw for one request. Shared doubles
// are mutex-guarded: a lock on one side only is still a race under -race
// (mutex-test-double-concurrent-provider).
type recordedReq struct {
	auth        string
	contentType string
	body        string
}

// tavilyRecorder is a mutex-guarded collector of the requests a test server
// observed, plus the parsed query/max_results of the last request.
type tavilyRecorder struct {
	mu   sync.Mutex
	reqs []recordedReq
}

func (r *tavilyRecorder) record(req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, recordedReq{
		auth:        req.Header.Get("Authorization"),
		contentType: req.Header.Get("Content-Type"),
		body:        string(body),
	})
}

func (r *tavilyRecorder) snapshot() []recordedReq {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedReq, len(r.reqs))
	copy(out, r.reqs)
	return out
}

// newTestTavilyProvider builds a provider pointed at srv with a fast-retry
// httptest-backed client.
func newTestTavilyProvider(apiKey, baseURL string) *TavilyProvider {
	p := NewTavilyProvider(apiKey)
	p.baseURL = baseURL
	p.client.RetryBackoff = time.Millisecond
	return p
}

func TestTavilyProviderName(t *testing.T) {
	if got := NewTavilyProvider("k").Name(); got != "tavily" {
		t.Errorf("Name() = %q, want tavily", got)
	}
}

func TestTavilySearchHappyPath(t *testing.T) {
	rec := &tavilyRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		io.WriteString(w, `{"results":[
			{"title":"First","url":"https://a.example/1","content":"body one extracted"},
			{"title":"Second","url":"https://b.example/2","content":"body two extracted"}
		]}`)
	}))
	defer srv.Close()

	p := newTestTavilyProvider("secret-key", srv.URL)
	results, err := p.Search(context.Background(), "quantum widgets", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	if results[0].Title != "First" || results[0].URL != "https://a.example/1" {
		t.Errorf("results[0] = %+v", results[0])
	}
	// content seeds Snippet so the skill can skip a web_fetch.
	if results[0].Snippet != "body one extracted" {
		t.Errorf("results[0].Snippet = %q, want content-derived snippet", results[0].Snippet)
	}
	if results[1].Snippet != "body two extracted" {
		t.Errorf("results[1].Snippet = %q", results[1].Snippet)
	}

	reqs := rec.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("server saw %d requests, want 1", len(reqs))
	}
	if reqs[0].auth != "Bearer secret-key" {
		t.Errorf("Authorization = %q, want Bearer secret-key", reqs[0].auth)
	}
	if !strings.HasPrefix(reqs[0].contentType, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", reqs[0].contentType)
	}
	// The POST body must carry query + max_results.
	var sent struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
	}
	if err := json.Unmarshal([]byte(reqs[0].body), &sent); err != nil {
		t.Fatalf("unmarshal sent body %q: %v", reqs[0].body, err)
	}
	if sent.Query != "quantum widgets" {
		t.Errorf("sent query = %q", sent.Query)
	}
	if sent.MaxResults != 5 {
		t.Errorf("sent max_results = %d, want 5", sent.MaxResults)
	}
}

func TestTavilySearchMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"results": [ this is not json`)
	}))
	defer srv.Close()

	p := newTestTavilyProvider("k", srv.URL)
	_, err := p.Search(context.Background(), "q", 3)
	if err == nil {
		t.Fatal("expected error on malformed JSON, got nil")
	}
}

func TestTavilySearchNonSuccessStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, "bad key")
	}))
	defer srv.Close()

	p := newTestTavilyProvider("k", srv.URL)
	_, err := p.Search(context.Background(), "q", 3)
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error %q should mention status 401", err)
	}
}

func TestTavilySearchMissingResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	p := newTestTavilyProvider("k", srv.URL)
	results, err := p.Search(context.Background(), "q", 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("len(results) = %d, want 0", len(results))
	}
}

// TestTavilySearchRetriesCarryFullBody is the GetBody-rewind proof: after a 429
// with Retry-After, the retried POST must still carry the complete body. The
// server asserts a non-empty body on BOTH requests. Counter and bodies are
// mutex-guarded.
func TestTavilySearchRetriesCarryFullBody(t *testing.T) {
	rec := &tavilyRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		// Decide behavior based on how many requests we have recorded.
		n := len(rec.snapshot())
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, "slow down")
			return
		}
		io.WriteString(w, `{"results":[{"title":"OK","url":"https://ok.example","content":"recovered"}]}`)
	}))
	defer srv.Close()

	p := newTestTavilyProvider("k", srv.URL)
	results, err := p.Search(context.Background(), "resilient query", 4)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].Snippet != "recovered" {
		t.Fatalf("results = %+v, want single recovered result", results)
	}

	reqs := rec.snapshot()
	if len(reqs) != 2 {
		t.Fatalf("server saw %d requests, want 2 (429 then 200)", len(reqs))
	}
	// Both requests must have carried the full body — this proves GetBody rewind.
	for i, req := range reqs {
		if req.body == "" {
			t.Errorf("request %d carried empty body; GetBody rewind failed", i)
			continue
		}
		var sent struct {
			Query      string `json:"query"`
			MaxResults int    `json:"max_results"`
		}
		if err := json.Unmarshal([]byte(req.body), &sent); err != nil {
			t.Errorf("request %d body %q not valid JSON: %v", i, req.body, err)
			continue
		}
		if sent.Query != "resilient query" || sent.MaxResults != 4 {
			t.Errorf("request %d body = %+v, want full query+max_results", i, sent)
		}
	}
}
