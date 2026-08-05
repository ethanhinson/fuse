package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/research"
)

// webFetchTool is the web_fetch built-in: it retrieves a URL and returns the
// page's main-body text (or a human-readable note for robots refusals and
// non-HTML content) via the shared research.Scraper.
type webFetchTool struct {
	scraper *research.Scraper
}

// NewWebFetch returns the web_fetch tool wired to a research.Scraper built from
// cfg.
func NewWebFetch(cfg config.ResearchConfig) Tool {
	return &webFetchTool{scraper: research.NewScraper(cfg)}
}

// newWebFetchWithScraper is the test seam: it injects a scraper directly so
// tests can point it at an httptest server.
func newWebFetchWithScraper(s *research.Scraper) *webFetchTool {
	return &webFetchTool{scraper: s}
}

func (*webFetchTool) Name() string { return "web_fetch" }

func (*webFetchTool) Description() string {
	return "Fetch a web page and return its main-body text. " +
		"Robots.txt refusals and non-HTML content are returned as short notes."
}

func (*webFetchTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{"type": "string", "description": "The http(s) URL to fetch"},
		},
		"required": []string{"url"},
	}
}

type webFetchArgs struct {
	URL string `json:"url"`
}

func (t *webFetchTool) Execute(ctx context.Context, args string) Result {
	var a webFetchArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("bad arguments: %v", err)}
	}
	if a.URL == "" {
		return Result{IsError: true, Output: "url is required"}
	}
	text, err := t.scraper.Fetch(ctx, a.URL)
	if err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("web_fetch: %v", err)}
	}
	return Result{Output: text}
}
