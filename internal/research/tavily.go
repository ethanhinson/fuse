package research

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// defaultTavilyURL is Tavily's search endpoint. It is overridable per-provider
// so tests can point at an httptest server.
const defaultTavilyURL = "https://api.tavily.com/search"

// TavilyProvider is a SearchProvider backed by the Tavily search API. Tavily
// returns extracted page content alongside each hit, which we seed into
// SearchResult.Snippet so the research skill can often skip a follow-up fetch.
type TavilyProvider struct {
	apiKey  string
	baseURL string
	client  *Client
}

// NewTavilyProvider builds a TavilyProvider using the shared bounded Client.
// The baseURL and client fields are exported to the package for test injection.
func NewTavilyProvider(apiKey string) *TavilyProvider {
	return &TavilyProvider{
		apiKey:  apiKey,
		baseURL: defaultTavilyURL,
		client:  NewClient("tavily", nil),
	}
}

// Name identifies the provider for diagnostics and trace labels.
func (p *TavilyProvider) Name() string { return "tavily" }

// tavilyRequest is the JSON body posted to Tavily's search endpoint.
type tavilyRequest struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results"`
}

// tavilyResponse is the subset of Tavily's response we consume. Tavily's
// per-hit "content" is the extracted page text, mapped to SearchResult.Snippet.
type tavilyResponse struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
}

// Search runs a Tavily query and returns at most maxResults hits. The POST body
// is built from a *bytes.Reader so http.NewRequestWithContext sets GetBody,
// which makes the Client's per-attempt retries body-safe.
func (p *TavilyProvider) Search(ctx context.Context, query string, maxResults int) ([]SearchResult, error) {
	payload, err := json.Marshal(tavilyRequest{Query: query, MaxResults: maxResults})
	if err != nil {
		return nil, fmt.Errorf("tavily: marshal request: %w", err)
	}

	// bytes.NewReader makes http.NewRequestWithContext set req.GetBody, so the
	// Client can rewind the body on a retried attempt.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("tavily: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("tavily: search: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("tavily: unexpected status %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}

	var parsed tavilyResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("tavily: decode response: %w", err)
	}

	results := make([]SearchResult, 0, len(parsed.Results))
	for _, r := range parsed.Results {
		results = append(results, SearchResult{
			Title:   r.Title,
			URL:     r.URL,
			Snippet: r.Content,
		})
	}
	return results, nil
}
