package research

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/ethanhinson/fuse/internal/config"
)

// CustomHTTPProvider is a SearchProvider backed by any user-configured JSON
// search endpoint. It is the research mode's user-land extension point: rather
// than shipping an adapter per engine, the operator points cfg.URL at a JSON
// endpoint and describes its wire shape with a dotted results path plus per-
// field mappings. The defaults match a SearXNG `/search?format=json` response
// (results/title/url/content), so a stock SearXNG needs only a URL.
type CustomHTTPProvider struct {
	cfg    config.CustomProviderConfig
	client *Client
}

// NewCustomProvider builds a CustomHTTPProvider from cfg, wired to the shared
// bounded HTTP Client. Tests inject an httptest-backed client by overriding the
// unexported client field directly (mirroring the other providers).
func NewCustomProvider(cfg config.CustomProviderConfig) *CustomHTTPProvider {
	return &CustomHTTPProvider{
		cfg:    cfg,
		client: NewClient("custom", nil),
	}
}

// SearXNG-shaped defaults applied when the corresponding config field is empty.
const (
	customDefaultResultsPath  = "results"
	customDefaultTitleField   = "title"
	customDefaultURLField     = "url"
	customDefaultSnippetField = "content"
)

// Search runs a query against the configured endpoint and returns at most
// maxResults hits. The request URL is derived from cfg.URL by substituting
// {query} (URL-query-escaped) and {count} (maxResults). Only GET is issued.
// A missing results path yields an empty slice and a nil error; malformed
// entries (non-objects, or objects without a URL) are skipped.
func (p *CustomHTTPProvider) Search(ctx context.Context, query string, maxResults int) ([]SearchResult, error) {
	if !p.IsConfigured() {
		return nil, fmt.Errorf("custom: no search URL configured")
	}

	reqURL := strings.ReplaceAll(p.cfg.URL, "{query}", url.QueryEscape(query))
	reqURL = strings.ReplaceAll(reqURL, "{count}", strconv.Itoa(maxResults))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("custom: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range p.cfg.Headers {
		req.Header.Set(k, v)
	}

	resp, err := p.client.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Drain a bounded prefix so the connection can be reused, then surface
		// the status to the caller.
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("custom: search request failed with status %d", resp.StatusCode)
	}

	var doc any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("custom: decode response: %w", err)
	}

	resultsPath := firstNonEmpty(p.cfg.ResultsPath, customDefaultResultsPath)
	titleField := firstNonEmpty(p.cfg.TitleField, customDefaultTitleField)
	urlField := firstNonEmpty(p.cfg.URLField, customDefaultURLField)
	snippetField := firstNonEmpty(p.cfg.SnippetField, customDefaultSnippetField)

	raw, ok := navigatePath(doc, resultsPath).([]any)
	if !ok {
		// Missing results path (or a non-array at it) is not an error: the
		// endpoint simply returned no usable results block.
		return []SearchResult{}, nil
	}

	results := make([]SearchResult, 0, len(raw))
	for _, entry := range raw {
		obj, ok := entry.(map[string]any)
		if !ok {
			continue // non-object entry, skip gracefully
		}
		u := stringField(obj, urlField)
		if u == "" {
			continue // a result needs at least a URL
		}
		results = append(results, SearchResult{
			Title:   stringField(obj, titleField),
			URL:     u,
			Snippet: stringField(obj, snippetField),
		})
	}
	return results, nil
}

// Name identifies this provider in diagnostics and trace labels.
func (p *CustomHTTPProvider) Name() string { return "custom" }

// IsConfigured reports whether a search URL is set. Task 5's provider
// resolution uses this to decide whether the custom provider is auto-selectable.
func (p *CustomHTTPProvider) IsConfigured() bool { return p.cfg.URL != "" }

// navigatePath walks a dotted path (e.g. "data.items") into a decoded JSON
// value, descending through nested objects. It returns nil if any segment is
// absent or the value at that segment is not an object. An empty path returns
// the root unchanged.
func navigatePath(root any, path string) any {
	if path == "" {
		return root
	}
	cur := root
	for _, seg := range strings.Split(path, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		next, ok := obj[seg]
		if !ok {
			return nil
		}
		cur = next
	}
	return cur
}

// stringField returns obj[key] as a string, or "" if it is absent or not a
// string. JSON strings decode to string; anything else (number, bool, object)
// is treated as unset for our text-only result fields.
func stringField(obj map[string]any, key string) string {
	if v, ok := obj[key].(string); ok {
		return v
	}
	return ""
}

// firstNonEmpty returns v if non-empty, else def.
func firstNonEmpty(v, def string) string {
	if v != "" {
		return v
	}
	return def
}
