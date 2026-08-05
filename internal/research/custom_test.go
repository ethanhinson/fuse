package research

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/config"
)

// customServerDouble is a mutex-guarded httptest handler double. Every field is
// touched under the same lock on both the write (handler goroutine) and the
// read (test-assertion goroutine) paths: a lock on only one side is still a
// race under -race (mutex-test-double-concurrent-provider).
type customServerDouble struct {
	mu sync.Mutex

	calls int
	// captured request details from the most recent call.
	rawQuery  string // r.URL.RawQuery
	path      string // r.URL.Path
	headers   http.Header
	queryVals map[string]string // decoded r.URL.Query()

	status int
	body   string
}

func (d *customServerDouble) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.calls++
		d.rawQuery = r.URL.RawQuery
		d.path = r.URL.Path
		d.headers = r.Header.Clone()
		d.queryVals = map[string]string{}
		for k, v := range r.URL.Query() {
			if len(v) > 0 {
				d.queryVals[k] = v[0]
			}
		}
		status := d.status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		io.WriteString(w, d.body)
	}
}

func (d *customServerDouble) captured() (rawQuery, path string, header http.Header, vals map[string]string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.rawQuery, d.path, d.headers, d.queryVals
}

func (d *customServerDouble) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// newTestCustomProvider wires a CustomHTTPProvider to an httptest server by
// injecting the server's client so the bounded research Client talks to it,
// with a tiny retry backoff for fast tests.
func newTestCustomProvider(srv *httptest.Server, cfg config.CustomProviderConfig) *CustomHTTPProvider {
	p := NewCustomProvider(cfg)
	p.client = NewClient("custom", srv.Client())
	p.client.RetryBackoff = time.Millisecond
	return p
}

// defaultCustomConfig returns a config with the SearXNG-shaped defaults filled
// in, pointed at the given base URL with {query}/{count} placeholders.
func defaultCustomConfig(baseURL string) config.CustomProviderConfig {
	return config.CustomProviderConfig{
		URL:          baseURL + "/search?q={query}&count={count}&format=json",
		ResultsPath:  "results",
		TitleField:   "title",
		URLField:     "url",
		SnippetField: "content",
	}
}

func TestCustomSearchDefaultMappings(t *testing.T) {
	d := &customServerDouble{
		status: http.StatusOK,
		body: `{"results":[
			{"title":"Go Programming","url":"https://go.dev","content":"The Go language"},
			{"title":"Rust Lang","url":"https://rust-lang.org","content":"Systems language"}
		]}`,
	}
	srv := httptest.NewServer(d.handler())
	defer srv.Close()

	p := newTestCustomProvider(srv, defaultCustomConfig(srv.URL))
	results, err := p.Search(context.Background(), "programming languages", 2)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	if results[0].Title != "Go Programming" || results[0].URL != "https://go.dev" || results[0].Snippet != "The Go language" {
		t.Errorf("result[0] = %+v, want title/url/content mapped", results[0])
	}
	if results[1].Title != "Rust Lang" || results[1].Snippet != "Systems language" {
		t.Errorf("result[1] = %+v, wrong mapping", results[1])
	}

	// Assert {query}/{count} substitution: the server saw decoded values.
	_, _, _, vals := d.captured()
	if vals["q"] != "programming languages" {
		t.Errorf("q = %q, want %q (query substituted+escaped)", vals["q"], "programming languages")
	}
	if vals["count"] != "2" {
		t.Errorf("count = %q, want 2 (count substituted)", vals["count"])
	}
}

func TestCustomSearchOverriddenMappings(t *testing.T) {
	cfg := config.CustomProviderConfig{
		URL:          "", // filled below
		ResultsPath:  "data.items",
		TitleField:   "name",
		URLField:     "url",
		SnippetField: "desc",
	}
	d := &customServerDouble{
		status: http.StatusOK,
		body: `{"data":{"items":[
			{"name":"Alpha","url":"https://a.example","desc":"first"},
			{"name":"Beta","url":"https://b.example","desc":"second"}
		]}}`,
	}
	srv := httptest.NewServer(d.handler())
	defer srv.Close()

	cfg.URL = srv.URL + "/api?query={query}&n={count}"
	p := newTestCustomProvider(srv, cfg)
	results, err := p.Search(context.Background(), "hello", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	if results[0].Title != "Alpha" || results[0].URL != "https://a.example" || results[0].Snippet != "first" {
		t.Errorf("result[0] = %+v, want name/url/desc mapped from nested path", results[0])
	}
	if results[1].Title != "Beta" || results[1].Snippet != "second" {
		t.Errorf("result[1] = %+v, wrong mapping", results[1])
	}
}

func TestCustomIsConfigured(t *testing.T) {
	unconfigured := NewCustomProvider(config.CustomProviderConfig{})
	if unconfigured.IsConfigured() {
		t.Error("IsConfigured() = true for empty URL, want false")
	}
	// An unconfigured provider must surface a clear error rather than dialing.
	if _, err := unconfigured.Search(context.Background(), "q", 3); err == nil {
		t.Error("Search on unconfigured provider = nil error, want error")
	}

	configured := NewCustomProvider(config.CustomProviderConfig{URL: "https://example.com/search?q={query}"})
	if !configured.IsConfigured() {
		t.Error("IsConfigured() = false for non-empty URL, want true")
	}
}

func TestCustomHeadersApplied(t *testing.T) {
	d := &customServerDouble{status: http.StatusOK, body: `{"results":[]}`}
	srv := httptest.NewServer(d.handler())
	defer srv.Close()

	cfg := defaultCustomConfig(srv.URL)
	cfg.Headers = map[string]string{
		"Authorization": "Bearer secret-token",
		"X-Custom":      "flavor",
	}
	p := newTestCustomProvider(srv, cfg)
	if _, err := p.Search(context.Background(), "q", 3); err != nil {
		t.Fatalf("Search: %v", err)
	}
	_, _, header, _ := d.captured()
	if got := header.Get("Authorization"); got != "Bearer secret-token" {
		t.Errorf("Authorization header = %q, want %q", got, "Bearer secret-token")
	}
	if got := header.Get("X-Custom"); got != "flavor" {
		t.Errorf("X-Custom header = %q, want flavor", got)
	}
}

func TestCustomQueryEscaped(t *testing.T) {
	d := &customServerDouble{status: http.StatusOK, body: `{"results":[]}`}
	srv := httptest.NewServer(d.handler())
	defer srv.Close()

	p := newTestCustomProvider(srv, defaultCustomConfig(srv.URL))
	special := "c++ & rust/go?"
	if _, err := p.Search(context.Background(), special, 3); err != nil {
		t.Fatalf("Search: %v", err)
	}
	rawQuery, _, _, vals := d.captured()
	// The server must decode back to the original, un-mangled query...
	if vals["q"] != special {
		t.Errorf("decoded q = %q, want %q", vals["q"], special)
	}
	// ...and the raw wire form must carry the escaped octets, proving the
	// bare special characters were URL-escaped rather than passed through.
	if len(rawQuery) == 0 {
		t.Fatal("raw query empty")
	}
	if wantsEscaped := "c%2B%2B"; !contains(rawQuery, wantsEscaped) {
		t.Errorf("raw query %q missing escaped %q (query not URL-escaped)", rawQuery, wantsEscaped)
	}
}

func TestCustomSearchNonProblemStatus(t *testing.T) {
	d := &customServerDouble{status: http.StatusUnauthorized, body: `{"error":"nope"}`}
	srv := httptest.NewServer(d.handler())
	defer srv.Close()

	p := newTestCustomProvider(srv, defaultCustomConfig(srv.URL))
	if _, err := p.Search(context.Background(), "q", 3); err == nil {
		t.Fatal("expected error on non-2xx status, got nil")
	}
}

func TestCustomSearchMissingResultsPath(t *testing.T) {
	d := &customServerDouble{status: http.StatusOK, body: `{"unexpected":true}`}
	srv := httptest.NewServer(d.handler())
	defer srv.Close()

	p := newTestCustomProvider(srv, defaultCustomConfig(srv.URL))
	results, err := p.Search(context.Background(), "q", 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results = %d, want 0 for missing results path", len(results))
	}
}

func TestCustomSearchSkipsMalformed(t *testing.T) {
	d := &customServerDouble{
		status: http.StatusOK,
		body: `{"results":[
			{"title":"Good","url":"https://good.example","content":"ok"},
			"a bare string, not an object",
			{"title":"No URL","content":"missing url so dropped"},
			{"url":"https://only-url.example"}
		]}`,
	}
	srv := httptest.NewServer(d.handler())
	defer srv.Close()

	p := newTestCustomProvider(srv, defaultCustomConfig(srv.URL))
	results, err := p.Search(context.Background(), "q", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2 (malformed/no-url dropped)", len(results))
	}
	if results[0].URL != "https://good.example" || results[1].URL != "https://only-url.example" {
		t.Errorf("results = %+v, want the two url-bearing entries", results)
	}
}

func TestCustomName(t *testing.T) {
	if got := NewCustomProvider(config.CustomProviderConfig{}).Name(); got != "custom" {
		t.Errorf("Name() = %q, want custom", got)
	}
}

func TestCustomFieldDefaultsWhenEmpty(t *testing.T) {
	// A config that leaves ResultsPath/field mappings empty must fall back to
	// the SearXNG defaults (results/title/url/content).
	cfg := config.CustomProviderConfig{}
	d := &customServerDouble{
		status: http.StatusOK,
		body:   `{"results":[{"title":"T","url":"https://u.example","content":"C"}]}`,
	}
	srv := httptest.NewServer(d.handler())
	defer srv.Close()

	cfg.URL = srv.URL + "/search?q={query}&count={count}"
	p := newTestCustomProvider(srv, cfg)
	results, err := p.Search(context.Background(), "q", 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if results[0].Title != "T" || results[0].URL != "https://u.example" || results[0].Snippet != "C" {
		t.Errorf("result = %+v, want defaults applied", results[0])
	}
}

// contains is a tiny substring helper kept local to avoid importing strings
// solely for the escape assertion.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
