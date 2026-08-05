package tools

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/research"
)

// fakeSearchProvider is a mutex-guarded SearchProvider double. Every field is
// touched under the same lock on both the write and read paths so it is safe
// under -race even if a tool ever calls it concurrently.
type fakeSearchProvider struct {
	mu sync.Mutex

	results   []research.SearchResult
	err       error
	calls     int
	lastQuery string
	lastMax   int
}

func (f *fakeSearchProvider) Search(_ context.Context, query string, maxResults int) ([]research.SearchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastQuery = query
	f.lastMax = maxResults
	return f.results, f.err
}

func (f *fakeSearchProvider) Name() string { return "fake" }

func (f *fakeSearchProvider) snapshot() (calls int, query string, max int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.lastQuery, f.lastMax
}

func TestWebSearchNameAndParameters(t *testing.T) {
	tool := newWebSearchWithResolver(func() (research.SearchProvider, error) {
		return &fakeSearchProvider{}, nil
	})
	if tool.Name() != "web_search" {
		t.Fatalf("Name() = %q, want web_search", tool.Name())
	}
	params := tool.Parameters()
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties missing or wrong type: %#v", params["properties"])
	}
	if _, ok := props["query"]; !ok {
		t.Errorf("parameters missing query property")
	}
	if _, ok := props["max_results"]; !ok {
		t.Errorf("parameters missing max_results property")
	}
	req, ok := params["required"].([]string)
	if !ok || len(req) != 1 || req[0] != "query" {
		t.Errorf("required = %#v, want [query]", params["required"])
	}
}

func TestWebSearchHappyPathFormatsResults(t *testing.T) {
	fake := &fakeSearchProvider{
		results: []research.SearchResult{
			{Title: "First Title", URL: "https://example.com/a", Snippet: "snippet a"},
			{Title: "Second Title", URL: "https://example.com/b", Snippet: "snippet b"},
		},
	}
	tool := newWebSearchWithResolver(func() (research.SearchProvider, error) {
		return fake, nil
	})

	res := tool.Execute(context.Background(), `{"query":"golang testing","max_results":2}`)
	if res.IsError {
		t.Fatalf("unexpected error result: %s", res.Output)
	}
	for _, want := range []string{
		"First Title", "https://example.com/a", "snippet a",
		"Second Title", "https://example.com/b", "snippet b",
	} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("output missing %q\n---\n%s", want, res.Output)
		}
	}
	calls, query, max := fake.snapshot()
	if calls != 1 {
		t.Errorf("provider called %d times, want 1", calls)
	}
	if query != "golang testing" {
		t.Errorf("query = %q, want golang testing", query)
	}
	if max != 2 {
		t.Errorf("max_results forwarded = %d, want 2", max)
	}
}

func TestWebSearchEmptyResults(t *testing.T) {
	tool := newWebSearchWithResolver(func() (research.SearchProvider, error) {
		return &fakeSearchProvider{results: nil}, nil
	})
	res := tool.Execute(context.Background(), `{"query":"nothing here"}`)
	if res.IsError {
		t.Fatalf("empty results should not be an error: %s", res.Output)
	}
	if !strings.Contains(strings.ToLower(res.Output), "no results") {
		t.Errorf("expected a no-results message, got %q", res.Output)
	}
}

func TestWebSearchBadArgs(t *testing.T) {
	tool := newWebSearchWithResolver(func() (research.SearchProvider, error) {
		return &fakeSearchProvider{}, nil
	})
	res := tool.Execute(context.Background(), `{"query":`)
	if !res.IsError {
		t.Fatalf("bad JSON should be an error result, got %q", res.Output)
	}
}

func TestWebSearchProviderResolutionError(t *testing.T) {
	tool := newWebSearchWithResolver(func() (research.SearchProvider, error) {
		return nil, errors.New("no search provider configured")
	})
	res := tool.Execute(context.Background(), `{"query":"anything"}`)
	if !res.IsError {
		t.Fatalf("resolution error should be an error result, got %q", res.Output)
	}
	if !strings.Contains(strings.ToLower(res.Output), "setup") {
		t.Errorf("error should mention setup, got %q", res.Output)
	}
}
