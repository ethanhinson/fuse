package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The SDK half (loop.start) is deliberately NOT tested here: it needs a live
// authenticated server, which is exactly what the operator smoke provides and a
// unit test must not. These cover the two HTTP gates.

func TestWaitReadyRetriesUntilOK(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := waitReady(context.Background(), srv.Client(), srv.URL, 5, time.Millisecond); err != nil {
		t.Fatalf("waitReady: %v", err)
	}
	if calls != 3 {
		t.Fatalf("want 3 attempts, got %d", calls)
	}
}

func TestWaitReadyGivesUpAfterAttempts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	err := waitReady(context.Background(), srv.Client(), srv.URL, 2, time.Millisecond)
	if err == nil {
		t.Fatal("want an error when readiness never arrives")
	}
	if !strings.Contains(err.Error(), "never returned 200") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestScrapeFuseMetricsCountsFuseSamples(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("# HELP fuse_loops_total loops\n" +
			"fuse_loops_total 1\n" +
			"go_goroutines 7\n" +
			"fuse_events_total{tenant=\"_default\"} 4\n"))
	}))
	defer srv.Close()

	n, err := scrapeFuseMetrics(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("scrapeFuseMetrics: %v", err)
	}
	if n != 2 {
		t.Fatalf("want 2 fuse_ sample lines, got %d", n)
	}
}

// A 200 carrying no fuse_ series means the registry was never populated, which is
// a failure the smoke must not pass over.
func TestScrapeFuseMetricsRejectsEmptyRegistry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("go_goroutines 7\n# fuse_loops_total is only a comment\n"))
	}))
	defer srv.Close()

	if _, err := scrapeFuseMetrics(context.Background(), srv.Client(), srv.URL); err == nil {
		t.Fatal("want an error when no fuse_ sample lines are exposed")
	}
}

func TestScrapeFuseMetricsRejectsNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	if _, err := scrapeFuseMetrics(context.Background(), srv.Client(), srv.URL); err == nil {
		t.Fatal("want an error on a non-200 metrics scrape")
	}
}
