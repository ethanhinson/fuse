package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/research"
)

// fetchServerDouble is a mutex-guarded httptest handler double. State is touched
// under the same lock on both the handler and assertion paths so it is safe
// under -race.
type fetchServerDouble struct {
	mu       sync.Mutex
	pageBody string
	calls    int
}

func (d *fetchServerDouble) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if r.URL.Path == "/robots.txt" {
			// No robots.txt present; fail-open.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		d.calls++
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(d.pageBody))
	}
}

func (d *fetchServerDouble) pageCalls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

const fetchFixtureHTML = `<!DOCTYPE html><html><head><title>Fetch Fixture</title></head>
<body><article>
<h1>The Headline That Anchors The Article</h1>
<p>This is the first substantial paragraph of the fixture article body. It has
enough real prose that go-readability treats it as the primary content and keeps
it in the extracted text returned to the caller.</p>
<p>A second paragraph continues so the extractor has a clear main-body region to
lock onto and return as readable text for downstream consumers to read.</p>
</article></body></html>`

// newFetchTestScraper builds a real Scraper (robots respected but the test
// server returns 404 → fail-open) using the default research client, which can
// reach the localhost httptest server. No external network is touched.
func newFetchTestScraper() *research.Scraper {
	cfg := config.Default().Research
	cfg.RespectRobots = false // avoid the extra robots round-trip in the happy path
	return research.NewScraper(cfg)
}

func TestWebFetchNameAndParameters(t *testing.T) {
	tool := newWebFetchWithScraper(newFetchTestScraper())
	if tool.Name() != "web_fetch" {
		t.Fatalf("Name() = %q, want web_fetch", tool.Name())
	}
	params := tool.Parameters()
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties missing or wrong type: %#v", params["properties"])
	}
	if _, ok := props["url"]; !ok {
		t.Errorf("parameters missing url property")
	}
	req, ok := params["required"].([]string)
	if !ok || len(req) != 1 || req[0] != "url" {
		t.Errorf("required = %#v, want [url]", params["required"])
	}
}

func TestWebFetchHappyPath(t *testing.T) {
	d := &fetchServerDouble{pageBody: fetchFixtureHTML}
	srv := httptest.NewServer(d.handler())
	defer srv.Close()

	tool := newWebFetchWithScraper(newFetchTestScraper())
	res := tool.Execute(context.Background(), `{"url":"`+srv.URL+`/article"}`)
	if res.IsError {
		t.Fatalf("unexpected error result: %s", res.Output)
	}
	if !strings.Contains(res.Output, "first substantial paragraph") {
		t.Errorf("extracted text missing expected body\n---\n%s", res.Output)
	}
	if strings.Contains(res.Output, "<article>") {
		t.Errorf("output should be extracted text, not raw HTML\n---\n%s", res.Output)
	}
	if got := d.pageCalls(); got != 1 {
		t.Errorf("page fetched %d times, want 1", got)
	}
}

func TestWebFetchBadArgs(t *testing.T) {
	tool := newWebFetchWithScraper(newFetchTestScraper())
	res := tool.Execute(context.Background(), `{"url":`)
	if !res.IsError {
		t.Fatalf("bad JSON should be an error result, got %q", res.Output)
	}
}

func TestWebFetchMissingURL(t *testing.T) {
	tool := newWebFetchWithScraper(newFetchTestScraper())
	res := tool.Execute(context.Background(), `{}`)
	if !res.IsError {
		t.Fatalf("missing url should be an error result, got %q", res.Output)
	}
}
