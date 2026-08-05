package research

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	readability "github.com/go-shiori/go-readability"

	"github.com/ethanhinson/fuse/internal/config"
)

// truncationMarker is appended when extracted content is cut at the MaxContentKB
// cap, so downstream readers can tell a source was truncated rather than short.
const truncationMarker = "\n\n[content truncated at size limit]"

// scraperUserAgent identifies the fetcher in requests and is the token matched
// against robots.txt group headers ("User-agent: *" always applies).
const scraperUserAgent = "fuse-research/1.0"

// perURLDeadline bounds one Fetch end-to-end. The bounded Client already bounds
// each attempt via RequestTimeout; this caps the whole call (including retries).
const perURLDeadline = 10 * time.Second

// domainRatePerSec is the sustained per-domain request rate. A single burst
// token lets the first request through immediately, then requests to the same
// host are paced at this rate.
const domainRatePerSec = 2.0

// Scraper is the web_fetch engine: it fetches a URL, honors robots.txt (once
// per host per instance), paces requests per domain, and returns the page's
// main-body text extracted with go-readability (with a crude tag-strip
// fallback), truncated to the configured content cap.
//
// A Scraper is safe for concurrent use: its robots cache and limiter map are
// mutex-guarded.
type Scraper struct {
	client *Client

	maxContentKB  int
	respectRobots bool

	robotsMu    sync.Mutex
	robotsCache map[string]robotsRules // host -> parsed rules (cached once per instance)

	limiterMu sync.Mutex
	limiters  map[string]*rateLimiter // host -> per-domain token bucket
	// limiter is a template whose rate/burst seed every per-domain limiter; tests
	// override it before use to run without real pacing delays.
	limiter rateLimiter
}

// NewScraper builds a Scraper from cfg, wired to the shared bounded HTTP Client.
// Tests inject an httptest-backed client by overriding the unexported client
// field directly (mirroring the search providers).
func NewScraper(cfg config.ResearchConfig) *Scraper {
	return &Scraper{
		client:        NewClient("scraper", nil),
		maxContentKB:  cfg.MaxContentKB,
		respectRobots: cfg.RespectRobots,
		robotsCache:   map[string]robotsRules{},
		limiters:      map[string]*rateLimiter{},
		limiter:       rateLimiter{rate: domainRatePerSec, burst: 1},
	}
}

// Fetch retrieves rawurl and returns its main-body text. Robots refusals and
// non-HTML content are returned as human-readable NOTE strings (not errors): the
// caller surfaces them as noted refusals. A nil error with a note is the normal
// way this method declines to scrape.
func (s *Scraper) Fetch(ctx context.Context, rawurl string) (string, error) {
	u, err := url.Parse(rawurl)
	if err != nil {
		return "", fmt.Errorf("scraper: parse url %q: %w", rawurl, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("scraper: unsupported url scheme %q", u.Scheme)
	}

	// (a) robots gate — fetch and cache the host's rules once per instance.
	if s.respectRobots {
		rules := s.robotsFor(ctx, u)
		if !rules.allowed(u.Path) {
			return fmt.Sprintf("[robots.txt disallows fetching %s]", rawurl), nil
		}
	}

	// (b) per-domain rate limit — block until the token bucket admits us.
	if err := s.limiterFor(u.Host).wait(ctx); err != nil {
		return "", fmt.Errorf("scraper: rate limit wait: %w", err)
	}

	// (c) HTTP GET, bounded end-to-end by perURLDeadline.
	fetchCtx, cancel := context.WithTimeout(ctx, perURLDeadline)
	defer cancel()

	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, rawurl, nil)
	if err != nil {
		return "", fmt.Errorf("scraper: build request: %w", err)
	}
	req.Header.Set("User-Agent", scraperUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := s.client.Do(fetchCtx, req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return "", fmt.Errorf("scraper: fetch %s failed with status %d", rawurl, resp.StatusCode)
	}

	// (d) non-HTML content is skipped with a note.
	ct := resp.Header.Get("Content-Type")
	if !isHTMLContentType(ct) {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		note := ct
		if note == "" {
			note = "(none)"
		}
		return fmt.Sprintf("[skipped non-HTML content: %s]", note), nil
	}

	// Read the body under a generous read cap so a hostile huge page cannot
	// exhaust memory: keep a few multiples of the content cap to leave room for
	// markup that readability strips.
	readCap := int64(s.contentCapBytes()) * 8
	if readCap < 1<<20 {
		readCap = 1 << 20
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, readCap))
	if err != nil {
		return "", fmt.Errorf("scraper: read body: %w", err)
	}

	// (e) extract main-body text via readability, falling back to a crude strip.
	text := extractText(raw, u)

	// (f) truncate at the content cap on a word boundary.
	return truncateAtWord(text, s.contentCapBytes()), nil
}

// contentCapBytes is the byte budget for extracted content.
func (s *Scraper) contentCapBytes() int {
	kb := s.maxContentKB
	if kb <= 0 {
		kb = 50 // mirror config.Default() so a zero cap never yields empty output
	}
	return kb * 1024
}

// limiterFor returns the per-domain limiter for host, creating it (seeded from
// the scraper's limiter template) on first use.
func (s *Scraper) limiterFor(host string) *rateLimiter {
	s.limiterMu.Lock()
	defer s.limiterMu.Unlock()
	if rl, ok := s.limiters[host]; ok {
		return rl
	}
	rl := newRateLimiter(s.limiter.rate, s.limiter.burst)
	s.limiters[host] = rl
	return rl
}

// robotsFor returns the cached robots rules for u's host, fetching and parsing
// robots.txt exactly once per host per instance. Any fetch/parse failure (or an
// absent robots.txt) caches a permissive ruleset: fail-open, matching common
// crawler behavior and the "noted refusal, not an error" contract.
func (s *Scraper) robotsFor(ctx context.Context, u *url.URL) robotsRules {
	s.robotsMu.Lock()
	defer s.robotsMu.Unlock()
	if rules, ok := s.robotsCache[u.Host]; ok {
		return rules
	}

	rules := s.fetchRobots(ctx, u)
	s.robotsCache[u.Host] = rules
	return rules
}

// fetchRobots GETs /robots.txt for u's host and parses it. It fails open (empty,
// allow-all rules) on any error or non-200.
func (s *Scraper) fetchRobots(ctx context.Context, u *url.URL) robotsRules {
	robotsURL := (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/robots.txt"}).String()

	fetchCtx, cancel := context.WithTimeout(ctx, perURLDeadline)
	defer cancel()

	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, robotsURL, nil)
	if err != nil {
		return robotsRules{}
	}
	req.Header.Set("User-Agent", scraperUserAgent)

	resp, err := s.client.Do(fetchCtx, req)
	if err != nil {
		return robotsRules{}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return robotsRules{}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	if err != nil {
		return robotsRules{}
	}
	return parseRobots(string(body))
}

// isHTMLContentType reports whether a Content-Type header names HTML. The MIME
// parameters (e.g. "; charset=utf-8") are ignored. An empty type is treated as
// HTML: many servers omit it, and the readability/fallback path handles noise.
func isHTMLContentType(ct string) bool {
	if ct == "" {
		return true
	}
	mime := strings.TrimSpace(strings.SplitN(ct, ";", 2)[0])
	mime = strings.ToLower(mime)
	return mime == "text/html" || mime == "application/xhtml+xml"
}

// extractText runs go-readability over raw HTML and returns the main-body plain
// text. On extraction failure (or an empty result) it falls back to a crude
// tag-strip that always yields some text.
func extractText(raw []byte, pageURL *url.URL) string {
	article, err := readability.FromReader(strings.NewReader(string(raw)), pageURL)
	if err == nil {
		if txt := strings.TrimSpace(article.TextContent); txt != "" {
			return txt
		}
	}
	return crudeStrip(raw)
}

// crudeStrip removes <script> and <style> element contents, then strips all
// remaining tags, collapsing whitespace. It always returns some text (possibly
// empty) and never errors — the guaranteed fallback when readability declines.
func crudeStrip(raw []byte) string {
	s := string(raw)
	s = removeElement(s, "script")
	s = removeElement(s, "style")

	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
			b.WriteByte(' ')
		case !inTag:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// removeElement drops every <tag>...</tag> region (case-insensitive) from s,
// including the element's text. Used to purge script/style noise before the
// crude tag-strip.
func removeElement(s, tag string) string {
	lower := strings.ToLower(s)
	open := "<" + tag
	closeTag := "</" + tag + ">"
	for {
		start := strings.Index(lower, open)
		if start < 0 {
			break
		}
		end := strings.Index(lower[start:], closeTag)
		if end < 0 {
			// Unclosed element: drop to end of string.
			s = s[:start]
			break
		}
		end = start + end + len(closeTag)
		s = s[:start] + s[end:]
		lower = strings.ToLower(s)
	}
	return s
}

// truncateAtWord caps text at capBytes bytes, backing up to the last space so no
// word is cut mid-token, and appends truncationMarker. Text within the cap is
// returned unchanged.
func truncateAtWord(text string, capBytes int) string {
	if len(text) <= capBytes {
		return text
	}
	cut := text[:capBytes]
	if idx := strings.LastIndexByte(cut, ' '); idx >= 0 {
		cut = cut[:idx+1] // keep the trailing space to mark the word boundary
	}
	return cut + truncationMarker
}

// robotsRules is a minimal robots.txt ruleset for user-agent "*". It captures
// the Disallow (and overriding Allow) path prefixes that apply to any crawler,
// which is all the scraper needs: it never sends a specific user-agent token, so
// only the "*" group governs it. The empty value allows everything (fail-open).
type robotsRules struct {
	// disallow and allow hold path prefixes from the "*" group, in file order.
	disallow []string
	allow    []string
}

// allowed reports whether path may be fetched. Standard robots precedence: the
// longest matching rule wins, and Allow beats Disallow on an equal-length tie.
// An empty Disallow directive ("Disallow:") means "allow all" and matches
// nothing. With no rules at all, everything is allowed.
func (r robotsRules) allowed(path string) bool {
	if path == "" {
		path = "/"
	}
	longestDisallow := -1
	for _, p := range r.disallow {
		if p == "" {
			continue // "Disallow:" with empty value disallows nothing
		}
		if strings.HasPrefix(path, p) && len(p) > longestDisallow {
			longestDisallow = len(p)
		}
	}
	if longestDisallow < 0 {
		return true
	}
	longestAllow := -1
	for _, p := range r.allow {
		if p == "" {
			continue
		}
		if strings.HasPrefix(path, p) && len(p) > longestAllow {
			longestAllow = len(p)
		}
	}
	// Allow wins on tie or when strictly longer.
	return longestAllow >= longestDisallow
}

// parseRobots extracts the rules that apply to user-agent "*". It walks groups
// of "User-agent:" lines followed by directives; only the group(s) matching "*"
// contribute. Lines are trimmed of comments and whitespace; unknown directives
// are ignored.
func parseRobots(body string) robotsRules {
	var rules robotsRules
	// activeStar tracks whether the current group's user-agent set includes "*".
	activeStar := false
	// sawDirectiveInGroup marks that a new User-agent after directives starts a
	// fresh group.
	sawDirectiveInGroup := false

	for _, raw := range strings.Split(body, "\n") {
		line := raw
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, val, ok := splitDirective(line)
		if !ok {
			continue
		}
		switch key {
		case "user-agent":
			// A user-agent line after directives begins a fresh group.
			if sawDirectiveInGroup {
				activeStar = false
				sawDirectiveInGroup = false
			}
			if strings.TrimSpace(val) == "*" {
				activeStar = true
			}
		case "disallow":
			sawDirectiveInGroup = true
			if activeStar {
				rules.disallow = append(rules.disallow, val)
			}
		case "allow":
			sawDirectiveInGroup = true
			if activeStar {
				rules.allow = append(rules.allow, val)
			}
		default:
			// Sitemap, Crawl-delay, etc. are not directives that start/end a
			// group; ignore without touching group state.
		}
	}
	return rules
}

// splitDirective splits a robots.txt line into a lower-cased key and its value.
func splitDirective(line string) (key, val string, ok bool) {
	idx := strings.IndexByte(line, ':')
	if idx < 0 {
		return "", "", false
	}
	key = strings.ToLower(strings.TrimSpace(line[:idx]))
	val = strings.TrimSpace(line[idx+1:])
	return key, val, true
}

// rateLimiter is a simple token-bucket limiter: it refills at rate tokens per
// second up to burst tokens and blocks callers until a token is available. It is
// safe for concurrent use.
type rateLimiter struct {
	mu     sync.Mutex
	rate   float64 // tokens per second
	burst  float64 // maximum accumulated tokens
	tokens float64
	last   time.Time
}

// newRateLimiter builds a limiter allowing rate tokens/sec with the given burst,
// starting full (burst tokens available) so the first request is immediate.
func newRateLimiter(rate, burst float64) *rateLimiter {
	if burst < 1 {
		burst = 1
	}
	return &rateLimiter{
		rate:   rate,
		burst:  burst,
		tokens: burst,
		last:   time.Now(),
	}
}

// wait blocks until a token is available or ctx is cancelled. On admission it
// consumes one token. A non-positive rate degrades to no limiting.
func (rl *rateLimiter) wait(ctx context.Context) error {
	for {
		rl.mu.Lock()
		if rl.rate <= 0 {
			rl.mu.Unlock()
			return ctx.Err()
		}
		now := time.Now()
		elapsed := now.Sub(rl.last).Seconds()
		rl.last = now
		rl.tokens += elapsed * rl.rate
		if rl.tokens > rl.burst {
			rl.tokens = rl.burst
		}
		if rl.tokens >= 1 {
			rl.tokens--
			rl.mu.Unlock()
			return nil
		}
		// Time until the next token becomes available.
		deficit := 1 - rl.tokens
		wait := time.Duration(deficit / rl.rate * float64(time.Second))
		rl.mu.Unlock()

		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}
