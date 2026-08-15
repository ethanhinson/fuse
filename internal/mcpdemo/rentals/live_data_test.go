package rentals

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/research"
)

// Compile-time only (constructs nothing, calls nothing, reaches no network): the
// real provider must keep satisfying the narrow local seam, so task 5's wiring can
// pass a *research.TavilyProvider straight into LiveDataConfig.Searcher.
var _ ResultSearcher = (*research.TavilyProvider)(nil)

// stubSearcher is the injected seam that keeps every test in this file OFF the
// network: no TAVILY_API_KEY, no HTTP, no real provider.
type stubSearcher struct {
	results []research.SearchResult
	err     error
	block   bool

	gotQuery string
	gotMax   int
	calls    int
}

func (s *stubSearcher) Search(ctx context.Context, query string, maxResults int) ([]research.SearchResult, error) {
	s.calls++
	s.gotQuery = query
	s.gotMax = maxResults
	if s.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.results, nil
}

func TestLiveDataMapsResultsAndDerivesStableIDs(t *testing.T) {
	stub := &stubSearcher{results: []research.SearchResult{
		{Title: "Beach Cottage in Santa Cruz", URL: "https://example.test/a", Snippet: "Cozy place, $220 per night"},
		{Title: "Mountain Cabin in Tahoe", URL: "https://example.test/b", Snippet: "Sleeps 6"},
	}}
	src := NewLiveData(LiveDataConfig{Searcher: stub, MaxResults: 5})

	got := src.Search("cabins")
	if len(got) != 2 {
		t.Fatalf("want 2 listings, got %d (%+v)", len(got), got)
	}
	if stub.gotQuery != "cabins" {
		t.Errorf("query not forwarded: %q", stub.gotQuery)
	}
	if got[0].Title != "Beach Cottage in Santa Cruz" {
		t.Errorf("title not mapped: %q", got[0].Title)
	}
	if got[0].City != "Santa Cruz" {
		t.Errorf("city best-effort extraction: want %q, got %q", "Santa Cruz", got[0].City)
	}
	if got[0].Price != 220 {
		t.Errorf("price best-effort extraction: want 220, got %d", got[0].Price)
	}
	if got[0].ID == "" || got[1].ID == "" {
		t.Fatalf("listings must carry a derived ID: %+v", got)
	}
	if got[0].ID == got[1].ID {
		t.Errorf("distinct URLs must derive distinct IDs, both %q", got[0].ID)
	}

	// Stability: the SAME url derives the SAME id on a later call, so a
	// favorite_listing on a search result stays meaningful across searches.
	again := src.Search("cabins again")
	if again[0].ID != got[0].ID {
		t.Errorf("ID not stable across calls: %q then %q", got[0].ID, again[0].ID)
	}
}

func TestLiveDataRespectsResultCap(t *testing.T) {
	stub := &stubSearcher{results: []research.SearchResult{
		{Title: "One", URL: "https://example.test/1"},
		{Title: "Two", URL: "https://example.test/2"},
		{Title: "Three", URL: "https://example.test/3"},
	}}
	src := NewLiveData(LiveDataConfig{Searcher: stub, MaxResults: 2})

	got := src.Search("anything")
	if stub.gotMax != 2 {
		t.Errorf("cap not passed to provider: got %d", stub.gotMax)
	}
	// A provider that over-returns must still be trimmed at the seam.
	if len(got) != 2 {
		t.Fatalf("cap not enforced on the response: got %d", len(got))
	}
}

func TestLiveDataAbsorbsProviderError(t *testing.T) {
	stub := &stubSearcher{err: errors.New("boom")}
	src := NewLiveData(LiveDataConfig{Searcher: stub, MaxResults: 3})

	got := src.Search("q")
	if got == nil {
		t.Fatalf("want empty non-nil slice on error, got nil")
	}
	if len(got) != 0 {
		t.Fatalf("want empty slice on error, got %+v", got)
	}
}

func TestLiveDataTimesOutToEmpty(t *testing.T) {
	stub := &stubSearcher{block: true}
	src := NewLiveData(LiveDataConfig{Searcher: stub, MaxResults: 3, Timeout: 20 * time.Millisecond})

	done := make(chan []Listing, 1)
	go func() { done <- src.Search("q") }()
	select {
	case got := <-done:
		if len(got) != 0 {
			t.Fatalf("want empty slice on timeout, got %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Search did not honour its per-search timeout")
	}
}

func TestLiveDataHonoursCancelledBaseContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stub := &stubSearcher{block: true}
	src := NewLiveData(LiveDataConfig{Searcher: stub, Ctx: ctx})

	if got := src.Search("q"); len(got) != 0 {
		t.Fatalf("want empty slice on cancelled context, got %+v", got)
	}
}

func TestLiveDataNilSearcherIsInert(t *testing.T) {
	src := NewLiveData(LiveDataConfig{})
	if got := src.Search("q"); len(got) != 0 {
		t.Fatalf("nil searcher must degrade to empty, got %+v", got)
	}
}

// CannedData stays the default: a nil Config.Data must never resolve to the live
// source (invariant 1 — the acceptance lane never reaches the network).
func TestCannedDataRemainsDefaultDataSource(t *testing.T) {
	s := newServer(Config{Audience: "https://rentals.test"})
	if _, ok := s.data.(CannedData); !ok {
		t.Fatalf("default DataSource must be CannedData, got %T", s.data)
	}
}
