package research

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/config"
)

// scraperServerDouble is a mutex-guarded httptest handler double. Every field is
// touched under the same lock on both the write (handler goroutine) and the read
// (test-assertion goroutine) paths: a lock on only one side is still a race under
// -race (mutex-test-double-concurrent-provider).
type scraperServerDouble struct {
	mu sync.Mutex
	// pageCalls counts requests to any path other than /robots.txt.
	pageCalls int
	// robotsCalls counts requests to /robots.txt.
	robotsCalls int
	// robotsBody, when non-empty, is served for /robots.txt; robotsStatus
	// defaults to 200 and robots404 forces a 404 (no robots.txt present).
	robotsBody   string
	robots404    bool
	// page response.
	pageBody        string
	pageContentType string
}

func (d *scraperServerDouble) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if r.URL.Path == "/robots.txt" {
			d.robotsCalls++
			if d.robots404 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(d.robotsBody))
			return
		}
		d.pageCalls++
		ct := d.pageContentType
		if ct == "" {
			ct = "text/html; charset=utf-8"
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(d.pageBody))
	}
}

func (d *scraperServerDouble) counts() (page, robots int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pageCalls, d.robotsCalls
}

// newTestScraper builds a Scraper wired to srv's client so the bounded research
// Client talks to the httptest server, and sets a permissive rate limiter so
// timing does not slow focused tests.
func newTestScraper(t *testing.T, srv *httptest.Server, cfg config.ResearchConfig) *Scraper {
	t.Helper()
	s := NewScraper(cfg)
	s.client = NewClient("scraper", srv.Client())
	s.client.RetryBackoff = time.Millisecond
	// Fast limiter so multi-fetch tests do not pace on the 2/s default.
	s.limiter.rate = 1000
	s.limiter.burst = 1000
	return s
}

const fixtureArticleHTML = `<!DOCTYPE html>
<html>
<head><title>Test Article</title>
<style>.nav { color: red; }</style>
<script>var tracking = "should not appear in output";</script>
</head>
<body>
<nav id="navbar"><a href="/">HomeNavLink</a> <a href="/about">AboutNavLink</a></nav>
<article>
<h1>The Main Headline Of The Piece</h1>
<p>This is the first substantial paragraph of the article body. It contains
enough real prose that go-readability will treat it as the main content and keep
it while discarding the navigation and boilerplate around it.</p>
<p>Here is a second paragraph continuing the argument with more sentences so the
extractor has a clear main-body region to lock onto and return as readable text
for the reader to consume downstream.</p>
</article>
<footer>FooterBoilerplateText copyright notice</footer>
</body>
</html>`

func TestScraperFetchExtractsArticleBody(t *testing.T) {
	d := &scraperServerDouble{robots404: true, pageBody: fixtureArticleHTML}
	srv := httptest.NewServer(d.handler())
	defer srv.Close()

	cfg := config.Default().Research
	s := newTestScraper(t, srv, cfg)

	out, err := s.Fetch(context.Background(), srv.URL+"/article")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.Contains(out, "first substantial paragraph") {
		t.Errorf("output missing main article text; got:\n%s", out)
	}
	if !strings.Contains(out, "second paragraph continuing") {
		t.Errorf("output missing second paragraph; got:\n%s", out)
	}
	if strings.Contains(out, "should not appear") {
		t.Errorf("script content leaked into output:\n%s", out)
	}
	if strings.Contains(out, "color: red") {
		t.Errorf("style content leaked into output:\n%s", out)
	}
}

func TestScraperRobotsDisallowSkipsFetch(t *testing.T) {
	d := &scraperServerDouble{robotsBody: "User-agent: *\nDisallow: /\n", pageBody: fixtureArticleHTML}
	srv := httptest.NewServer(d.handler())
	defer srv.Close()

	cfg := config.Default().Research // RespectRobots default true
	s := newTestScraper(t, srv, cfg)

	out, err := s.Fetch(context.Background(), srv.URL+"/article")
	if err != nil {
		t.Fatalf("Fetch returned error, want a note: %v", err)
	}
	if !strings.Contains(out, "robots.txt disallows") {
		t.Errorf("expected robots note, got: %q", out)
	}
	page, robots := d.counts()
	if page != 0 {
		t.Errorf("page fetched %d time(s), want 0 (robots disallowed)", page)
	}
	if robots == 0 {
		t.Errorf("robots.txt never fetched, want at least 1")
	}
}

func TestScraperRobotsCachedAcrossFetches(t *testing.T) {
	d := &scraperServerDouble{robotsBody: "User-agent: *\nDisallow: /\n", pageBody: fixtureArticleHTML}
	srv := httptest.NewServer(d.handler())
	defer srv.Close()

	s := newTestScraper(t, srv, config.Default().Research)

	for i := 0; i < 3; i++ {
		if _, err := s.Fetch(context.Background(), srv.URL+"/article"); err != nil {
			t.Fatalf("Fetch %d: %v", i, err)
		}
	}
	_, robots := d.counts()
	if robots != 1 {
		t.Errorf("robots.txt fetched %d times, want 1 (cached per instance)", robots)
	}
}

func TestScraperRespectRobotsFalseFetchesAnyway(t *testing.T) {
	d := &scraperServerDouble{robotsBody: "User-agent: *\nDisallow: /\n", pageBody: fixtureArticleHTML}
	srv := httptest.NewServer(d.handler())
	defer srv.Close()

	cfg := config.Default().Research
	cfg.RespectRobots = false
	s := newTestScraper(t, srv, cfg)

	out, err := s.Fetch(context.Background(), srv.URL+"/article")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if strings.Contains(out, "robots.txt disallows") {
		t.Errorf("robots gate should be skipped when RespectRobots=false; got: %q", out)
	}
	if !strings.Contains(out, "first substantial paragraph") {
		t.Errorf("expected article body, got: %q", out)
	}
	page, robots := d.counts()
	if page == 0 {
		t.Errorf("page not fetched, want it fetched (robots skipped)")
	}
	if robots != 0 {
		t.Errorf("robots.txt fetched %d times, want 0 (gate skipped)", robots)
	}
}

func TestScraperNonHTMLSkipped(t *testing.T) {
	d := &scraperServerDouble{
		robots404:       true,
		pageBody:        "%PDF-1.4 binary junk",
		pageContentType: "application/pdf",
	}
	srv := httptest.NewServer(d.handler())
	defer srv.Close()

	s := newTestScraper(t, srv, config.Default().Research)

	out, err := s.Fetch(context.Background(), srv.URL+"/doc.pdf")
	if err != nil {
		t.Fatalf("Fetch returned error, want a note: %v", err)
	}
	if !strings.Contains(out, "skipped non-HTML content") {
		t.Errorf("expected non-HTML skip note, got: %q", out)
	}
	if !strings.Contains(out, "application/pdf") {
		t.Errorf("skip note should mention the content type, got: %q", out)
	}
}

func TestScraperTruncatesAtWordBoundary(t *testing.T) {
	// Build a long HTML body of real words so readability keeps it and it far
	// exceeds the cap.
	var sb strings.Builder
	sb.WriteString("<html><body><article><h1>Long Piece Headline Here</h1>")
	for i := 0; i < 4000; i++ {
		sb.WriteString("<p>alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima mike november oscar papa quebec romeo sierra tango.</p>")
	}
	sb.WriteString("</article></body></html>")

	d := &scraperServerDouble{robots404: true, pageBody: sb.String()}
	srv := httptest.NewServer(d.handler())
	defer srv.Close()

	cfg := config.Default().Research
	cfg.MaxContentKB = 4 // small cap to force truncation
	s := newTestScraper(t, srv, cfg)

	out, err := s.Fetch(context.Background(), srv.URL+"/long")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	cap := cfg.MaxContentKB * 1024
	if !strings.Contains(out, truncationMarker) {
		t.Fatalf("expected truncation marker in output of len %d", len(out))
	}
	body := strings.TrimSuffix(out, truncationMarker)
	if len(body) > cap {
		t.Errorf("truncated body len %d exceeds cap %d", len(body), cap)
	}
	// Word boundary: the retained body must not end mid-word. Since words are
	// separated by spaces, the last rune before the marker should be a space
	// (we back up to the last space and keep it) — assert no trailing partial
	// word by checking the body ends at a space boundary.
	if body != "" && !strings.HasSuffix(body, " ") {
		t.Errorf("truncated body does not end at a word boundary: %q", tailOf(body, 30))
	}
}

func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func TestScraperCrudeFallbackWhenReadabilityFails(t *testing.T) {
	// A body with no article-like structure that still contains text; readability
	// may yield little/nothing, so the crude fallback must still surface text.
	body := `<html><body><script>var x = "hidden";</script><style>p{}</style>plain visible words here</body></html>`
	d := &scraperServerDouble{robots404: true, pageBody: body}
	srv := httptest.NewServer(d.handler())
	defer srv.Close()

	s := newTestScraper(t, srv, config.Default().Research)

	out, err := s.Fetch(context.Background(), srv.URL+"/thin")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.Contains(out, "plain visible words") {
		t.Errorf("expected visible text via extraction/fallback, got: %q", out)
	}
	if strings.Contains(out, "hidden") {
		t.Errorf("script text leaked through fallback: %q", out)
	}
}

// TestRateLimiterPaces exercises the token-bucket limiter directly at a slow
// rate with a fake-free timing tolerance, keeping the test sub-second.
func TestRateLimiterPaces(t *testing.T) {
	// 2 tokens/sec, burst 1: first Wait is immediate, the second must block for
	// roughly 1/rate = 500ms.
	rl := newRateLimiter(2, 1)
	ctx := context.Background()

	start := time.Now()
	if err := rl.wait(ctx); err != nil {
		t.Fatalf("first wait: %v", err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("first wait blocked %s, want ~immediate", d)
	}

	start = time.Now()
	if err := rl.wait(ctx); err != nil {
		t.Fatalf("second wait: %v", err)
	}
	if d := time.Since(start); d < 400*time.Millisecond {
		t.Errorf("second wait blocked %s, want >= ~500ms (2/s pacing)", d)
	}
}

func TestScraperRateLimiterIsPerDomain(t *testing.T) {
	s := NewScraper(config.Default().Research)
	a := s.limiterFor("example.com")
	b := s.limiterFor("example.com")
	c := s.limiterFor("other.com")
	if a != b {
		t.Errorf("same host returned distinct limiters")
	}
	if a == c {
		t.Errorf("different hosts share a limiter")
	}
}
