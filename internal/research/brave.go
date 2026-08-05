package research

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// braveDefaultBaseURL is Brave's web-search endpoint. We use the standard web
// search API and pair it with our own readability extraction downstream, rather
// than Brave's "LLM Context" endpoint: the web endpoint returns titles, URLs,
// and descriptions that suffice for snippet-level results, and extraction of
// full page content is handled by the scraper. The LLM Context endpoint was
// evaluated and deferred per the spec open question (no extra dependency and a
// simpler contract for now; revisit if snippet quality proves insufficient).
const braveDefaultBaseURL = "https://api.search.brave.com/res/v1/web/search"

// BraveSearchProvider is a SearchProvider backed by the Brave Search API. It
// returns snippet-only results (title, URL, and Brave's description); page
// content is fetched separately by the scraper.
type BraveSearchProvider struct {
	apiKey  string
	baseURL string
	client  *Client
}

// NewBraveProvider builds a BraveSearchProvider for the given subscription key,
// wired to Brave's production web-search endpoint and the shared bounded HTTP
// Client. Tests override baseURL and inject an httptest-backed client directly.
func NewBraveProvider(apiKey string) *BraveSearchProvider {
	return &BraveSearchProvider{
		apiKey:  apiKey,
		baseURL: braveDefaultBaseURL,
		client:  NewClient("brave", nil),
	}
}

// braveResponse mirrors the subset of Brave's web-search JSON we consume:
// {"web":{"results":[{"title":..,"url":..,"description":..}]}}.
type braveWireResponse struct {
	Web struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
		} `json:"results"`
	} `json:"web"`
}

// Search runs a Brave web search for query and returns at most maxResults hits.
// A missing web/results block yields an empty slice and a nil error.
func (p *BraveSearchProvider) Search(ctx context.Context, query string, maxResults int) ([]SearchResult, error) {
	q := url.Values{}
	q.Set("q", query)
	q.Set("count", strconv.Itoa(maxResults))
	reqURL := p.baseURL + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("brave: build request: %w", err)
	}
	req.Header.Set("X-Subscription-Token", p.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := p.client.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Drain a bounded prefix so the connection can be reused, then surface
		// the status to the caller.
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("brave: search request failed with status %d", resp.StatusCode)
	}

	var wire braveWireResponse
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return nil, fmt.Errorf("brave: decode response: %w", err)
	}

	results := make([]SearchResult, 0, len(wire.Web.Results))
	for _, r := range wire.Web.Results {
		results = append(results, SearchResult{
			Title:   r.Title,
			URL:     r.URL,
			Snippet: r.Description,
		})
	}
	return results, nil
}

// Name identifies this provider in diagnostics and trace labels.
func (p *BraveSearchProvider) Name() string { return "brave" }
