package main

import (
	"bufio"
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/mcpdemo/rentals"
)

// envMap turns a fixed map into the env lookup func run takes. A key that is absent
// reads as "" — exactly like os.Getenv — so a test never inherits the real process
// environment (and can therefore never pick up a real TAVILY_API_KEY).
func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// startRun boots the entrypoint's wiring on an ephemeral loopback port and returns the
// address it actually bound. Teardown is registered with t.Cleanup ONLY: the /sse
// handler blocks on the request context, so a `defer` shutdown ahead of the client's
// body close deadlocks (learning httptest-defer-close-before-tcleanup-deadlock).
func startRun(t *testing.T, args []string, env map[string]string) (addr string, errCh <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	var logs bytes.Buffer

	go func() {
		done <- runContext(ctx, runOpts{
			args:          args,
			env:           envMap(env),
			stderr:        &logs,
			ready:         func(a string) { ready <- a },
			shutdownGrace: 2 * time.Second,
		})
	}()

	// Registered FIRST so LIFO runs it LAST — after the caller's response-body close.
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("runContext returned %v (logs: %s)", err, logs.String())
			}
		case <-time.After(15 * time.Second):
			t.Errorf("runContext did not shut down (logs: %s)", logs.String())
		}
	})

	select {
	case a := <-ready:
		return a, done
	case err := <-done:
		t.Fatalf("runContext exited before listening: %v (logs: %s)", err, logs.String())
	case <-time.After(10 * time.Second):
		t.Fatalf("runContext never signalled ready (logs: %s)", logs.String())
	}
	return "", nil
}

// TestRunServesSSEEndpointEvent is the entrypoint's end-to-end wiring proof: run binds a
// real listener, serves the rentals handler, emits the MCP `endpoint` SSE event, and then
// shuts down cleanly when its context is cancelled. No TAVILY_API_KEY, no network.
func TestRunServesSSEEndpointEvent(t *testing.T) {
	addr, _ := startRun(t, []string{
		"--addr", "127.0.0.1:0",
		"--audience", "https://rentals.example",
		"--signing-key", "test-signing-key-0123456789",
		"--tenants", "acme,globex",
	}, map[string]string{})

	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/sse", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /sse: %v", err)
	}
	// Registered SECOND so LIFO closes the SSE body BEFORE the server shuts down.
	t.Cleanup(func() { _ = resp.Body.Close() })

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /sse status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	br := bufio.NewReader(resp.Body)
	var event, data string
	for i := 0; i < 4 && data == ""; i++ {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE line: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		}
	}
	if event != "endpoint" {
		t.Fatalf("SSE event = %q, want %q", event, "endpoint")
	}
	if want := "http://" + addr + "/messages"; data != want {
		t.Fatalf("endpoint data = %q, want %q", data, want)
	}
}

// TestRunRejectsBadConfig covers the fail-loud contract: a missing audience, an
// unparseable tenant key, and no tenant keys at all must each return a clear error
// instead of serving an unusable server.
func TestRunRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "missing audience",
			args: []string{"--signing-key", "k", "--tenants", "acme"},
			want: "--audience",
		},
		{
			name: "unparseable tenant key",
			args: []string{"--audience", "https://r", "--tenant-key", "acme=zzzz-not-hex"},
			want: "--tenant-key",
		},
		{
			name: "tenant key missing separator",
			args: []string{"--audience", "https://r", "--tenant-key", "acme"},
			want: "--tenant-key",
		},
		{
			name: "no tenant keys",
			args: []string{"--audience", "https://r"},
			want: "tenant",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			err := runContext(context.Background(), runOpts{
				args:   tc.args,
				env:    envMap(map[string]string{}),
				stderr: &logs,
				ready:  func(string) { t.Errorf("server should never have started") },
			})
			if err == nil {
				t.Fatalf("runContext = nil, want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err.Error(), tc.want)
			}
		})
	}
}

// TestResolveDataSourceDefaultsToCanned pins the hard invariant: with no search
// credential in the environment, the entrypoint serves CannedData — never the live
// backend, never an error. Auto mode degrades; explicit --data live fails loud.
func TestResolveDataSourceDefaultsToCanned(t *testing.T) {
	src, err := resolveDataSource(context.Background(), "auto", 5, envMap(map[string]string{}), nil)
	if err != nil {
		t.Fatalf("auto with no key = %v, want canned and no error", err)
	}
	if _, ok := src.(rentals.CannedData); !ok {
		t.Fatalf("auto with no key = %T, want rentals.CannedData", src)
	}
	if got := len(src.Search("")); got == 0 {
		t.Fatalf("canned source returned %d listings, want the fixed set", got)
	}
	if _, err := resolveDataSource(context.Background(), "live", 5, envMap(map[string]string{}), nil); err == nil {
		t.Fatal("--data live with no credential = nil error, want a loud failure")
	}
	if _, err := resolveDataSource(context.Background(), "bogus", 5, envMap(map[string]string{}), nil); err == nil {
		t.Fatal("--data bogus = nil error, want a loud failure")
	}
}
