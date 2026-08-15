package rentals

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/research"
)

// degradedSource is a DataSource whose backend is DOWN: every search yields no
// listings AND reports the outage through the optional degraded seam.
type degradedSource struct{}

func (degradedSource) Search(string) []Listing { return nil }

func (degradedSource) SearchStatus(string) ([]Listing, error) {
	return nil, errors.New("provider unreachable")
}

// TestSearchRentalsReportsBackendOutage: when the listing backend is degraded, the
// model must be TOLD the search is unavailable rather than shown an empty market —
// "no rentals found" during a provider outage is a confident lie to the user.
func TestSearchRentalsReportsBackendOutage(t *testing.T) {
	keys := map[event.TenantID][]byte{"acme": []byte("k-acme")}
	reg, _, ctxFor := harnessWithConfig(t, Config{Audience: testAudience, TenantKeys: keys, Data: degradedSource{}})

	ctx, cancel := context.WithTimeout(ctxFor("alice", "acme"), 5*time.Second)
	defer cancel()
	res := reg.Execute(ctx, "mcp:rentals/search_rentals", `{"query":"tahoe"}`)
	if !res.IsError {
		t.Fatalf("a degraded backend must surface as a tool error, got a normal result %q", res.Output)
	}
	if !strings.Contains(strings.ToLower(res.Output), "unavailable") {
		t.Fatalf("expected an availability message the model can relay, got %q", res.Output)
	}
}

// TestCannedNoMatchStaysAnOrdinaryEmptyResult pins the other half of the
// distinction: CannedData is the nil-Config default and can NEVER be degraded, so a
// genuine no-match still renders as a plain empty list, not an outage.
func TestCannedNoMatchStaysAnOrdinaryEmptyResult(t *testing.T) {
	keys := map[event.TenantID][]byte{"acme": []byte("k-acme")}
	reg, _, ctxFor := harness(t, keys)

	ctx, cancel := context.WithTimeout(ctxFor("alice", "acme"), 5*time.Second)
	defer cancel()
	res := reg.Execute(ctx, "mcp:rentals/search_rentals", `{"query":"no-such-city-anywhere"}`)
	if res.IsError {
		t.Fatalf("a genuine canned no-match must NOT look like an outage: %s", res.Output)
	}
	if got := strings.TrimSpace(res.Output); got != "[]" && got != "null" {
		t.Fatalf("expected an ordinary empty listing result, got %q", got)
	}
}

// TestLiveDataSearchStatusSurfacesProviderError proves LiveData reports the outage
// through the status seam while its DataSource.Search stays absorb-and-degrade.
func TestLiveDataSearchStatusSurfacesProviderError(t *testing.T) {
	stub := &stubSearcher{err: errors.New("boom")}
	d := NewLiveData(LiveDataConfig{Searcher: stub})

	if got := d.Search("tahoe"); len(got) != 0 {
		t.Fatalf("Search must still degrade to empty, got %v", got)
	}
	listings, err := d.SearchStatus("tahoe")
	if err == nil {
		t.Fatal("SearchStatus must report the provider failure")
	}
	if len(listings) != 0 {
		t.Fatalf("a failed search returns no listings, got %v", listings)
	}
}

// TestLiveDataSearchStatusHealthyReportsNoError: a healthy live search is NOT
// degraded — including when it legitimately matches nothing.
func TestLiveDataSearchStatusHealthyReportsNoError(t *testing.T) {
	stub := &stubSearcher{results: []research.SearchResult{}}
	d := NewLiveData(LiveDataConfig{Searcher: stub})

	listings, err := d.SearchStatus("tahoe")
	if err != nil {
		t.Fatalf("a healthy empty search must not report an outage: %v", err)
	}
	if len(listings) != 0 {
		t.Fatalf("expected no listings, got %v", listings)
	}
}
