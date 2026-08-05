// Package research holds the web-search and page-fetch machinery behind the
// research mode: pluggable search providers, a bounded HTTP helper shared by
// them, and (in later tasks) the scraper and skill wiring.
package research

import "context"

// SearchResult is one hit returned by a SearchProvider. Fields are plain,
// untrusted text: the display path is responsible for sanitizing before render.
type SearchResult struct {
	Title   string
	URL     string
	Snippet string
}

// SearchProvider is a web-search backend (Brave, Tavily, a user-configured
// custom endpoint, ...). Implementations resolve their credentials/config at
// construction and translate the provider's wire shape into []SearchResult.
type SearchProvider interface {
	// Search runs a query and returns at most maxResults hits. A non-nil error
	// means the search could not be completed; callers surface it rather than
	// silently degrading.
	Search(ctx context.Context, query string, maxResults int) ([]SearchResult, error)

	// Name identifies the provider for diagnostics and trace labels.
	Name() string
}
