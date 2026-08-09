package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/research"
)

// webSearchDefaultMaxResults bounds an unspecified max_results so a model that
// omits the field still gets a compact list rather than a provider default that
// might flood the context.
const webSearchDefaultMaxResults = 5

// webSearchTool is the web_search built-in: it runs a query through the
// configured research search provider and returns a compact numbered list of
// hits. The provider is resolved lazily on first use and cached, so a missing
// API key surfaces as a loud error Result on the first call instead of failing
// at construction (when no credentials may yet be needed) or panicking.
type webSearchTool struct {
	// resolve produces (and validates) the search provider. It is invoked at
	// most once — its result (or error) is cached in provider/resolveErr.
	resolve func() (research.SearchProvider, error)

	// once guards the single provider resolution. Execute is called concurrently
	// on a shared tool instance (multiple agents run in parallel), so the
	// resolve-and-cache must be atomic; a plain boolean check-and-set here is a
	// data race on provider/resolveErr/resolved.
	once       sync.Once
	provider   research.SearchProvider
	resolveErr error
}

// NewWebSearch returns the web_search tool bound to cfg. The provider is
// resolved lazily via research.Resolve(cfg, os.Getenv) on the first Execute, so
// a misconfiguration is reported as an error Result naming the setup rather than
// preventing the tool from registering.
func NewWebSearch(cfg config.ResearchConfig) Tool {
	return &webSearchTool{
		resolve: func() (research.SearchProvider, error) {
			return research.Resolve(cfg, os.Getenv)
		},
	}
}

// newWebSearchWithResolver is the test seam: it injects a resolver directly so
// tests can supply a fake provider (or a resolver that errors) without a network
// or real credentials.
func newWebSearchWithResolver(resolve func() (research.SearchProvider, error)) *webSearchTool {
	return &webSearchTool{resolve: resolve}
}

func (*webSearchTool) Name() string { return "web_search" }

func (*webSearchTool) Description() string {
	return "Search the web via the configured research provider. " +
		"Returns a compact numbered list of results (title, URL, snippet). " +
		"Use web_fetch to read a result's full page."
}

func (*webSearchTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query":       map[string]any{"type": "string", "description": "Search query"},
			"max_results": map[string]any{"type": "integer", "description": "Maximum results to return (default 5)"},
		},
		"required": []string{"query"},
	}
}

type webSearchArgs struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results"`
}

// providerOnce resolves the provider exactly once, caching the result and any
// error so a misconfiguration is reported consistently on every call. Safe for
// concurrent use: sync.Once serialises the resolution across parallel Execute
// calls on the shared tool instance.
func (t *webSearchTool) providerOnce() (research.SearchProvider, error) {
	t.once.Do(func() {
		t.provider, t.resolveErr = t.resolve()
	})
	return t.provider, t.resolveErr
}

func (t *webSearchTool) Execute(ctx context.Context, args string) Result {
	var a webSearchArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("bad arguments: %v", err)}
	}
	if a.Query == "" {
		return Result{IsError: true, Output: "query is required"}
	}
	maxResults := a.MaxResults
	if maxResults <= 0 {
		maxResults = webSearchDefaultMaxResults
	}

	provider, err := t.providerOnce()
	if err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("web_search: provider setup failed: %v", err)}
	}

	results, err := provider.Search(ctx, a.Query, maxResults)
	if err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("web_search: search failed: %v", err)}
	}
	if len(results) == 0 {
		return Result{Output: fmt.Sprintf("No results for %q.", a.Query)}
	}

	var b strings.Builder
	for i, r := range results {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, r.Title, r.URL)
		if r.Snippet != "" {
			fmt.Fprintf(&b, "   %s\n", r.Snippet)
		}
	}
	return Result{Output: strings.TrimRight(b.String(), "\n")}
}
