package rentals

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ethanhinson/fuse/internal/research"
)

// Defaults for the live source. They are demo tuning, not contract.
const (
	defaultLiveMaxResults = 5
	defaultLiveTimeout    = 10 * time.Second
)

// ResultSearcher is the NARROW local view of a web-search provider that LiveData
// needs — deliberately not research.SearchProvider and not *research.TavilyProvider.
// The concrete provider satisfies it structurally, so production wiring passes a real
// *research.TavilyProvider while tests inject a stub and never touch the network.
type ResultSearcher interface {
	Search(ctx context.Context, query string, maxResults int) ([]research.SearchResult, error)
}

// LiveDataConfig configures the live, search-backed DataSource.
type LiveDataConfig struct {
	// Searcher is the provider seam. A nil Searcher makes the source inert
	// (every Search returns empty) rather than a nil-panic on the tool path.
	Searcher ResultSearcher
	// Ctx is the base context the source's lifetime hangs off (typically the
	// server's). nil ⇒ context.Background(). Cancelling it makes every
	// subsequent Search return empty.
	Ctx context.Context
	// MaxResults caps both what is requested from the provider and what is
	// returned. ≤0 ⇒ defaultLiveMaxResults.
	MaxResults int
	// Timeout bounds a single search. ≤0 ⇒ defaultLiveTimeout.
	Timeout time.Duration
	// Logger records absorbed failures. nil ⇒ slog.Default().
	Logger *slog.Logger
}

// LiveData is the live, web-search-backed DataSource used by the runnable demo. It
// adapts a context+error provider call onto DataSource's one-method, error-free
// contract: Search NEVER panics and NEVER blocks past its timeout — a provider
// error, a timeout, or a cancelled base context all degrade to an EMPTY slice, with
// the failure logged rather than surfaced (the MCP tool has no error channel here).
//
// CannedData remains the default DataSource; LiveData is opt-in via Config.Data.
//
// Demo-grade mapping: a web search result carries a title, a URL and a snippet — not
// a structured rental. City and price are BEST-EFFORT text extraction and are often
// empty or wrong. They are good enough to browse a demo and are not a data contract.
type LiveData struct {
	searcher ResultSearcher
	ctx      context.Context
	max      int
	timeout  time.Duration
	log      *slog.Logger
}

// NewLiveData builds the live DataSource. It never returns an error: a
// misconfigured source degrades to empty results rather than failing the server.
func NewLiveData(cfg LiveDataConfig) *LiveData {
	d := &LiveData{
		searcher: cfg.Searcher,
		ctx:      cfg.Ctx,
		max:      cfg.MaxResults,
		timeout:  cfg.Timeout,
		log:      cfg.Logger,
	}
	if d.ctx == nil {
		d.ctx = context.Background()
	}
	if d.max <= 0 {
		d.max = defaultLiveMaxResults
	}
	if d.timeout <= 0 {
		d.timeout = defaultLiveTimeout
	}
	if d.log == nil {
		d.log = slog.Default()
	}
	return d
}

// Search implements DataSource. See the type doc for the absorb-and-degrade contract.
func (d *LiveData) Search(query string) []Listing {
	out := make([]Listing, 0, d.max)
	if d.searcher == nil {
		d.log.Warn("rentals: live data source has no searcher configured; returning no listings")
		return out
	}

	ctx, cancel := context.WithTimeout(d.ctx, d.timeout)
	defer cancel()

	results, err := d.searcher.Search(ctx, query, d.max)
	if err != nil {
		d.log.Warn("rentals: live search failed; degrading to no listings",
			"query", query, "error", err)
		return out
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		d.log.Warn("rentals: live search context ended; degrading to no listings",
			"query", query, "error", ctxErr)
		return out
	}

	for _, r := range results {
		if len(out) >= d.max {
			break // trim: a provider may over-return its own cap
		}
		out = append(out, Listing{
			ID:    listingIDForURL(r.URL),
			Title: strings.TrimSpace(r.Title),
			City:  extractCity(r.Title),
			Price: extractPrice(r.Title + " " + r.Snippet),
		})
	}
	return out
}

// listingIDForURL derives a DETERMINISTIC listing ID from the result URL, so
// favoriting a search result yields the same ID on every later search — favorites
// stay meaningful across calls and restarts. Results with no URL fall back to a
// hash of the empty string, which is stable but collides; that is acceptable for
// demo data where a URL-less result is already degenerate.
func listingIDForURL(u string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(u)))
	return "live-" + hex.EncodeToString(sum[:])[:16]
}

// priceRe matches a plain dollar amount, optionally with thousands separators.
var priceRe = regexp.MustCompile(`\$\s?([0-9][0-9,]*)`)

// extractPrice is BEST-EFFORT demo-grade: it takes the first dollar amount in the
// text and calls it the nightly price. 0 means "unknown", not "free".
func extractPrice(text string) int {
	m := priceRe.FindStringSubmatch(text)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(strings.ReplaceAll(m[1], ",", ""))
	if err != nil {
		return 0
	}
	return n
}

// extractCity is BEST-EFFORT demo-grade: it reads the segment after a trailing
// " in " (e.g. "Beach Cottage in Santa Cruz"), else after the last comma. Empty
// means "unknown".
func extractCity(title string) string {
	t := strings.TrimSpace(title)
	if i := strings.LastIndex(strings.ToLower(t), " in "); i >= 0 {
		return trimCity(t[i+len(" in "):])
	}
	if i := strings.LastIndex(t, ","); i >= 0 {
		return trimCity(t[i+1:])
	}
	return ""
}

func trimCity(s string) string {
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(s), ".,;:-|"))
}
